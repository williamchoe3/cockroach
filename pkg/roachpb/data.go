// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package roachpb

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/cockroachdb/apd/v3"
	"github.com/cockroachdb/cockroach/pkg/geo/geopb"
	"github.com/cockroachdb/cockroach/pkg/keysbase"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/isolation"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/bitarray"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/duration"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/interval"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timetz"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

const (
	localPrefixByte = '\x01'
	// LocalMaxByte is the end of the local key range.
	LocalMaxByte = '\x02'
	// PrevishKeyLength is a reasonable key length to use for Key.Prevish(),
	// typically when peeking to the left of a known key. We want this to be as
	// tight as possible, since it can e.g. be used for latch spans. However, the
	// exact previous key has infinite length, so we assume that most keys are
	// less than 1024 bytes, or have a fairly unique 1024-byte prefix.
	PrevishKeyLength = 1024
)

var (
	// RKeyMin is a minimum key value which sorts before all other keys.
	RKeyMin = RKey("")
	// KeyMin is a minimum key value which sorts before all other keys.
	KeyMin = Key(RKeyMin)
	// RKeyMax is a maximum key value which sorts after all other keys.
	RKeyMax = RKey(keysbase.KeyMax)
	// KeyMax is a maximum key value which sorts after all other keys.
	KeyMax = Key(RKeyMax)

	// LocalPrefix is the prefix for all local keys.
	LocalPrefix = Key{localPrefixByte}
	// LocalMax is the end of the local key range. It is itself a global key.
	LocalMax = Key{LocalMaxByte}

	// PrettyPrintKey prints a key in human readable format. It's
	// implemented in package git.com/cockroachdb/cockroach/keys to avoid
	// circular package import dependencies (see keys.PrettyPrint for
	// implementation).
	// valDirs correspond to the encoding direction of each encoded value
	// in the key (if known). If left unspecified, the default encoding
	// direction for each value type is used (see
	// encoding.go:prettyPrintFirstValue).
	// See SafeFormatKey for a redaction-safe implementation.
	PrettyPrintKey func(valDirs []encoding.Direction, key Key) string

	// SafeFormatKey is the generalized redaction function used to redact pretty
	// printed keys. It's implemented in git.com/cockroachdb/cockroach/keys to
	// avoid circular package import dependencies (see keys.SafeFormat for
	// implementation).
	// valDirs correspond to the encoding direction of each encoded value
	// in the key (if known). If left unspecified, the default encoding
	// direction for each value type is used (see encoding.go:prettyPrintFirstValue).
	SafeFormatKey func(w redact.SafeWriter, valDirs []encoding.Direction, key Key)

	// PrettyPrintRange prints a key range in human readable format. It's
	// implemented in package git.com/cockroachdb/cockroach/keys to avoid
	// package circle import.
	PrettyPrintRange func(start, end Key, maxChars int) redact.RedactableString
)

// RKey denotes a Key whose local addressing has been accounted for.
// A key can be transformed to an RKey by keys.Addr().
//
// RKey stands for "resolved key," as in a key whose address has been resolved.
type RKey Key

// AsRawKey returns the RKey as a Key. This is to be used only in select
// situations in which an RKey is known to not contain a wrapped locally-
// addressed Key. That is, it must only be used when the original Key was not a
// local key. Whenever the Key which created the RKey is still available, it
// should be used instead.
func (rk RKey) AsRawKey() Key {
	return Key(rk)
}

// Less returns true if receiver < otherRK.
func (rk RKey) Less(otherRK RKey) bool {
	return rk.Compare(otherRK) < 0
}

// Compare compares the two RKeys.
func (rk RKey) Compare(other RKey) int {
	return bytes.Compare(rk, other)
}

// Equal checks for byte-wise equality.
func (rk RKey) Equal(other []byte) bool {
	return bytes.Equal(rk, other)
}

// Next returns the RKey that sorts immediately after the given one.
// The method may only take a shallow copy of the RKey, so both the
// receiver and the return value should be treated as immutable after.
func (rk RKey) Next() RKey {
	return RKey(encoding.BytesNext(rk))
}

// PrefixEnd determines the end key given key as a prefix, that is the
// key that sorts precisely behind all keys starting with prefix: "1"
// is added to the final byte and the carry propagated. The special
// cases of nil and KeyMin always returns KeyMax.
func (rk RKey) PrefixEnd() RKey {
	return RKey(keysbase.PrefixEnd(rk))
}

// SafeFormat - see Key.SafeFormat.
func (rk RKey) SafeFormat(w redact.SafePrinter, r rune) {
	rk.AsRawKey().SafeFormat(w, r)
}

func (rk RKey) String() string {
	return Key(rk).String()
}

// StringWithDirs - see Key.StringWithDirs.
func (rk RKey) StringWithDirs(valDirs []encoding.Direction) string {
	var buf redact.StringBuilder
	Key(rk).StringWithDirs(&buf, valDirs)
	return buf.String()
}

// Key is a custom type for a byte string in proto
// messages which refer to Cockroach keys.
type Key []byte

// Clone returns a copy of the key.
func (k Key) Clone() Key {
	if k == nil {
		return nil
	}
	c := make(Key, len(k))
	copy(c, k)
	return c
}

// Next returns the next key in lexicographic sort order. The method may only
// take a shallow copy of the Key, so both the receiver and the return
// value should be treated as immutable after.
func (k Key) Next() Key {
	return Key(encoding.BytesNext(k))
}

// Prevish returns a previous key in lexicographic sort order. It is impossible
// in general to find the exact immediate predecessor key, because it has an
// infinite number of 0xff bytes at the end, so this returns the nearest
// previous key right-padded with 0xff up to length bytes. An infinite number of
// keys may exist between Key and Key.Prevish(), as keys have unbounded length.
// This also implies that k.Prevish().IsPrev(k) will often be false.
//
// PrevishKeyLength can be used as a reasonable length in most situations.
//
// The method may only take a shallow copy of the Key, so both the receiver and
// the return value should be treated as immutable after.
func (k Key) Prevish(length int) Key {
	return Key(encoding.BytesPrevish(k, length))
}

// IsPrev is a more efficient version of k.Next().Equal(m).
func (k Key) IsPrev(m Key) bool {
	l := len(m) - 1
	return l == len(k) && m[l] == 0 && k.Equal(m[:l])
}

// PrefixEnd determines the end key given key as a prefix, that is the
// key that sorts precisely behind all keys starting with prefix: "1"
// is added to the final byte and the carry propagated. The special
// cases of nil and KeyMin always returns KeyMax.
func (k Key) PrefixEnd() Key {
	return Key(keysbase.PrefixEnd(k))
}

// Equal returns whether two keys are identical.
func (k Key) Equal(l Key) bool {
	return bytes.Equal(k, l)
}

// Compare compares the two Keys.
func (k Key) Compare(b Key) int {
	return bytes.Compare(k, b)
}

// Less says whether key k is less than key b.
func (k Key) Less(b Key) bool {
	return k.Compare(b) < 0
}

// Clamp fixes the key to something within the range a < k < b.
func (k Key) Clamp(min, max Key) (Key, error) {
	if max.Less(min) {
		return nil, errors.Newf("cannot clamp when min '%s' is larger than max '%s'", min, max)
	}
	result := k
	if k.Less(min) {
		result = min
	}
	if max.Less(k) {
		result = max
	}
	return result, nil
}

// SafeFormat implements the redact.SafeFormatter interface.
func (k Key) SafeFormat(w redact.SafePrinter, _ rune) {
	SafeFormatKey(w, nil /* valDirs */, k)
}

// String returns a string-formatted version of the key.
func (k Key) String() string {
	return redact.StringWithoutMarkers(k)
}

// StringWithDirs is the value encoding direction-aware version of String.
//
// Args:
//
//	valDirs: The direction for the key's components, generally needed for
//	correct decoding. If nil, the values are pretty-printed with default
//	encoding direction.
//
//	returned, plus a "..." suffix.
func (k Key) StringWithDirs(buf *redact.StringBuilder, valDirs []encoding.Direction) {
	if PrettyPrintKey != nil {
		buf.Print(PrettyPrintKey(valDirs, k))
	} else {
		buf.Printf("%q", []byte(k))
	}
}

// Format implements the fmt.Formatter interface.
func (k Key) Format(f fmt.State, verb rune) {
	// Note: this implementation doesn't handle the width and precision
	// specifiers such as "%20.10s".
	if verb == 'x' {
		fmt.Fprintf(f, "%x", []byte(k))
	} else if PrettyPrintKey != nil {
		fmt.Fprint(f, PrettyPrintKey(nil /* valDirs */, k))
	} else {
		fmt.Fprint(f, strconv.Quote(string(k)))
	}
}

const (
	checksumUninitialized = 0
	checksumSize          = 4
	tagPos                = checksumSize
	headerSize            = tagPos + 1

	extendedMVCCValLenSize = 4
	extendedPreludeSize    = extendedMVCCValLenSize + 1
)

var _ redact.SafeFormatter = new(ValueType)
var _ redact.SafeFormatter = new(LockStateInfo)
var _ redact.SafeFormatter = new(RKey)
var _ redact.SafeFormatter = new(Key)
var _ redact.SafeFormatter = new(StoreProperties)
var _ redact.SafeFormatter = new(Transaction)
var _ redact.SafeFormatter = new(ChangeReplicasTrigger)
var _ redact.SafeFormatter = new(Lease)
var _ redact.SafeFormatter = new(Span)
var _ redact.SafeFormatter = new(RSpan)
var _ redact.SafeFormatter = new(LockAcquisition)

// Safeformat implements the redact.SafeFormatter interface.
func (t ValueType) SafeFormat(w redact.SafePrinter, _ rune) {
	w.SafeString(redact.SafeString(t.String()))
}

func (v Value) checksum() uint32 {
	if len(v.RawBytes) < checksumSize {
		return 0
	}

	checksumStart := 0
	if v.usesExtendedEncoding() {
		extendedHeaderSize := int(extendedMVCCValLenSize + binary.BigEndian.Uint32(v.RawBytes))
		if len(v.RawBytes) < extendedHeaderSize+headerSize {
			return 0
		}
		checksumStart = extendedHeaderSize + 1
	}

	_, u, err := encoding.DecodeUint32Ascending(v.RawBytes[checksumStart : checksumStart+checksumSize])
	if err != nil {
		panic(err)
	}
	return u
}

func (v *Value) setChecksum(cksum uint32) {
	if len(v.RawBytes) >= checksumSize {
		encoding.EncodeUint32Ascending(v.RawBytes[:0], cksum)
	}
}

func (v *Value) usesExtendedEncoding() bool {
	return len(v.RawBytes) > headerSize && v.RawBytes[tagPos] == byte(ValueType_MVCC_EXTENDED_ENCODING_SENTINEL)
}

// InitChecksum initializes a checksum based on the provided key and
// the contents of the value. If the value contains a byte slice, the
// checksum includes it directly.
//
// TODO(peter): This method should return an error if the Value is corrupted
// (e.g. the RawBytes field is > 0 but smaller than the header size).
func (v *Value) InitChecksum(key []byte) {
	if v.RawBytes == nil {
		return
	}
	// Should be uninitialized.
	if v.checksum() != checksumUninitialized {
		panic(errors.Errorf("initialized checksum = %x", v.checksum()))
	}
	v.setChecksum(v.computeChecksum(key))
}

// ClearChecksum clears the checksum value.
func (v *Value) ClearChecksum() {
	v.setChecksum(0)
}

// Verify verifies the value's Checksum matches a newly-computed
// checksum of the value's contents. If the value's Checksum is not
// set the verification is a noop.
func (v Value) Verify(key []byte) error {
	if err := v.VerifyHeader(); err != nil {
		return err
	}
	if sum := v.checksum(); sum != 0 {
		if computedSum := v.computeChecksum(key); computedSum != sum {
			return errors.Errorf("%s: invalid checksum (%x) value [% x]",
				Key(key), computedSum, v.RawBytes)
		}
	}
	return nil
}

// VerifyHeader checks that, if the Value is not empty, it includes a header.
func (v Value) VerifyHeader() error {
	if n := len(v.RawBytes); n > 0 && n < headerSize {
		return errors.Errorf("invalid header size: %d", n)
	}
	return nil
}

// ShallowClone returns a shallow clone of the receiver.
func (v *Value) ShallowClone() *Value {
	if v == nil {
		return nil
	}
	t := *v
	return &t
}

// IsPresent returns true if the value is present (existent and not a tombstone).
func (v *Value) IsPresent() bool {
	if v == nil || len(v.RawBytes) == 0 {
		return false
	}
	// TODO(ssd): This is a bit awkward because this is the right thing to
	// do for production callers trying to determine if this value is a
	// tombstone. But, many tests shove random strings into RawBytes, and in
	// then case we'll hit this case if the 5th character of that string
	// happens to be `e` (ascii 101). There aren't _that_ many callers to
	// IsPresent(). We may just need to audit them all.
	if v.usesExtendedEncoding() {
		extendedHeaderSize := extendedPreludeSize + binary.BigEndian.Uint32(v.RawBytes)
		return len(v.RawBytes) > int(extendedHeaderSize)
	}
	return true
}

// MakeValueFromString returns a value with bytes and tag set.
func MakeValueFromString(s string) Value {
	v := Value{}
	v.SetString(s)
	return v
}

// MakeValueFromBytes returns a value with bytes and tag set.
func MakeValueFromBytes(bs []byte) Value {
	v := Value{}
	v.SetBytes(bs)
	return v
}

// MakeValueFromBytesAndTimestamp returns a value with bytes, timestamp and
// tag set.
func MakeValueFromBytesAndTimestamp(bs []byte, t hlc.Timestamp) Value {
	v := Value{Timestamp: t}
	v.SetBytes(bs)
	return v
}

// GetTag retrieves the value type.
func (v Value) GetTag() ValueType {
	if len(v.RawBytes) <= tagPos {
		return ValueType_UNKNOWN
	}
	if v.RawBytes[tagPos] == byte(ValueType_MVCC_EXTENDED_ENCODING_SENTINEL) {
		simpleTagPos := v.extendedSimpleTagPos()
		if len(v.RawBytes) <= simpleTagPos {
			return ValueType_UNKNOWN
		}
		return ValueType(v.RawBytes[simpleTagPos])
	}
	return ValueType(v.RawBytes[tagPos])
}

// GetMVCCValueHeader returns the MVCCValueHeader if one exists.
func (v Value) GetMVCCValueHeader() (enginepb.MVCCValueHeader, error) {
	if len(v.RawBytes) <= tagPos {
		return enginepb.MVCCValueHeader{}, nil
	}
	if v.RawBytes[tagPos] == byte(ValueType_MVCC_EXTENDED_ENCODING_SENTINEL) {
		extendedHeaderSize := extendedPreludeSize + binary.BigEndian.Uint32(v.RawBytes)
		if len(v.RawBytes) < int(extendedHeaderSize) {
			return enginepb.MVCCValueHeader{}, nil
		}

		parseBytes := v.RawBytes[extendedPreludeSize:extendedHeaderSize]
		var vh enginepb.MVCCValueHeader
		// NOTE: we don't use protoutil to avoid passing header through an interface,
		// which would cause a heap allocation and incur the cost of dynamic dispatch.
		if err := vh.Unmarshal(parseBytes); err != nil {
			return enginepb.MVCCValueHeader{}, errors.Wrapf(err, "unmarshaling MVCCValueHeader")
		}
		return vh, nil
	}
	return enginepb.MVCCValueHeader{}, nil
}

func (v *Value) setTag(t ValueType) {
	v.RawBytes[tagPos] = byte(t)
}

// extendedSimpleTagPos returns the position of the value tag assuming
// that the value contains an enginepb.MVCCValueHeader.
func (v Value) extendedSimpleTagPos() int {
	return int(extendedMVCCValLenSize + binary.BigEndian.Uint32(v.RawBytes) + headerSize)
}

func (v Value) dataBytes() []byte {
	if v.usesExtendedEncoding() {
		simpleTagPos := v.extendedSimpleTagPos()
		return v.RawBytes[simpleTagPos+1:]
	}
	return v.RawBytes[headerSize:]
}

// TagAndDataBytes returns the value's tag and data (no checksum, no timestamp).
// This is suitable to be used as the expected value in a CPut.
func (v Value) TagAndDataBytes() []byte {
	if v.usesExtendedEncoding() {
		simpleTagPos := v.extendedSimpleTagPos()
		return v.RawBytes[simpleTagPos:]
	}
	return v.RawBytes[tagPos:]
}

func (v *Value) ensureRawBytes(size int) {
	if cap(v.RawBytes) < size {
		v.RawBytes = make([]byte, size)
		return
	}
	v.RawBytes = v.RawBytes[:size]
	v.setChecksum(checksumUninitialized)
}

// EqualTagAndData returns a boolean reporting whether the receiver and the parameter
// have equivalent byte values. This check ignores the optional checksum field
// in the Values' byte slices, returning only whether the Values have the same
// tag and encoded data.
//
// This method should be used whenever the raw bytes of two Values are being
// compared instead of comparing the RawBytes slices directly because it ignores
// the checksum header, which is optional.
func (v Value) EqualTagAndData(o Value) bool {
	return bytes.Equal(v.TagAndDataBytes(), o.TagAndDataBytes())
}

// SetBytes copies the bytes and tag field to the receiver and clears the
// checksum.
func (v *Value) SetBytes(b []byte) {
	v.ensureRawBytes(headerSize + len(b))
	copy(v.dataBytes(), b)
	v.setTag(ValueType_BYTES)
}

// AllocBytes allocates space for a BYTES value of the given size and clears the
// checksum. The caller must populate the returned slice with exactly the same
// number of bytes.
func (v *Value) AllocBytes(size int) []byte {
	v.ensureRawBytes(headerSize + size)
	v.setTag(ValueType_BYTES)
	return v.RawBytes[headerSize:]
}

// SetTagAndData copies the bytes and tag field to the receiver and clears the
// checksum. As opposed to SetBytes, b is assumed to contain the tag too, not
// just the data.
func (v *Value) SetTagAndData(b []byte) {
	v.ensureRawBytes(checksumSize + len(b))
	copy(v.TagAndDataBytes(), b)
}

// SetString sets the bytes and tag field of the receiver and clears the
// checksum. This is identical to SetBytes, but specialized for a string
// argument.
func (v *Value) SetString(s string) {
	v.ensureRawBytes(headerSize + len(s))
	copy(v.dataBytes(), s)
	v.setTag(ValueType_BYTES)
}

// SetFloat encodes the specified float64 value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetFloat(f float64) {
	v.ensureRawBytes(headerSize + 8)
	encoding.EncodeUint64Ascending(v.RawBytes[headerSize:headerSize], math.Float64bits(f))
	v.setTag(ValueType_FLOAT)
}

// SetGeo encodes the specified geo value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetGeo(so geopb.SpatialObject) error {
	bytes, err := protoutil.Marshal(&so)
	if err != nil {
		return err
	}
	v.ensureRawBytes(headerSize + len(bytes))
	copy(v.dataBytes(), bytes)
	v.setTag(ValueType_GEO)
	return nil
}

// SetBox2D encodes the specified Box2D value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetBox2D(b geopb.BoundingBox) {
	v.ensureRawBytes(headerSize + 32)
	encoding.EncodeUint64Ascending(v.RawBytes[headerSize:headerSize], math.Float64bits(b.LoX))
	encoding.EncodeUint64Ascending(v.RawBytes[headerSize+8:headerSize+8], math.Float64bits(b.HiX))
	encoding.EncodeUint64Ascending(v.RawBytes[headerSize+16:headerSize+16], math.Float64bits(b.LoY))
	encoding.EncodeUint64Ascending(v.RawBytes[headerSize+24:headerSize+24], math.Float64bits(b.HiY))
	v.setTag(ValueType_BOX2D)
}

// SetBool encodes the specified bool value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetBool(b bool) {
	// 0 or 1 will always encode to a 1-byte long varint.
	v.ensureRawBytes(headerSize + 1)
	i := int64(0)
	if b {
		i = 1
	}
	_ = binary.PutVarint(v.RawBytes[headerSize:], i)
	v.setTag(ValueType_INT)
}

// SetInt encodes the specified int64 value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetInt(i int64) {
	v.ensureRawBytes(headerSize + binary.MaxVarintLen64)
	n := binary.PutVarint(v.RawBytes[headerSize:], i)
	v.RawBytes = v.RawBytes[:headerSize+n]
	v.setTag(ValueType_INT)
}

// SetProto encodes the specified proto message into the bytes field of the
// receiver and clears the checksum. If the proto message is an
// InternalTimeSeriesData, the tag will be set to TIMESERIES rather than BYTES.
func (v *Value) SetProto(msg protoutil.Message) error {
	// All of the Cockroach protos implement MarshalTo and Size. So we marshal
	// directly into the Value.RawBytes field instead of allocating a separate
	// []byte and copying.
	v.ensureRawBytes(headerSize + msg.Size())
	if _, err := protoutil.MarshalToSizedBuffer(msg, v.RawBytes[headerSize:]); err != nil {
		return err
	}
	// Special handling for timeseries data.
	if _, ok := msg.(*InternalTimeSeriesData); ok {
		v.setTag(ValueType_TIMESERIES)
	} else {
		v.setTag(ValueType_BYTES)
	}
	return nil
}

// SetTime encodes the specified time value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetTime(t time.Time) {
	const encodingSizeOverestimate = 11
	v.ensureRawBytes(headerSize + encodingSizeOverestimate)
	v.RawBytes = encoding.EncodeTimeAscending(v.RawBytes[:headerSize], t)
	v.setTag(ValueType_TIME)
}

// SetTimeTZ encodes the specified time value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetTimeTZ(t timetz.TimeTZ) {
	v.ensureRawBytes(headerSize + encoding.EncodedTimeTZMaxLen)
	v.RawBytes = encoding.EncodeTimeTZAscending(v.RawBytes[:headerSize], t)
	v.setTag(ValueType_TIMETZ)
}

// SetDuration encodes the specified duration value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetDuration(t duration.Duration) error {
	var err error
	v.ensureRawBytes(headerSize + encoding.EncodedDurationMaxLen)
	v.RawBytes, err = encoding.EncodeDurationAscending(v.RawBytes[:headerSize], t)
	if err != nil {
		return err
	}
	v.setTag(ValueType_DURATION)
	return nil
}

// SetBitArray encodes the specified bit array value into the bytes field of the
// receiver, sets the tag and clears the checksum.
func (v *Value) SetBitArray(t bitarray.BitArray) {
	words, _ := t.EncodingParts()
	v.ensureRawBytes(headerSize + encoding.MaxNonsortingUvarintLen + 8*len(words))
	v.RawBytes = encoding.EncodeUntaggedBitArrayValue(v.RawBytes[:headerSize], t)
	v.setTag(ValueType_BITARRAY)
}

// SetDecimal encodes the specified decimal value into the bytes field of
// the receiver using Gob encoding, sets the tag and clears the checksum.
func (v *Value) SetDecimal(dec *apd.Decimal) error {
	decSize := encoding.UpperBoundNonsortingDecimalSize(dec)
	v.ensureRawBytes(headerSize + decSize)
	v.RawBytes = encoding.EncodeNonsortingDecimal(v.RawBytes[:headerSize], dec)
	v.setTag(ValueType_DECIMAL)
	return nil
}

// SetTuple sets the tuple bytes and tag field of the receiver and clears the
// checksum.
func (v *Value) SetTuple(data []byte) {
	v.ensureRawBytes(headerSize + len(data))
	copy(v.dataBytes(), data)
	v.setTag(ValueType_TUPLE)
}

// GetBytes returns the bytes field of the receiver. If the tag is not
// BYTES an error will be returned.
func (v Value) GetBytes() ([]byte, error) {
	if tag := v.GetTag(); tag != ValueType_BYTES {
		return nil, errors.Errorf("value type is not %s: %s", ValueType_BYTES, tag)
	}
	return v.dataBytes(), nil
}

// GetFloat decodes a float64 value from the bytes field of the receiver. If
// the bytes field is not 8 bytes in length or the tag is not FLOAT an error
// will be returned.
func (v Value) GetFloat() (float64, error) {
	if tag := v.GetTag(); tag != ValueType_FLOAT {
		return 0, errors.Errorf("value type is not %s: %s", ValueType_FLOAT, tag)
	}
	dataBytes := v.dataBytes()
	if len(dataBytes) != 8 {
		return 0, errors.Errorf("float64 value should be exactly 8 bytes: %d", len(dataBytes))
	}
	_, u, err := encoding.DecodeUint64Ascending(dataBytes)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(u), nil
}

// GetGeo decodes a geo value from the bytes field of the receiver. If the
// tag is not GEO an error will be returned.
func (v Value) GetGeo() (geopb.SpatialObject, error) {
	if tag := v.GetTag(); tag != ValueType_GEO {
		return geopb.SpatialObject{}, errors.Errorf("value type is not %s: %s", ValueType_GEO, tag)
	}
	var ret geopb.SpatialObject
	err := protoutil.Unmarshal(v.dataBytes(), &ret)
	return ret, err
}

// GetBox2D decodes a geo value from the bytes field of the receiver. If the
// tag is not BOX2D an error will be returned.
func (v Value) GetBox2D() (geopb.BoundingBox, error) {
	box := geopb.BoundingBox{}
	if tag := v.GetTag(); tag != ValueType_BOX2D {
		return box, errors.Errorf("value type is not %s: %s", ValueType_BOX2D, tag)
	}
	dataBytes := v.dataBytes()
	if len(dataBytes) != 32 {
		return box, errors.Errorf("float64 value should be exactly 32 bytes: %d", len(dataBytes))
	}
	var err error
	var val uint64
	dataBytes, val, err = encoding.DecodeUint64Ascending(dataBytes)
	if err != nil {
		return box, err
	}
	box.LoX = math.Float64frombits(val)
	dataBytes, val, err = encoding.DecodeUint64Ascending(dataBytes)
	if err != nil {
		return box, err
	}
	box.HiX = math.Float64frombits(val)
	dataBytes, val, err = encoding.DecodeUint64Ascending(dataBytes)
	if err != nil {
		return box, err
	}
	box.LoY = math.Float64frombits(val)
	_, val, err = encoding.DecodeUint64Ascending(dataBytes)
	if err != nil {
		return box, err
	}
	box.HiY = math.Float64frombits(val)

	return box, nil
}

// GetBool decodes a bool value from the bytes field of the receiver. If the
// tag is not INT (the tag used for bool values) or the value cannot be decoded
// an error will be returned.
func (v Value) GetBool() (bool, error) {
	if tag := v.GetTag(); tag != ValueType_INT {
		return false, errors.Errorf("value type is not %s: %s", ValueType_INT, tag)
	}
	i, n := binary.Varint(v.dataBytes())
	if n <= 0 {
		return false, errors.Errorf("int64 varint decoding failed: %d", n)
	}
	if i > 1 || i < 0 {
		return false, errors.Errorf("invalid bool: %d", i)
	}
	return i != 0, nil
}

// GetInt decodes an int64 value from the bytes field of the receiver. If the
// tag is not INT or the value cannot be decoded an error will be returned.
func (v Value) GetInt() (int64, error) {
	if tag := v.GetTag(); tag != ValueType_INT {
		return 0, errors.Errorf("value type is not %s: %s", ValueType_INT, tag)
	}
	i, n := binary.Varint(v.dataBytes())
	if n <= 0 {
		return 0, errors.Errorf("int64 varint decoding failed: %d", n)
	}
	return i, nil
}

// GetProto unmarshals the bytes field of the receiver into msg. If
// unmarshalling fails or the tag is not BYTES, an error will be
// returned.
func (v Value) GetProto(msg protoutil.Message) error {
	expectedTag := ValueType_BYTES

	// Special handling for ts data.
	if _, ok := msg.(*InternalTimeSeriesData); ok {
		expectedTag = ValueType_TIMESERIES
	}

	if tag := v.GetTag(); tag != expectedTag {
		return errors.Errorf("value type is not %s: %s", expectedTag, tag)
	}
	return protoutil.Unmarshal(v.dataBytes(), msg)
}

// GetTime decodes a time value from the bytes field of the receiver. If the
// tag is not TIME an error will be returned.
func (v Value) GetTime() (time.Time, error) {
	if tag := v.GetTag(); tag != ValueType_TIME {
		return time.Time{}, errors.Errorf("value type is not %s: %s", ValueType_TIME, tag)
	}
	_, t, err := encoding.DecodeTimeAscending(v.dataBytes())
	return t, err
}

// GetTimeTZ decodes a time value from the bytes field of the receiver. If the
// tag is not TIMETZ an error will be returned.
func (v Value) GetTimeTZ() (timetz.TimeTZ, error) {
	if tag := v.GetTag(); tag != ValueType_TIMETZ {
		return timetz.TimeTZ{}, errors.Errorf("value type is not %s: %s", ValueType_TIMETZ, tag)
	}
	_, t, err := encoding.DecodeTimeTZAscending(v.dataBytes())
	return t, err
}

// GetDuration decodes a duration value from the bytes field of the receiver. If
// the tag is not DURATION an error will be returned.
func (v Value) GetDuration() (duration.Duration, error) {
	if tag := v.GetTag(); tag != ValueType_DURATION {
		return duration.Duration{}, errors.Errorf("value type is not %s: %s", ValueType_DURATION, tag)
	}
	_, t, err := encoding.DecodeDurationAscending(v.dataBytes())
	return t, err
}

// GetBitArray decodes a bit array value from the bytes field of the receiver. If
// the tag is not BITARRAY an error will be returned.
func (v Value) GetBitArray() (bitarray.BitArray, error) {
	if tag := v.GetTag(); tag != ValueType_BITARRAY {
		return bitarray.BitArray{}, errors.Errorf("value type is not %s: %s", ValueType_BITARRAY, tag)
	}
	_, t, err := encoding.DecodeUntaggedBitArrayValue(v.dataBytes())
	return t, err
}

// GetDecimal decodes a decimal value from the bytes of the receiver. If the
// tag is not DECIMAL an error will be returned.
func (v Value) GetDecimal() (apd.Decimal, error) {
	if tag := v.GetTag(); tag != ValueType_DECIMAL {
		return apd.Decimal{}, errors.Errorf("value type is not %s: %s", ValueType_DECIMAL, tag)
	}
	return encoding.DecodeNonsortingDecimal(v.dataBytes(), nil)
}

// GetDecimalInto decodes a decimal value from the bytes of the receiver,
// writing it directly into the provided non-null apd.Decimal. If the
// tag is not DECIMAL an error will be returned.
func (v Value) GetDecimalInto(d *apd.Decimal) error {
	if tag := v.GetTag(); tag != ValueType_DECIMAL {
		return errors.Errorf("value type is not %s: %s", ValueType_DECIMAL, tag)
	}
	return encoding.DecodeIntoNonsortingDecimal(d, v.dataBytes(), nil)
}

// GetTimeseries decodes an InternalTimeSeriesData value from the bytes
// field of the receiver. An error will be returned if the tag is not
// TIMESERIES or if decoding fails.
func (v Value) GetTimeseries() (InternalTimeSeriesData, error) {
	ts := InternalTimeSeriesData{}
	// GetProto mutates its argument. `return ts, v.GetProto(&ts)`
	// happens to work in gc, but does not work in gccgo.
	//
	// See https://github.com/golang/go/issues/23188.
	err := v.GetProto(&ts)
	return ts, err
}

// GetTuple returns the tuple bytes of the receiver. If the tag is not TUPLE an
// error will be returned.
func (v Value) GetTuple() ([]byte, error) {
	if tag := v.GetTag(); tag != ValueType_TUPLE {
		return nil, errors.Errorf("value type is not %s: %s", ValueType_TUPLE, tag)
	}
	return v.dataBytes(), nil
}

var crc32Pool = sync.Pool{
	New: func() interface{} {
		return crc32.NewIEEE()
	},
}

func computeChecksum(key, rawBytes []byte, crc hash.Hash32) uint32 {
	if len(rawBytes) < headerSize {
		return 0
	}

	if rawBytes[tagPos] == byte(ValueType_MVCC_EXTENDED_ENCODING_SENTINEL) {
		simpleValueStart := extendedMVCCValLenSize + binary.BigEndian.Uint32(rawBytes) + 1
		rawBytes = rawBytes[simpleValueStart:]
		if len(rawBytes) < headerSize {
			return 0
		}
	}

	if _, err := crc.Write(key); err != nil {
		panic(err)
	}
	if _, err := crc.Write(rawBytes[checksumSize:]); err != nil {
		panic(err)
	}
	sum := crc.Sum32()
	crc.Reset()
	// We reserved the value 0 (checksumUninitialized) to indicate that a checksum
	// has not been initialized. This reservation is accomplished by folding a
	// computed checksum of 0 to the value 1.
	if sum == checksumUninitialized {
		return 1
	}
	return sum
}

// computeChecksum computes a checksum based on the provided key and
// the contents of the value.
func (v Value) computeChecksum(key []byte) uint32 {
	crc := crc32Pool.Get().(hash.Hash32)
	sum := computeChecksum(key, v.RawBytes, crc)
	crc32Pool.Put(crc)
	return sum
}

// PrettyPrint returns the value in a human readable format.
// e.g. `Put /Table/51/1/1/0 -> /TUPLE/2:2:Int/7/1:3:Float/6.28`
// In `1:3:Float/6.28`, the `1` is the column id diff as stored, `3` is the
// computed (i.e. not stored) actual column id, `Float` is the type, and `6.28`
// is the encoded value.
func (v Value) PrettyPrint() (ret string) {
	if len(v.RawBytes) == 0 {
		return "/<empty>"
	}
	// In certain cases untagged bytes could be malformed because they are
	// coming from user input, in which case recover with an error instead
	// of crashing.
	defer func() {
		if r := recover(); r != nil {
			ret = fmt.Sprintf("/<err: paniced parsing with %v>", r)
		}
	}()
	var buf bytes.Buffer
	t := v.GetTag()
	buf.WriteRune('/')
	buf.WriteString(t.String())
	buf.WriteRune('/')

	var err error
	switch t {
	case ValueType_TUPLE:
		b := v.dataBytes()
		var colID uint32
		for i := 0; len(b) > 0; i++ {
			if i != 0 {
				buf.WriteRune('/')
			}
			_, _, colIDDelta, typ, err := encoding.DecodeValueTag(b)
			if err != nil {
				break
			}
			colID += colIDDelta
			var s string
			b, s, err = encoding.PrettyPrintValueEncoded(b)
			if err != nil {
				break
			}
			fmt.Fprintf(&buf, "%d:%d:%s/%s", colIDDelta, colID, typ, s)
		}
	case ValueType_INT:
		var i int64
		i, err = v.GetInt()
		buf.WriteString(strconv.FormatInt(i, 10))
	case ValueType_FLOAT:
		var f float64
		f, err = v.GetFloat()
		buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	case ValueType_BYTES:
		var data []byte
		data, err = v.GetBytes()
		if encoding.PrintableBytes(data) {
			buf.WriteString(string(data))
		} else {
			buf.WriteString("0x")
			buf.WriteString(hex.EncodeToString(data))
		}
	case ValueType_BITARRAY:
		var data bitarray.BitArray
		data, err = v.GetBitArray()
		buf.WriteByte('B')
		data.Format(&buf)
	case ValueType_TIME:
		var t time.Time
		t, err = v.GetTime()
		buf.WriteString(t.UTC().Format(time.RFC3339Nano))
	case ValueType_DECIMAL:
		var d apd.Decimal
		d, err = v.GetDecimal()
		buf.WriteString(d.String())
	case ValueType_DURATION:
		var d duration.Duration
		d, err = v.GetDuration()
		buf.WriteString(d.StringNanos())
	case ValueType_TIMETZ:
		var tz timetz.TimeTZ
		tz, err = v.GetTimeTZ()
		buf.WriteString(tz.String())
	case ValueType_GEO:
		var g geopb.SpatialObject
		g, err = v.GetGeo()
		buf.WriteString(g.String())
	case ValueType_BOX2D:
		var g geopb.BoundingBox
		g, err = v.GetBox2D()
		buf.WriteString(g.String())
	default:
		err = errors.Errorf("unknown tag: %s", t)
	}
	if err != nil {
		// Ignore the contents of buf and return directly.
		return fmt.Sprintf("/<err: %s>", err)
	}
	return buf.String()
}

// SafeFormat implements the redact.SafeFormatter interface.
func (sp StoreProperties) SafeFormat(w redact.SafePrinter, _ rune) {
	w.SafeString(redact.SafeString(sp.Dir))
	w.SafeString(":")
	if sp.ReadOnly {
		w.SafeString(" ro")
	} else {
		w.SafeString(" rw")
	}
	w.Printf(" encrypted=%t", sp.Encrypted)
	if sp.WalFailoverPath != nil {
		w.Printf(" wal_failover_path=%s", redact.SafeString(*sp.WalFailoverPath))
	}
	if sp.FileStoreProperties != nil {
		w.SafeString(" fs:{")
		w.Printf("bdev=%s", redact.SafeString(sp.FileStoreProperties.BlockDevice))
		w.Printf(" fstype=%s", redact.SafeString(sp.FileStoreProperties.FsType))
		w.Printf(" mountpoint=%s", redact.SafeString(sp.FileStoreProperties.MountPoint))
		w.Printf(" mountopts=%s", redact.SafeString(sp.FileStoreProperties.MountOptions))
		w.SafeString("}")
	}
}

// Kind returns the kind of commit trigger as a string.
func (ct InternalCommitTrigger) Kind() redact.SafeString {
	switch {
	case ct.SplitTrigger != nil:
		return "split"
	case ct.MergeTrigger != nil:
		return "merge"
	case ct.ChangeReplicasTrigger != nil:
		return "change-replicas"
	case ct.ModifiedSpanTrigger != nil:
		switch {
		case ct.ModifiedSpanTrigger.NodeLivenessSpan != nil:
			return "modified-span (node-liveness)"
		default:
			panic(fmt.Sprintf("unknown modified-span commit trigger kind %v", ct))
		}
	case ct.StickyBitTrigger != nil:
		return "sticky-bit"
	default:
		panic("unknown commit trigger kind")
	}
}

// IsFinalized determines whether the transaction status is in a finalized
// state. A finalized state is terminal, meaning that once a transaction
// enters one of these states, it will never leave it.
func (ts TransactionStatus) IsFinalized() bool {
	return ts == COMMITTED || ts == ABORTED
}

// SafeValue implements the redact.SafeValue interface.
func (TransactionStatus) SafeValue() {}

// MakeTransaction creates a new transaction. The transaction key is
// composed using the specified baseKey (for locality with data
// affected by the transaction) and a random ID to guarantee
// uniqueness. The specified user-level priority is combined with a
// randomly chosen value to yield a final priority, used to settle
// write conflicts in a way that avoids starvation of long-running
// transactions (see Replica.PushTxn).
//
// coordinatorNodeID is provided to track the SQL (or possibly KV) node
// that created this transaction, in order to be used (as
// of this writing) to enable observability on contention events
// between different transactions.
//
// baseKey can be nil, in which case it will be set when sending the first
// write.
//
// omitInRangefeeds controls whether the transaction's writes are exposed via
// rangefeeds. When set to true, all the transaction's writes will be
// filtered out by rangefeeds, and will not be available in changefeeds.
func MakeTransaction(
	name string,
	baseKey Key,
	isoLevel isolation.Level,
	userPriority UserPriority,
	now hlc.Timestamp,
	maxOffsetNs int64,
	coordinatorNodeID int32,
	admissionPriority admissionpb.WorkPriority,
	omitInRangefeeds bool,
) Transaction {
	u := uuid.MakeV4()
	gul := now.Add(maxOffsetNs, 0)

	return Transaction{
		TxnMeta: enginepb.TxnMeta{
			Key:               baseKey,
			ID:                u,
			IsoLevel:          isoLevel,
			WriteTimestamp:    now,
			MinTimestamp:      now,
			Priority:          MakePriority(userPriority),
			Sequence:          0, // 1-indexed, incremented before each Request
			CoordinatorNodeID: coordinatorNodeID,
		},
		Name:                   name,
		LastHeartbeat:          now,
		ReadTimestamp:          now,
		GlobalUncertaintyLimit: gul,
		AdmissionPriority:      int32(admissionPriority),
		// When set to true OmitInRangefeeds indicates that none of the
		// transaction's writes will appear in rangefeeds. Should be set to false
		// for all transactions that write to internal system tables and most other
		// transactions unless specifically stated otherwise (e.g. by the
		// disable_changefeed_replication session variable).
		OmitInRangefeeds: omitInRangefeeds,
	}
}

// LastActive returns the last timestamp at which client activity definitely
// occurred, i.e. the maximum of MinTimestamp and LastHeartbeat.
func (t Transaction) LastActive() hlc.Timestamp {
	ts := t.MinTimestamp
	ts.Forward(t.LastHeartbeat)
	return ts
}

// RequiredFrontier returns the largest timestamp at which the transaction may
// read values when performing a read-only operation. This is the maximum of the
// transaction's read timestamp, its write timestamp, and its global uncertainty
// limit.
func (t *Transaction) RequiredFrontier() hlc.Timestamp {
	// A transaction can observe committed values up to its read timestamp.
	ts := t.ReadTimestamp
	// Forward to the transaction's write timestamp. The transaction will read
	// committed values at its read timestamp but may perform reads up to its
	// intent timestamps if the transaction is reading its own intent writes,
	// which we know to all be at timestamps <= its current write timestamp. See
	// the ownIntent cases in pebbleMVCCScanner.getAndAdvance for more.
	//
	// There is a case where an intent written by a transaction is above the
	// transaction's write timestamp — after a successful intent push. Such
	// cases do allow a transaction to read values above its required frontier.
	// However, this is fine for the purposes of follower reads because an
	// intent that was pushed to a higher timestamp must have at some point been
	// stored with its original write timestamp. The means that a follower with
	// a closed timestamp above the original write timestamp but below the new
	// pushed timestamp will either store the pre-pushed intent or the
	// post-pushed intent, depending on whether replication of the push has
	// completed yet. Either way, the intent will exist in some form on the
	// follower, so either way, the transaction will be able to read its own
	// write.
	ts.Forward(t.WriteTimestamp)
	// Forward to the transaction's global uncertainty limit, because the
	// transaction may observe committed writes from other transactions up to
	// this time and consider them to be "uncertain". When a transaction begins,
	// this will be above its read timestamp, but the read timestamp can surpass
	// the global uncertainty limit due to refreshes or retries.
	ts.Forward(t.GlobalUncertaintyLimit)
	return ts
}

// Clone creates a copy of the given transaction. The copy is shallow because
// none of the references held by a transaction allow interior mutability.
func (t Transaction) Clone() *Transaction {
	return &t
}

// AssertInitialized crashes if the transaction is not initialized.
func (t *Transaction) AssertInitialized(ctx context.Context) {
	if t.ID == (uuid.UUID{}) || t.WriteTimestamp.IsEmpty() {
		log.Fatalf(ctx, "uninitialized txn: %s", *t)
	}
}

// UserPriority is a custom type for transaction's user priority.
type UserPriority float64

func (up UserPriority) String() string {
	switch up {
	case MinUserPriority:
		return "low"
	case UnspecifiedUserPriority, NormalUserPriority:
		return "normal"
	case MaxUserPriority:
		return "high"
	default:
		return fmt.Sprintf("%g", float64(up))
	}
}

const (
	// MinUserPriority is the minimum allowed user priority.
	MinUserPriority UserPriority = 0.001
	// UnspecifiedUserPriority means NormalUserPriority.
	UnspecifiedUserPriority UserPriority = 0
	// NormalUserPriority is set to 1, meaning ops run through the database
	// are all given equal weight when a random priority is chosen. This can
	// be set specifically via client.NewDBWithPriority().
	NormalUserPriority UserPriority = 1
	// MaxUserPriority is the maximum allowed user priority.
	MaxUserPriority UserPriority = 1000
)

// MakePriority generates a random priority value, biased by the specified
// userPriority. If userPriority=100, the random priority will be 100x more
// likely to be greater than if userPriority=1. If userPriority = 0.1, the
// random priority will be 1/10th as likely to be greater than if
// userPriority=NormalUserPriority ( = 1). Balance is achieved when
// userPriority=NormalUserPriority, in which case the priority chosen is
// unbiased.
//
// If userPriority is less than or equal to MinUserPriority, returns
// MinTxnPriority; if greater than or equal to MaxUserPriority, returns
// MaxTxnPriority. If userPriority is 0, returns NormalUserPriority.
func MakePriority(userPriority UserPriority) enginepb.TxnPriority {
	// A currently undocumented feature allows an explicit priority to
	// be set by specifying priority < 1. The explicit priority is
	// simply -userPriority in this case. This is hacky, but currently
	// used for unittesting. Perhaps this should be documented and allowed.
	if userPriority < 0 {
		if -userPriority > UserPriority(math.MaxInt32) {
			panic(fmt.Sprintf("cannot set explicit priority to a value less than -%d", math.MaxInt32))
		}
		return enginepb.TxnPriority(-userPriority)
	} else if userPriority == 0 {
		userPriority = NormalUserPriority
	} else if userPriority >= MaxUserPriority {
		return enginepb.MaxTxnPriority
	} else if userPriority <= MinUserPriority {
		return enginepb.MinTxnPriority
	}

	// We generate random values which are biased according to priorities. If v1 is a value
	// generated for priority p1 and v2 is a value of priority v2, we want the ratio of wins vs
	// losses to be the same with the ratio of priorities:
	//
	//    P[ v1 > v2 ]     p1                                           p1
	//    ------------  =  --     or, equivalently:    P[ v1 > v2 ] = -------
	//    P[ v2 < v1 ]     p2                                         p1 + p2
	//
	//
	// For example, priority 10 wins 10 out of 11 times over priority 1, and it wins 100 out of 101
	// times over priority 0.1.
	//
	//
	// We use the exponential distribution. This distribution has the probability density function
	//   PDF_lambda(x) = lambda * exp(-lambda * x)
	// and the cumulative distribution function (i.e. probability that a random value is smaller
	// than x):
	//   CDF_lambda(x) = Integral_0^x PDF_lambda(x) dx
	//                 = 1 - exp(-lambda * x)
	//
	// Let's assume we generate x from the exponential distribution with the lambda rate set to
	// l1 and we generate y from the distribution with the rate set to l2. The probability that x
	// wins is:
	//    P[ x > y ] = Integral_0^inf Integral_0^x PDF_l1(x) PDF_l2(y) dy dx
	//               = Integral_0^inf PDF_l1(x) Integral_0^x PDF_l2(y) dy dx
	//               = Integral_0^inf PDF_l1(x) CDF_l2(x) dx
	//               = Integral_0^inf PDF_l1(x) (1 - exp(-l2 * x)) dx
	//               = 1 - Integral_0^inf l1 * exp(-(l1+l2) * x) dx
	//               = 1 - l1 / (l1 + l2) * Integral_0^inf PDF_(l1+l2)(x) dx
	//               = 1 - l1 / (l1 + l2)
	//               = l2 / (l1 + l2)
	//
	// We want this probability to be p1 / (p1 + p2) which we can get by setting
	//    l1 = 1 / p1
	//    l2 = 1 / p2
	// It's easy to verify that (1/p2) / (1/p1 + 1/p2) = p1 / (p2 + p1).
	//
	// We can generate an exponentially distributed value using (rand.ExpFloat64() / lambda).
	// In our case this works out to simply rand.ExpFloat64() * userPriority.
	val := rand.ExpFloat64() * float64(userPriority)

	// To convert to an integer, we scale things to accommodate a few (5) standard deviations for
	// the maximum priority. The choice of the value is a trade-off between loss of resolution for
	// low priorities and overflow (capping the value to MaxInt32) for high priorities.
	//
	// For userPriority=MaxUserPriority, the probability of overflow is 0.7%.
	// For userPriority=(MaxUserPriority/2), the probability of overflow is 0.005%.
	val = (val / (5 * float64(MaxUserPriority))) * math.MaxInt32
	if val < float64(enginepb.MinTxnPriority+1) {
		return enginepb.MinTxnPriority + 1
	} else if val > float64(enginepb.MaxTxnPriority-1) {
		return enginepb.MaxTxnPriority - 1
	}
	return enginepb.TxnPriority(val)
}

// Restart reconfigures a transaction for restart. The epoch is
// incremented for an in-place restart. The timestamp of the
// transaction on restart is set to the maximum of the transaction's
// timestamp and the specified timestamp.
func (t *Transaction) Restart(
	userPriority UserPriority, upgradePriority enginepb.TxnPriority, timestamp hlc.Timestamp,
) {
	t.BumpEpoch()
	if t.WriteTimestamp.Less(timestamp) {
		t.WriteTimestamp = timestamp
	}
	t.ReadTimestamp = t.WriteTimestamp
	// Upgrade priority to the maximum of:
	// - the current transaction priority
	// - a random priority created from userPriority
	// - the conflicting transaction's upgradePriority
	t.UpgradePriority(MakePriority(userPriority))
	t.UpgradePriority(upgradePriority)
	// Reset all epoch-scoped state.
	t.Sequence = 0
	t.ReadTimestampFixed = false
	t.LockSpans = nil
	t.InFlightWrites = nil
	t.IgnoredSeqNums = nil
}

// BumpEpoch increments the transaction's epoch, allowing for an in-place
// restart. This invalidates all write intents previously written at lower
// epochs.
func (t *Transaction) BumpEpoch() {
	t.Epoch++
}

// BumpReadTimestamp forwards the transaction's read timestamp to the provided
// timestamp. It also forwards its write timestamp, if necessary, to ensure that
// its write timestamp is always greater than or equal to its read timestamp.
//
// A transaction's write timestamp serves as a lower bound on its commit
// timestamp. It is free to advance over the course of the transaction's
// lifetime when experiencing read-write or write-write contention. The write
// timestamp can advance without restraint or prior validation.
//
// A transaction's read timestamp establishes the consistent read snapshot that
// the transaction observes when reading data. Unlike the write timestamp, the
// read timestamp is not free to advance, except in specific circumstances.
// Movement of the read timestamp outside these circumstances would break the
// consistent view of data that the transaction is meaning to provide. The read
// can be advanced in three situations:
//   - When a transaction restarts and moves to a new epoch. At this time, the
//     reads and writes performed in the prior epoch(s) are considered invalid
//     and the read snapshot can be re-established using a new read timestamp.
//   - When a transaction performs a read refresh, having validated that all
//     prior reads are equivalent at the old and new read timestamp. For details
//     about transaction read refreshes, see the comment on txnSpanRefresher.
//   - When the transaction reaches a statement boundary, if the transaction's
//     isolation level permits it to observe a different read snapshot on each
//     statement. For more, see the comment on isolation.Level.
func (t *Transaction) BumpReadTimestamp(timestamp hlc.Timestamp) {
	t.ReadTimestamp.Forward(timestamp)
	t.WriteTimestamp.Forward(t.ReadTimestamp)
}

// Update ratchets priority, timestamp and original timestamp values (among
// others) for the transaction. If t.ID is empty, then the transaction is
// copied from o.
func (t *Transaction) Update(o *Transaction) {
	ctx := context.TODO()
	if o == nil {
		return
	}
	o.AssertInitialized(ctx)
	if t.ID == (uuid.UUID{}) {
		*t = *o
		return
	} else if t.ID != o.ID {
		log.Fatalf(ctx, "updating txn %s with different txn %s", t.String(), o.String())
		return
	}
	if len(t.Key) == 0 {
		t.Key = o.Key
	}
	t.IsoLevel = o.IsoLevel

	// Update epoch-scoped state, depending on the two transactions' epochs.
	if t.Epoch < o.Epoch {
		// Ensure that the transaction status makes sense. If the transaction
		// has already been finalized, then it should remain finalized.
		if !t.Status.IsFinalized() {
			t.Status = o.Status
		} else if t.Status == COMMITTED {
			log.Warningf(ctx, "updating COMMITTED txn %s with txn at later epoch %s", t.String(), o.String())
		}
		// Replace all epoch-scoped state.
		t.Epoch = o.Epoch
		t.ReadTimestampFixed = o.ReadTimestampFixed
		t.Sequence = o.Sequence
		t.LockSpans = o.LockSpans
		t.InFlightWrites = o.InFlightWrites
		t.IgnoredSeqNums = o.IgnoredSeqNums
	} else if t.Epoch == o.Epoch {
		// Forward all epoch-scoped state.
		switch t.Status {
		case PENDING:
			t.Status = o.Status
		case PREPARED:
			if o.Status != PENDING {
				t.Status = o.Status
			}
		case STAGING:
			if o.Status != PENDING {
				t.Status = o.Status
			}
		case ABORTED:
			if o.Status == COMMITTED {
				log.Warningf(ctx, "updating ABORTED txn %s with COMMITTED txn %s", t.String(), o.String())
			}
		case COMMITTED:
			// Nothing to do.
		default:
			log.Fatalf(ctx, "unexpected txn status: %s", t.Status)
		}

		if t.ReadTimestamp == o.ReadTimestamp {
			t.ReadTimestampFixed = t.ReadTimestampFixed || o.ReadTimestampFixed
		} else if t.ReadTimestamp.Less(o.ReadTimestamp) {
			t.ReadTimestampFixed = o.ReadTimestampFixed
		}

		if t.Sequence < o.Sequence {
			t.Sequence = o.Sequence
		}
		if len(o.LockSpans) > 0 {
			t.LockSpans = o.LockSpans
		}
		if len(o.InFlightWrites) > 0 {
			t.InFlightWrites = o.InFlightWrites
		}
		if len(o.IgnoredSeqNums) > 0 {
			t.IgnoredSeqNums = o.IgnoredSeqNums
		}
	} else /* t.Epoch > o.Epoch */ {
		// Ignore epoch-specific state from previous epoch. However, ensure that
		// the transaction status still makes sense.
		switch o.Status {
		case ABORTED:
			// Once aborted, always aborted. The transaction coordinator might
			// have incremented the txn's epoch without realizing that it was
			// aborted.
			t.Status = ABORTED
		case PREPARED, COMMITTED:
			log.Warningf(ctx, "updating txn %s with %s txn at earlier epoch %s", t.String(), o.Status, o.String())
		}
	}

	// Forward each of the transaction timestamps.
	t.WriteTimestamp.Forward(o.WriteTimestamp)
	t.LastHeartbeat.Forward(o.LastHeartbeat)
	t.GlobalUncertaintyLimit.Forward(o.GlobalUncertaintyLimit)
	t.ReadTimestamp.Forward(o.ReadTimestamp)

	// On update, set lower bound timestamps to the minimum seen by either txn.
	// These shouldn't differ unless one of them is empty, but we're careful
	// anyway.
	if t.MinTimestamp.IsEmpty() {
		t.MinTimestamp = o.MinTimestamp
	} else if !o.MinTimestamp.IsEmpty() {
		t.MinTimestamp.Backward(o.MinTimestamp)
	}

	// Absorb the collected clock uncertainty information.
	for _, v := range o.ObservedTimestamps {
		t.UpdateObservedTimestamp(v.NodeID, v.Timestamp)
	}

	// Ratchet the transaction priority.
	t.UpgradePriority(o.Priority)

	// The following fields are not present in TransactionRecord, so we need to be
	// careful when updating them since Transaction o might be coming from a
	// TransactionRecord. If the fields were previously set, do not overwrite them
	// with the default values. Conversely, if the fields were previously unset,
	// allow updating them to handle the case when a Transaction proto updates a
	// TransactionRecord proto.

	// AdmissionPriority doesn't change after the transaction is created, so we
	// don't ever expect to change it from a non-zero value to 0.
	if o.AdmissionPriority != 0 {
		t.AdmissionPriority = o.AdmissionPriority
	}
	// OmitInRangefeeds doesn't change after the transaction is created, so we
	// don't ever expect to change it from true to false.
	if o.OmitInRangefeeds {
		t.OmitInRangefeeds = o.OmitInRangefeeds
	}
}

// UpgradePriority sets transaction priority to the maximum of current
// priority and the specified minPriority. The exception is if the
// current priority is set to the minimum, in which case the minimum
// is preserved.
func (t *Transaction) UpgradePriority(minPriority enginepb.TxnPriority) {
	if minPriority > t.Priority && t.Priority != enginepb.MinTxnPriority {
		t.Priority = minPriority
	}
}

// IsLocking returns whether the transaction has begun acquiring locks.
// This method will never return false for a writing transaction.
func (t *Transaction) IsLocking() bool {
	return t.Key != nil
}

// LocksAsLockUpdates turns t.LockSpans into a bunch of LockUpdates.
func (t *Transaction) LocksAsLockUpdates() []LockUpdate {
	ret := make([]LockUpdate, len(t.LockSpans))
	for i, sp := range t.LockSpans {
		ret[i] = MakeLockUpdate(t, sp)
	}
	return ret
}

// String formats transaction into human readable string.
func (t Transaction) String() string {
	return redact.StringWithoutMarkers(t)
}

// SafeFormat implements the redact.SafeFormatter interface.
func (t Transaction) SafeFormat(w redact.SafePrinter, _ rune) {
	if len(t.Name) > 0 {
		w.Printf("%q ", redact.SafeString(t.Name))
	}
	w.Printf("meta={%s} lock=%t stat=%s rts=%s gul=%s",
		t.TxnMeta, t.IsLocking(), t.Status, t.ReadTimestamp, t.GlobalUncertaintyLimit)

	// Print observed timestamps (limited to 5 for readability).
	if obsCount := len(t.ObservedTimestamps); obsCount > 0 {
		w.Printf(" obs={")
		limit := obsCount
		if limit > 5 {
			limit = 5
		}

		for i := 0; i < limit; i++ {
			if i > 0 {
				w.Printf(" ")
			}
			obs := t.ObservedTimestamps[i]
			w.Printf("n%d@%s", obs.NodeID, obs.Timestamp)
		}

		if obsCount > 5 {
			w.Printf(", ...")
		}
		w.Printf("}")
	}

	if ni := len(t.LockSpans); t.Status != PENDING && ni > 0 {
		w.Printf(" int=%d", ni)
	}
	if nw := len(t.InFlightWrites); t.Status != PENDING && nw > 0 {
		w.Printf(" ifw=%d", nw)
	}
	if ni := len(t.IgnoredSeqNums); ni > 0 {
		w.Printf(" isn=%d", ni)
	}
}

// ResetObservedTimestamps clears out all timestamps recorded from individual
// nodes.
func (t *Transaction) ResetObservedTimestamps() {
	t.ObservedTimestamps = nil
}

// UpdateObservedTimestamp stores a timestamp off a node's clock for future
// operations in the transaction. When multiple calls are made for a single
// nodeID, the lowest timestamp prevails.
func (t *Transaction) UpdateObservedTimestamp(nodeID NodeID, timestamp hlc.ClockTimestamp) {
	// Fast path optimization for either no observed timestamps or
	// exactly one, for the same nodeID as we're updating.
	if l := len(t.ObservedTimestamps); l == 0 {
		t.ObservedTimestamps = []ObservedTimestamp{{NodeID: nodeID, Timestamp: timestamp}}
		return
	} else if l == 1 && t.ObservedTimestamps[0].NodeID == nodeID {
		if timestamp.Less(t.ObservedTimestamps[0].Timestamp) {
			t.ObservedTimestamps = []ObservedTimestamp{{NodeID: nodeID, Timestamp: timestamp}}
		}
		return
	}
	s := observedTimestampSlice(t.ObservedTimestamps)
	t.ObservedTimestamps = s.update(nodeID, timestamp)
}

// GetObservedTimestamp returns the lowest HLC timestamp recorded from the given
// node's clock during the transaction. The returned boolean is false if no
// observation about the requested node was found. Otherwise, the transaction's
// uncertainty limit can be lowered to the returned timestamp when reading from
// nodeID.
func (t *Transaction) GetObservedTimestamp(nodeID NodeID) (hlc.ClockTimestamp, bool) {
	s := observedTimestampSlice(t.ObservedTimestamps)
	return s.get(nodeID)
}

// AddIgnoredSeqNumRange adds the given range to the given list of
// ignored seqnum ranges. Since none of the references held by a Transaction
// allow interior mutations, the existing list is copied instead of being
// mutated in place.
//
// See enginepb.TxnSeqListAppend for more details.
func (t *Transaction) AddIgnoredSeqNumRange(newRange enginepb.IgnoredSeqNumRange) {
	t.IgnoredSeqNums = enginepb.TxnSeqListAppend(t.IgnoredSeqNums, newRange)
}

// AsRecord returns a TransactionRecord object containing only the subset of
// fields from the receiver that must be persisted in the transaction record.
func (t *Transaction) AsRecord() TransactionRecord {
	var tr TransactionRecord
	tr.TxnMeta = t.TxnMeta
	tr.Status = t.Status
	tr.LastHeartbeat = t.LastHeartbeat
	tr.LockSpans = t.LockSpans
	tr.InFlightWrites = t.InFlightWrites
	tr.IgnoredSeqNums = t.IgnoredSeqNums
	return tr
}

// AsTransaction returns a Transaction object containing populated fields for
// state in the transaction record and empty fields for state omitted from the
// transaction record.
func (tr *TransactionRecord) AsTransaction() Transaction {
	var t Transaction
	t.TxnMeta = tr.TxnMeta
	t.Status = tr.Status
	t.LastHeartbeat = tr.LastHeartbeat
	t.LockSpans = tr.LockSpans
	t.InFlightWrites = tr.InFlightWrites
	t.IgnoredSeqNums = tr.IgnoredSeqNums
	return t
}

// Replicas returns all of the replicas present in the descriptor after this
// trigger applies.
func (crt ChangeReplicasTrigger) Replicas() []ReplicaDescriptor {
	return crt.Desc.Replicas().Descriptors()
}

// NextReplicaID returns the next replica id to use after this trigger applies.
func (crt ChangeReplicasTrigger) NextReplicaID() ReplicaID {
	return crt.Desc.NextReplicaID
}

// ConfChange returns the configuration change described by the trigger.
func (crt ChangeReplicasTrigger) ConfChange(encodedCtx []byte) (raftpb.ConfChangeI, error) {
	return confChangeImpl(crt, encodedCtx)
}

func (crt ChangeReplicasTrigger) alwaysV2() bool {
	// NB: we can return true in 20.1, but we don't win anything unless
	// we are actively trying to migrate out of V1 membership changes, which
	// could modestly simplify small areas of our codebase.
	return false
}

// confChangeImpl is the implementation of (ChangeReplicasTrigger).ConfChange
// narrowed down to the inputs it actually needs for better testability.
func confChangeImpl(
	crt interface {
		Added() []ReplicaDescriptor
		Removed() []ReplicaDescriptor
		Replicas() []ReplicaDescriptor
		alwaysV2() bool
	},
	encodedCtx []byte,
) (raftpb.ConfChangeI, error) {
	added, removed, replicas := crt.Added(), crt.Removed(), crt.Replicas()

	var sl []raftpb.ConfChangeSingle

	checkExists := func(in ReplicaDescriptor) error {
		for _, rDesc := range replicas {
			if rDesc.ReplicaID == in.ReplicaID {
				if in.Type != rDesc.Type {
					return errors.Errorf("have %s, but descriptor has %s", in, rDesc)
				}
				return nil
			}
		}
		return errors.Errorf("%s missing from descriptors %v", in, replicas)
	}
	checkNotExists := func(in ReplicaDescriptor) error {
		for _, rDesc := range replicas {
			if rDesc.ReplicaID == in.ReplicaID {
				return errors.Errorf("%s must no longer be present in descriptor", in)
			}
		}
		return nil
	}

	for _, rDesc := range removed {
		sl = append(sl, raftpb.ConfChangeSingle{
			Type:   raftpb.ConfChangeRemoveNode,
			NodeID: raftpb.PeerID(rDesc.ReplicaID),
		})

		switch rDesc.Type {
		case VOTER_DEMOTING_LEARNER, VOTER_DEMOTING_NON_VOTER:
			// If a voter is demoted through joint consensus, it will
			// be turned into a demoting voter first.
			if err := checkExists(rDesc); err != nil {
				return nil, err
			}
			// It's being re-added as a learner, not only removed.
			sl = append(sl, raftpb.ConfChangeSingle{
				Type:   raftpb.ConfChangeAddLearnerNode,
				NodeID: raftpb.PeerID(rDesc.ReplicaID),
			})
		case LEARNER:
			// A learner could in theory show up in the descriptor if the removal was
			// really a demotion and no joint consensus is used. But etcd/raft
			// currently forces us to go through joint consensus when demoting, so
			// demotions will always have a VOTER_DEMOTING_LEARNER instead. We must be
			// straight-up removing a voter or learner, so the target should be gone
			// from the descriptor at this point.
			if err := checkNotExists(rDesc); err != nil {
				return nil, err
			}
		case NON_VOTER:
			// Like the case above, we must be removing a non-voter, so the target
			// should be gone from the descriptor.
			if err := checkNotExists(rDesc); err != nil {
				return nil, err
			}
		default:
			return nil, errors.Errorf("removal of %v unsafe, demote to LEARNER first", rDesc.Type)
		}
	}

	for _, rDesc := range added {
		// The incoming descriptor must also be present in the set of all
		// replicas, which is ultimately the authoritative one because that's
		// what's written to the KV store.
		if err := checkExists(rDesc); err != nil {
			return nil, err
		}

		var changeType raftpb.ConfChangeType
		switch rDesc.Type {
		case VOTER_FULL:
			// We're adding a new voter.
			changeType = raftpb.ConfChangeAddNode
		case VOTER_INCOMING:
			// We're adding a voter, but will transition into a joint config
			// first.
			changeType = raftpb.ConfChangeAddNode
		case LEARNER, NON_VOTER:
			// We're adding a learner or non-voter.
			// Note that we're guaranteed by virtue of the upstream ChangeReplicas txn
			// that this learner/non-voter is not currently a voter. Demotions (i.e.
			// transitioning from voter to learner/non-voter) are not represented in
			// `added`; they're handled in `removed` above.
			changeType = raftpb.ConfChangeAddLearnerNode
		default:
			// A voter that is demoting was just removed and re-added in the
			// `removals` handler. We should not see it again here.
			// A voter that's outgoing similarly has no reason to show up here.
			return nil, errors.Errorf("can't add replica in state %v", rDesc.Type)
		}
		sl = append(sl, raftpb.ConfChangeSingle{
			Type:   changeType,
			NodeID: raftpb.PeerID(rDesc.ReplicaID),
		})
	}

	// Check whether we're entering a joint state. This is the case precisely when
	// the resulting descriptors tells us that this is the case. Note that we've
	// made sure above that all of the additions/removals are in tune with that
	// descriptor already.
	var enteringJoint bool
	for _, rDesc := range replicas {
		switch rDesc.Type {
		case VOTER_INCOMING, VOTER_OUTGOING, VOTER_DEMOTING_LEARNER, VOTER_DEMOTING_NON_VOTER:
			enteringJoint = true
		default:
		}
	}
	wantLeaveJoint := len(added)+len(removed) == 0
	if !enteringJoint {
		if len(added)+len(removed) > 1 {
			return nil, errors.Errorf("change requires joint consensus")
		}
	} else if wantLeaveJoint {
		return nil, errors.Errorf("descriptor enters joint state, but trigger is requesting to leave one")
	}

	var cc raftpb.ConfChangeI

	if enteringJoint || crt.alwaysV2() {
		// V2 membership changes, which allow atomic replication changes. We
		// track the joint state in the range descriptor and thus we need to be
		// in charge of when to leave the joint state.
		transition := raftpb.ConfChangeTransitionJointExplicit
		if !enteringJoint {
			// If we're using V2 just to avoid V1 (and not because we actually
			// have a change that requires V2), then use an auto transition
			// which skips the joint state. This is necessary: our descriptor
			// says we're not supposed to go through one.
			transition = raftpb.ConfChangeTransitionAuto
		}
		cc = raftpb.ConfChangeV2{
			Transition: transition,
			Changes:    sl,
			Context:    encodedCtx,
		}
	} else if wantLeaveJoint {
		// Transitioning out of a joint config.
		cc = raftpb.ConfChangeV2{
			Context: encodedCtx,
		}
	} else {
		// Legacy path with exactly one change.
		cc = raftpb.ConfChange{
			Type:    sl[0].Type,
			NodeID:  sl[0].NodeID,
			Context: encodedCtx,
		}
	}
	return cc, nil
}

var _ fmt.Stringer = &ChangeReplicasTrigger{}

func (crt ChangeReplicasTrigger) String() string {
	return redact.StringWithoutMarkers(crt)
}

// SafeFormat implements the redact.SafeFormatter interface.
func (crt ChangeReplicasTrigger) SafeFormat(w redact.SafePrinter, _ rune) {
	var nextReplicaID ReplicaID
	var afterReplicas []ReplicaDescriptor
	added, removed := crt.Added(), crt.Removed()
	nextReplicaID = crt.Desc.NextReplicaID
	// NB: we don't want to mutate InternalReplicas, so we don't call
	// .Replicas()
	//
	// TODO(tbg): revisit after #39489 is merged.
	afterReplicas = crt.Desc.InternalReplicas
	cc, err := crt.ConfChange(nil)
	if err != nil {
		w.Printf("<malformed ChangeReplicasTrigger: %s>", err)
	} else {
		ccv2 := cc.AsV2()
		if ccv2.LeaveJoint() {
			// NB: this isn't missing a trailing space.
			//
			// TODO(tbg): could list the replicas that will actually leave the
			// voter set.
			w.SafeString("LEAVE_JOINT")
		} else if _, ok := ccv2.EnterJoint(); ok {
			w.Printf("ENTER_JOINT(%s) ", confChangesToRedactableString(ccv2.Changes))
		} else {
			w.Printf("SIMPLE(%s) ", confChangesToRedactableString(ccv2.Changes))
		}
	}
	if len(added) > 0 {
		w.Printf("%s", added)
	}
	if len(removed) > 0 {
		if len(added) > 0 {
			w.SafeString(", ")
		}
		w.Printf("%s", removed)
	}
	w.Printf(": after=%s next=%d", afterReplicas, nextReplicaID)
}

// confChangesToRedactableString produces a safe representation for
// the configuration changes.
func confChangesToRedactableString(ccs []raftpb.ConfChangeSingle) redact.RedactableString {
	return redact.Sprintfn(func(w redact.SafePrinter) {
		for i, cc := range ccs {
			if i > 0 {
				w.SafeRune(' ')
			}
			switch cc.Type {
			case raftpb.ConfChangeAddNode:
				w.SafeRune('v')
			case raftpb.ConfChangeAddLearnerNode:
				w.SafeRune('l')
			case raftpb.ConfChangeRemoveNode:
				w.SafeRune('r')
			case raftpb.ConfChangeUpdateNode:
				w.SafeRune('u')
			default:
				w.SafeString("unknown")
			}
			w.Print(cc.NodeID)
		}
	})
}

// Added returns the replicas added by this change (if there are any).
func (crt ChangeReplicasTrigger) Added() []ReplicaDescriptor {
	return crt.InternalAddedReplicas
}

// Removed returns the replicas whose removal is initiated by this change (if there are any).
// Note that in an atomic replication change, Removed() contains the replicas when they are
// transitioning to VOTER_{OUTGOING,DEMOTING} (from VOTER_FULL). The subsequent trigger
// leaving the joint configuration has an empty Removed().
func (crt ChangeReplicasTrigger) Removed() []ReplicaDescriptor {
	return crt.InternalRemovedReplicas
}

// LeaseSequence is a custom type for a lease sequence number.
type LeaseSequence uint64

// SafeValue implements the redact.SafeValue interface.
func (s LeaseSequence) SafeValue() {}

// SafeValue implements the redact.SafeValue interface.
func (LeaseAcquisitionType) SafeValue() {}

var _ fmt.Stringer = &Lease{}

func (l Lease) String() string {
	return redact.StringWithoutMarkers(l)
}

// SafeFormat implements the redact.SafeFormatter interface.
func (l Lease) SafeFormat(w redact.SafePrinter, _ rune) {
	if l.Empty() {
		w.SafeString("<empty>")
		return
	}
	w.Printf("repl=%s seq=%d start=%s", l.Replica, l.Sequence, l.Start)
	switch l.Type() {
	case LeaseExpiration:
		w.Printf(" exp=%s", l.Expiration)
	case LeaseEpoch:
		w.Printf(" epo=%d min-exp=%s", l.Epoch, l.MinExpiration)
	case LeaseLeader:
		w.Printf(" term=%d min-exp=%s", l.Term, l.MinExpiration)
	default:
		panic("unexpected lease type")
	}
	w.Printf(" pro=%s acq=%s", l.ProposedTS, l.AcquisitionType)
}

// Empty returns true for the Lease zero-value.
func (l *Lease) Empty() bool {
	return l == nil || *l == (Lease{})
}

// OwnedBy returns whether the given store is the lease owner.
func (l Lease) OwnedBy(storeID StoreID) bool {
	return l.Replica.StoreID == storeID
}

// LeaseType describes the type of lease.
//
//go:generate stringer -type=LeaseType
type LeaseType int32

const (
	// LeaseNone specifies no lease, to be used as a default value.
	LeaseNone LeaseType = iota
	// LeaseExpiration allows range operations while the wall clock is
	// within the expiration timestamp.
	LeaseExpiration
	// LeaseEpoch allows range operations while the node liveness epoch
	// is equal to the lease epoch.
	LeaseEpoch
	// LeaseLeader allows range operations while the replica is guaranteed
	// to be the range's raft leader.
	LeaseLeader
)

// TestingAllLeaseTypes returns a list of all lease types to test against.
func TestingAllLeaseTypes() []LeaseType {
	if syncutil.DeadlockEnabled {
		// Skip expiration-based leases under deadlock since it could overload the
		// testing cluster.
		return []LeaseType{LeaseEpoch, LeaseLeader}
	}
	return []LeaseType{LeaseExpiration, LeaseEpoch, LeaseLeader}
}

// EpochAndLeaderLeaseType returns a list of {epcoh, leader} lease types.
func EpochAndLeaderLeaseType() []LeaseType {
	return []LeaseType{LeaseEpoch, LeaseLeader}
}

// ExpirationAndLeaderLeaseType returns a list of {expiration, leader} lease
// types.
func ExpirationAndLeaderLeaseType() []LeaseType {
	return []LeaseType{LeaseExpiration, LeaseLeader}
}

// Type returns the lease type.
func (l Lease) Type() LeaseType {
	if l.Epoch != 0 && l.Term != 0 {
		panic("lease cannot have both epoch and term")
	}
	if l.Epoch != 0 {
		return LeaseEpoch
	}
	if l.Term != 0 {
		return LeaseLeader
	}
	return LeaseExpiration
}

// SupportsQuiescence returns whether the lease supports quiescence or not.
func (l Lease) SupportsQuiescence() bool {
	switch l.Type() {
	case LeaseExpiration, LeaseLeader:
		// Expiration based leases do not support quiescence because they'll likely
		// be renewed soon, so there's not much point to it.
		//
		// Leader leases use the similar but separate concept of sleep to indicate
		// that followers should stop ticking.
		return false
	case LeaseEpoch:
		return true
	default:
		panic("unexpected lease type")
	}
}

// SupportsSleep returns whether the lease supports replica sleep or not.
func (l Lease) SupportsSleep() bool {
	switch l.Type() {
	case LeaseExpiration, LeaseEpoch:
		// Expiration based leases do not support sleep because they'll likely be
		// renewed soon, so there's not much point to it.
		//
		// Epoch leases use the similar but separate concept of quiescence to
		// indicate that replicas should stop ticking.
		return false
	case LeaseLeader:
		return true
	default:
		panic("unexpected lease type")
	}
}

// Speculative returns true if this lease instance doesn't correspond to a
// committed lease (or at least to a lease that's *known* to have committed).
// For example, nodes sometimes guess who a leaseholder might be and synthesize
// a more or less complete lease struct. Such cases are identified by an empty
// Sequence.
func (l Lease) Speculative() bool {
	return l.Sequence == 0
}

// Equivalent determines whether the old lease (l) is considered the same as
// the new lease (newL) for the purposes of matching leases when executing a
// command.
//
// For expiration-based leases, extensions are allowed.
// Ignore proposed timestamps for lease verification; for epoch-
// based leases, the start time of the lease is sufficient to
// avoid using an older lease with same epoch.
//
// expToEpochEquiv indicates whether an expiration-based lease
// can be considered equivalent to an epoch-based lease during
// a promotion from expiration-based to epoch-based. It is used
// for mixed-version compatibility. No such flag is needed for
// expiration-based to leader lease promotion, because there is
// no need for mixed-version compatibility.
//
// NB: Lease.Equivalent is NOT symmetric. For expiration-based
// leases, a lease is equivalent to another with an equal or
// later expiration, but not an earlier expiration. Similarly,
// an expiration-based lease is equivalent to an epoch-based
// lease with the same replica and start time (representing a
// promotion from expiration-based to epoch-based), but the
// reverse is not true.
//
// One of the uses of Equivalent is in deciding what Sequence to assign to
// newL, so this method must not use the value of Sequence for equivalency.
//
// The Start time of the two leases is compared, and a necessary condition
// for equivalency is that they must be equal. So in the case where the
// caller is someone who is constructing a new lease proposal, it is the
// caller's responsibility to realize that the two leases *could* be
// equivalent, and adjust the start time to be the same. Even if the start
// times are the same, the leases could turn out to be non-equivalent -- in
// that case they will share a start time but not the sequence.
//
// NB: we do not allow transitions from epoch-based or leader leases to
// expiration-based leases to be equivalent. This was because both of the
// former lease types don't have an expiration in the lease, while the
// latter does. We can introduce safety violations by shortening the lease
// expiration if we allow this transition, since the new lease may not apply
// at the leaseholder until much after it applies at some other replica, so
// the leaseholder may continue acting as one based on an old lease, while
// the other replica has stepped up as leaseholder.
func (l Lease) Equivalent(newL Lease, expToEpochEquiv bool) bool {
	// Ignore proposed timestamp & deprecated start stasis.
	l.ProposedTS, newL.ProposedTS = hlc.ClockTimestamp{}, hlc.ClockTimestamp{}
	l.DeprecatedStartStasis, newL.DeprecatedStartStasis = nil, nil
	// Ignore sequence numbers, they are simply a reflection of the equivalency of
	// other fields. Also, newL may not have an initialized sequence number.
	l.Sequence, newL.Sequence = 0, 0
	// Ignore the acquisition type, as leases will always be extended via
	// RequestLease requests regardless of how a leaseholder first acquired its
	// lease.
	l.AcquisitionType, newL.AcquisitionType = 0, 0
	// Ignore the ReplicaDescriptor's type. This shouldn't affect lease
	// equivalency because Raft state shouldn't be factored into the state of a
	// Replica's lease. We don't expect a leaseholder to ever become a LEARNER
	// replica, but that also shouldn't prevent it from extending its lease.
	l.Replica.Type, newL.Replica.Type = 0, 0
	// If both leases are epoch-based, we must dereference the epochs
	// and then set to nil.
	switch l.Type() {
	case LeaseEpoch:
		// Ignore expirations. This seems benign but since we changed the
		// nullability of this field in the 1.2 cycle, it's crucial and
		// tested in TestLeaseEquivalence.
		l.Expiration, newL.Expiration = nil, nil

		if l.Epoch == newL.Epoch {
			l.Epoch, newL.Epoch = 0, 0
		}

		// For epoch-based leases, extensions to the minimum expiration are
		// considered equivalent.
		if l.MinExpiration.LessEq(newL.MinExpiration) {
			l.MinExpiration, newL.MinExpiration = hlc.Timestamp{}, hlc.Timestamp{}
		}

	case LeaseLeader:
		if l.Term == newL.Term {
			l.Term, newL.Term = 0, 0
		}
		// For leader leases, extensions to the minimum expiration are considered
		// equivalent.
		if l.MinExpiration.LessEq(newL.MinExpiration) {
			l.MinExpiration, newL.MinExpiration = hlc.Timestamp{}, hlc.Timestamp{}
		}

	case LeaseExpiration:
		switch newL.Type() {
		case LeaseEpoch:
			// An expiration-based lease being promoted to an epoch-based lease. This
			// transition occurs after a successful lease transfer if the setting
			// kv.transfer_expiration_leases_first.enabled is enabled.
			//
			// Expiration-based leases carry a local expiration timestamp. Epoch-based
			// leases store their expiration indirectly in NodeLiveness. We assume that
			// this promotion is only proposed if the liveness expiration is later than
			// previous expiration carried by the expiration-based lease. This is a
			// case where Equivalent is not commutative, as the reverse transition
			// (from epoch-based to expiration-based) requires a sequence increment.
			//
			// Ignore expiration, epoch, and min expiration. The remaining fields
			// which are compared are Replica and Start.
			if expToEpochEquiv {
				l.Expiration = nil
				newL.Epoch = 0
				newL.MinExpiration = hlc.Timestamp{}
			}

		case LeaseLeader:
			// An expiration-based lease being promoted to a leader lease. This
			// transition occurs after a successful lease transfer if the setting
			// kv.transfer_expiration_leases_first.enabled is enabled and leader
			// leases are in use.
			//
			// Expiration-based leases carry a local expiration timestamp. Leader
			// leases extend their expiration indirectly through the leadership
			// fortification protocol and associated Store Liveness heartbeats. We
			// assume that this promotion is only proposed if the leader support
			// expiration (and associated min expiration) is equal to or later than
			// previous expiration carried by the expiration-based lease. This is a
			// case where Equivalent is not commutative, as the reverse transition
			// (from leader lease to expiration-based) requires a sequence increment.
			//
			// Ignore expiration, term, and min expiration. The remaining fields
			// which are compared are Replica and Start.
			l.Expiration = nil
			newL.Term = 0
			newL.MinExpiration = hlc.Timestamp{}

		case LeaseExpiration:
			// See the comment above, though this field's nullability wasn't
			// changed. We nil it out for completeness only.
			l.Epoch, newL.Epoch = 0, 0

			// For expiration-based leases, extensions are considered equivalent.
			// This is one case where Equivalent is not commutative and, as such,
			// requires special handling beneath Raft (see checkForcedErr).
			if l.GetExpiration().LessEq(newL.GetExpiration()) {
				l.Expiration, newL.Expiration = nil, nil
			}

		default:
			panic("unexpected lease type")
		}

	default:
		panic("unexpected lease type")
	}
	return l == newL
}

// GetExpiration returns the lease expiration or the zero timestamp if the
// receiver is not an expiration-based lease.
func (l Lease) GetExpiration() hlc.Timestamp {
	if l.Expiration == nil {
		return hlc.Timestamp{}
	}
	return *l.Expiration
}

// equivalentTimestamps compares two timestamps for equality and also considers
// the nil timestamp equal to the zero timestamp.
func equivalentTimestamps(a, b *hlc.Timestamp) bool {
	if a == nil {
		if b == nil {
			return true
		}
		if b.IsEmpty() {
			return true
		}
	} else if b == nil {
		if a.IsEmpty() {
			return true
		}
	}
	return a.Equal(b)
}

// Equal implements the gogoproto Equal interface. This implementation is
// forked from the gogoproto generated code to allow l.Expiration == nil and
// l.Expiration == &hlc.Timestamp{} to compare equal. It also ignores
// DeprecatedStartStasis entirely to allow for its removal in a later release.
func (l *Lease) Equal(that interface{}) bool {
	if that == nil {
		return l == nil
	}

	that1, ok := that.(*Lease)
	if !ok {
		that2, ok := that.(Lease)
		if ok {
			that1 = &that2
		} else {
			panic(errors.AssertionFailedf("attempting to compare lease to %T", that))
		}
	}
	if that1 == nil {
		return l == nil
	} else if l == nil {
		return false
	}

	if !l.Start.Equal(&that1.Start) {
		return false
	}
	if !equivalentTimestamps(l.Expiration, that1.Expiration) {
		return false
	}
	if !l.Replica.Equal(&that1.Replica) {
		return false
	}
	if !l.ProposedTS.Equal(that1.ProposedTS) {
		return false
	}
	if l.Epoch != that1.Epoch {
		return false
	}
	if l.Sequence != that1.Sequence {
		return false
	}
	if l.AcquisitionType != that1.AcquisitionType {
		return false
	}
	if !l.MinExpiration.Equal(&that1.MinExpiration) {
		return false
	}
	if l.Term != that1.Term {
		return false
	}
	return true
}

// MakeLock makes a lock with the given txn, key, and strength.
// This is suitable for use when constructing a LockConflictError or
// WriteIntentError.
func MakeLock(txn *enginepb.TxnMeta, key Key, str lock.Strength) Lock {
	var l Lock
	l.Txn = *txn
	l.Key = key
	l.Strength = str
	return l
}

// Intent is an intent-strength lock. The type is a specialization of Lock and
// should be constructed using MakeIntent.
type Intent Lock

// MakeIntent makes an intent-strength lock with the given txn and key.
func MakeIntent(txn *enginepb.TxnMeta, key Key) Intent {
	return Intent(MakeLock(txn, key, lock.Intent))
}

// AsLock casts an Intent to a Lock.
func (i Intent) AsLock() Lock {
	return Lock(i)
}

// AsLockPtr casts a *Intent to a *Lock.
func (i *Intent) AsLockPtr() *Lock {
	return (*Lock)(i)
}

// AsLocks casts a slice of Intents to a slice of Locks.
func AsLocks(s []Intent) []Lock {
	return *(*[]Lock)(unsafe.Pointer(&s))
}

// AsIntents takes a transaction and a slice of keys and returns it as a slice
// of intent-strength locks.
func AsIntents(txn *enginepb.TxnMeta, keys []Key) []Intent {
	ret := make([]Intent, len(keys))
	for i := range keys {
		ret[i] = MakeIntent(txn, keys[i])
	}
	return ret
}

// MakeLockAcquisition makes a lock acquisition message from the given
// txn, key, durability level, and lock strength.
func MakeLockAcquisition(
	txn enginepb.TxnMeta,
	key Key,
	dur lock.Durability,
	str lock.Strength,
	ignoredSeqNums []enginepb.IgnoredSeqNumRange,
) LockAcquisition {
	return LockAcquisition{
		Span:           Span{Key: key},
		Txn:            txn,
		Durability:     dur,
		Strength:       str,
		IgnoredSeqNums: ignoredSeqNums,
	}
}

// Empty returns true if the lock acquisition is empty.
func (m *LockAcquisition) Empty() bool {
	return m.Span.Equal(Span{})
}

func (m LockAcquisition) SafeFormat(w redact.SafePrinter, _ rune) {
	w.Printf("{span=%v %v durability=%v strength=%v ignored=%v}",
		m.Span, m.Txn, m.Durability, m.Strength, m.IgnoredSeqNums)
}

// MakeLockUpdate makes a lock update from the given txn and span.
//
// See also txn.LocksAsLockUpdates().
func MakeLockUpdate(txn *Transaction, span Span) LockUpdate {
	u := LockUpdate{Span: span}
	u.SetTxn(txn)
	return u
}

// SetTxn updates the transaction details in the lock update.
func (u *LockUpdate) SetTxn(txn *Transaction) {
	u.Txn = txn.TxnMeta
	u.Status = txn.Status
	u.IgnoredSeqNums = txn.IgnoredSeqNums
}

// SafeFormat implements redact.SafeFormatter.
func (ls LockStateInfo) SafeFormat(w redact.SafePrinter, r rune) {
	expand := w.Flag('+')
	w.Printf("range_id=%d key=%s ", ls.RangeID, ls.Key)
	redactableLockHolder := redact.Sprint(nil)
	if ls.LockHolder != nil {
		if expand {
			redactableLockHolder = redact.Sprint(ls.LockHolder.ID)
		} else {
			redactableLockHolder = redact.Sprint(ls.LockHolder.Short())
		}
	}
	w.Printf("holder=%s ", redactableLockHolder)
	w.Printf("durability=%s ", ls.Durability)
	w.Printf("duration=%s", ls.HoldDuration)
	if len(ls.Waiters) > 0 {
		w.Printf("\n waiters:")

		for _, lw := range ls.Waiters {
			if expand {
				w.Printf("\n  %+v", lw)
			} else {
				w.Printf("\n  %s", lw)
			}
		}
	}
}

func (ls LockStateInfo) String() string {
	return redact.StringWithoutMarkers(ls)
}

// Clone returns a copy of the span.
func (s Span) Clone() Span {
	return Span{Key: s.Key.Clone(), EndKey: s.EndKey.Clone()}
}

// EqualValue is Equal.
//
// TODO(tbg): remove this passthrough.
func (s Span) EqualValue(o Span) bool {
	return s.Key.Equal(o.Key) && s.EndKey.Equal(o.EndKey)
}

// Equal compares two spans.
func (s Span) Equal(o Span) bool {
	return s.Key.Equal(o.Key) && s.EndKey.Equal(o.EndKey)
}

// ZeroLength returns true if the distance between the start and end key is 0.
func (s Span) ZeroLength() bool {
	return s.Key.Equal(s.EndKey)
}

// Clamp clamps span s's keys within the span defined in bounds.
func (s Span) Clamp(bounds Span) (Span, error) {
	start, err := s.Key.Clamp(bounds.Key, bounds.EndKey)
	if err != nil {
		return Span{}, err
	}
	end, err := s.EndKey.Clamp(bounds.Key, bounds.EndKey)
	if err != nil {
		return Span{}, err
	}
	return Span{
		Key:    start,
		EndKey: end,
	}, nil
}

// Overlaps returns true WLOG for span A and B iff:
//  1. Both spans contain one key (just the start key) and they are equal; or
//  2. The span with only one key is contained inside the other span; or
//  3. The end key of span A is strictly greater than the start key of span B
//     and the end key of span B is strictly greater than the start key of span
//     A.
func (s Span) Overlaps(o Span) bool {
	if !s.Valid() || !o.Valid() {
		return false
	}

	if len(s.EndKey) == 0 && len(o.EndKey) == 0 {
		return s.Key.Equal(o.Key)
	} else if len(s.EndKey) == 0 {
		return bytes.Compare(s.Key, o.Key) >= 0 && bytes.Compare(s.Key, o.EndKey) < 0
	} else if len(o.EndKey) == 0 {
		return bytes.Compare(o.Key, s.Key) >= 0 && bytes.Compare(o.Key, s.EndKey) < 0
	}
	return bytes.Compare(s.EndKey, o.Key) > 0 && bytes.Compare(s.Key, o.EndKey) < 0
}

// Intersect returns the intersection of the key space covered by the two spans.
// If there is no intersection between the two spans, an invalid span (see Valid)
// is returned.
func (s Span) Intersect(o Span) Span {
	// If two spans do not overlap, there is no intersection between them.
	if !s.Overlaps(o) {
		return Span{}
	}

	// An empty end key means this span contains a single key. Overlaps already
	// has special code for the single-key cases, so here we return whichever key
	// is the single key, if any. If they are both a single key, we know they are
	// equal anyway so the order doesn't matter.
	if len(s.EndKey) == 0 {
		return s
	}
	if len(o.EndKey) == 0 {
		return o
	}

	key := s.Key
	if key.Compare(o.Key) < 0 {
		key = o.Key
	}
	endKey := s.EndKey
	if endKey.Compare(o.EndKey) > 0 {
		endKey = o.EndKey
	}
	return Span{key, endKey}
}

// Combine creates a new span containing the full union of the key
// space covered by the two spans. This includes any key space not
// covered by either span, but between them if the spans are disjoint.
// Warning: using this method to combine local and non-local spans is
// not recommended and will result in potentially database-wide
// spans being returned. Use with caution.
func (s Span) Combine(o Span) Span {
	if !s.Valid() || !o.Valid() {
		return Span{}
	}

	min := s.Key
	max := s.Key
	if len(s.EndKey) > 0 {
		max = s.EndKey
	}
	if o.Key.Compare(min) < 0 {
		min = o.Key
	} else if o.Key.Compare(max) > 0 {
		max = o.Key
	}
	if len(o.EndKey) > 0 && o.EndKey.Compare(max) > 0 {
		max = o.EndKey
	}
	if min.Equal(max) {
		return Span{Key: min}
	} else if s.Key.Equal(max) || o.Key.Equal(max) {
		return Span{Key: min, EndKey: max.Next()}
	}
	return Span{Key: min, EndKey: max}
}

// Contains returns whether the receiver contains the given span.
func (s Span) Contains(o Span) bool {
	if !s.Valid() || !o.Valid() {
		return false
	}

	if len(s.EndKey) == 0 && len(o.EndKey) == 0 {
		return s.Key.Equal(o.Key)
	} else if len(s.EndKey) == 0 {
		return false
	} else if len(o.EndKey) == 0 {
		return bytes.Compare(o.Key, s.Key) >= 0 && bytes.Compare(o.Key, s.EndKey) < 0
	}
	return bytes.Compare(s.Key, o.Key) <= 0 && bytes.Compare(s.EndKey, o.EndKey) >= 0
}

// ContainsKey returns whether the span contains the given key.
func (s Span) ContainsKey(key Key) bool {
	return bytes.Compare(key, s.Key) >= 0 && bytes.Compare(key, s.EndKey) < 0
}

// CompareKey returns -1 if the key precedes the span start, 0 if its contained
// by the span and 1 if its after the end of the span.
func (s Span) CompareKey(key Key) int {
	if bytes.Compare(key, s.Key) >= 0 {
		if bytes.Compare(key, s.EndKey) < 0 {
			return 0
		}
		return 1
	}
	return -1
}

// ProperlyContainsKey returns whether the span properly contains the given key.
func (s Span) ProperlyContainsKey(key Key) bool {
	return bytes.Compare(key, s.Key) > 0 && bytes.Compare(key, s.EndKey) < 0
}

// AsRange returns the Span as an interval.Range.
func (s Span) AsRange() interval.Range {
	startKey := s.Key
	endKey := s.EndKey
	if len(endKey) == 0 {
		endKey = s.Key.Next()
		startKey = endKey[:len(startKey)]
	}
	return interval.Range{
		Start: interval.Comparable(startKey),
		End:   interval.Comparable(endKey),
	}
}

func (s Span) String() string {
	return redact.StringWithoutMarkers(s)
}

// SafeFormat implements the redact.SafeFormatter interface.
func (s Span) SafeFormat(w redact.SafePrinter, _ rune) {
	const maxChars = math.MaxInt32
	w.Print(PrettyPrintRange(s.Key, s.EndKey, maxChars))
}

// SplitOnKey returns two spans where the left span has EndKey and right span
// has start Key of the split key, respectively.
// If the split key lies outside the span, the original span is returned on the
// left (and right is an invalid span with empty keys).
func (s Span) SplitOnKey(key Key) (left Span, right Span) {
	// Cannot split on or before start key or on or after end key.
	if bytes.Compare(key, s.Key) <= 0 || bytes.Compare(key, s.EndKey) >= 0 {
		return s, Span{}
	}

	return Span{Key: s.Key, EndKey: key}, Span{Key: key, EndKey: s.EndKey}
}

// Valid returns whether or not the span is a "valid span".
// A valid span cannot have an empty start and end key and must satisfy either:
// 1. The end key is empty.
// 2. The start key is lexicographically-ordered before the end key.
func (s Span) Valid() bool {
	// s.Key can be empty if it is KeyMin.
	// Can't have both KeyMin start and end keys.
	if len(s.Key) == 0 && len(s.EndKey) == 0 {
		return false
	}

	if len(s.EndKey) == 0 {
		return true
	}

	if bytes.Compare(s.Key, s.EndKey) >= 0 {
		return false
	}

	return true
}

// SpanOverhead is the overhead of Span in bytes.
const SpanOverhead = int64(unsafe.Sizeof(Span{}))

// MemUsage returns the size of the Span in bytes for memory accounting
// purposes.
func (s Span) MemUsage() int64 {
	return SpanOverhead + int64(cap(s.Key)) + int64(cap(s.EndKey))
}

// Spans is a slice of spans.
type Spans []Span

// Implement sort.Interface.
func (a Spans) Len() int           { return len(a) }
func (a Spans) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Spans) Less(i, j int) bool { return a[i].Key.Compare(a[j].Key) < 0 }

// ContainsKey returns whether any of the spans in the set of spans contains
// the given key.
func (a Spans) ContainsKey(key Key) bool {
	for _, span := range a {
		if span.ContainsKey(key) {
			return true
		}
	}

	return false
}

// SpansOverhead is the overhead of Spans in bytes.
const SpansOverhead = int64(unsafe.Sizeof(Spans{}))

// MemUsageUpToLen returns the size of the Spans in bytes for memory accounting
// purposes. The method assumes that all spans in [len(a), cap(a)] range are
// empty and will panic in test builds when not.
func (a Spans) MemUsageUpToLen() int64 {
	if buildutil.CrdbTestBuild {
		l := len(a)
		aCap := a[:cap(a)]
		for _, s := range aCap[l:] {
			if len(s.Key) > 0 || len(s.EndKey) > 0 {
				panic(errors.AssertionFailedf(
					"spans are not empty past length: %v (len=%d, cap=%d)", aCap, l, cap(a)),
				)
			}
		}
	}
	size := SpansOverhead + int64(cap(a)-len(a))*SpanOverhead
	for i := range a {
		size += a[i].MemUsage()
	}
	return size
}

func (a Spans) String() string {
	var buf bytes.Buffer
	for i, span := range a {
		if i != 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(span.String())
	}
	return buf.String()
}

// BoundedString returns a stringified representation of Spans while adhering to
// the provided hint on the length (although not religiously). The following
// heuristics are used:
// - if there are no more than 6 spans, then all are printed,
// - otherwise, at least 3 "head" and at least 3 "tail" spans are always printed
//   - the bytes "budget" is consumed from the "head".
func (a Spans) BoundedString(bytesHint int) string {
	if len(a) <= 6 {
		return a.String()
	}
	var buf bytes.Buffer
	var i int
	headEndIdx, tailStartIdx := 2, len(a)-3
	for i = range a {
		if i != 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(a[i].String())
		if buf.Len() >= bytesHint && i >= headEndIdx && i+1 < tailStartIdx {
			// If the bytes budget has been consumed, and we've included at
			// least 3 spans from the "head", and we have more than 3 spans left
			// total, we stop iteration from the front.
			break
		}
	}
	if i+1 < len(a) {
		buf.WriteString(" ... ")
		for i = tailStartIdx; i < len(a); i++ {
			if i != tailStartIdx {
				buf.WriteString(", ")
			}
			buf.WriteString(a[i].String())
		}
	}
	return buf.String()
}

// RSpan is a key range with an inclusive start RKey and an exclusive end RKey.
type RSpan struct {
	Key, EndKey RKey
}

// KeySpan returns the Span with the StartKey forwarded to LocalMax, if necessary.
//
// See: https://github.com/cockroachdb/cockroach/issues/95055
func (rs RSpan) KeySpan() RSpan {
	start := rs.Key
	if start.Equal(RKeyMin) {
		// The first range in the keyspace is declared to start at KeyMin (the
		// lowest possible key). That is a lie, however, since the local key space
		// ([LocalMin,LocalMax)) doesn't belong to this range; it doesn't belong to
		// any range in particular.
		start = RKey(LocalMax)
	}
	return RSpan{
		Key:    start,
		EndKey: rs.EndKey,
	}
}

// Equal compares for equality.
func (rs RSpan) Equal(o RSpan) bool {
	return rs.Key.Equal(o.Key) && rs.EndKey.Equal(o.EndKey)
}

// ContainsKey returns whether this span contains the specified key.
func (rs RSpan) ContainsKey(key RKey) bool {
	return bytes.Compare(key, rs.Key) >= 0 && bytes.Compare(key, rs.EndKey) < 0
}

// ContainsKeyInverted returns whether this span contains the specified key. The
// receiver span is considered inverted, meaning that instead of containing the
// range ["key","endKey"), it contains the range ("key","endKey"].
func (rs RSpan) ContainsKeyInverted(key RKey) bool {
	return bytes.Compare(key, rs.Key) > 0 && bytes.Compare(key, rs.EndKey) <= 0
}

// ContainsKeyRange returns whether this span contains the specified
// key range from start (inclusive) to end (exclusive).
// If end is empty or start is equal to end, returns ContainsKey(start).
func (rs RSpan) ContainsKeyRange(start, end RKey) bool {
	if len(end) == 0 {
		return rs.ContainsKey(start)
	}
	if comp := bytes.Compare(end, start); comp < 0 {
		return false
	} else if comp == 0 {
		return rs.ContainsKey(start)
	}
	return bytes.Compare(start, rs.Key) >= 0 && bytes.Compare(rs.EndKey, end) >= 0
}

func (rs RSpan) String() string {
	return redact.StringWithoutMarkers(rs)
}

func (rs RSpan) SafeFormat(w redact.SafePrinter, r rune) {
	const maxChars = math.MaxInt32
	w.Print(PrettyPrintRange(Key(rs.Key), Key(rs.EndKey), maxChars))
}

// Intersect returns the intersection of the current span and the
// given range. Returns an error if the span and the range do not
// overlap.
func (rs RSpan) Intersect(rspan RSpan) (RSpan, error) {
	if !rs.Key.Less(rspan.EndKey) || !rspan.Key.Less(rs.EndKey) {
		return rs, errors.Errorf("spans do not overlap: %s vs %s", rs, rspan)
	}

	key := rs.Key
	if key.Less(rspan.Key) {
		key = rspan.Key
	}
	endKey := rs.EndKey
	if !rspan.ContainsKeyRange(rspan.Key, endKey) {
		endKey = rspan.EndKey
	}
	return RSpan{key, endKey}, nil
}

// AsRawSpanWithNoLocals returns the RSpan as a Span. This is to be used only
// in select situations in which an RSpan is known to not contain a wrapped
// locally-addressed Span.
func (rs RSpan) AsRawSpanWithNoLocals() Span {
	return Span{
		Key:    Key(rs.Key),
		EndKey: Key(rs.EndKey),
	}
}

// KeyValueByKey implements sorting of a slice of KeyValues by key.
type KeyValueByKey []KeyValue

// Len implements sort.Interface.
func (kv KeyValueByKey) Len() int {
	return len(kv)
}

// Less implements sort.Interface.
func (kv KeyValueByKey) Less(i, j int) bool {
	return bytes.Compare(kv[i].Key, kv[j].Key) < 0
}

// Swap implements sort.Interface.
func (kv KeyValueByKey) Swap(i, j int) {
	kv[i], kv[j] = kv[j], kv[i]
}

var _ sort.Interface = KeyValueByKey{}

// observedTimestampSlice maintains an immutable sorted list of observed
// timestamps.
type observedTimestampSlice []ObservedTimestamp

func (s observedTimestampSlice) index(nodeID NodeID) int {
	return sort.Search(len(s),
		func(i int) bool {
			return s[i].NodeID >= nodeID
		},
	)
}

// get the observed timestamp for the specified node, returning false if no
// timestamp exists.
func (s observedTimestampSlice) get(nodeID NodeID) (hlc.ClockTimestamp, bool) {
	i := s.index(nodeID)
	if i < len(s) && s[i].NodeID == nodeID {
		return s[i].Timestamp, true
	}
	return hlc.ClockTimestamp{}, false
}

// update the timestamp for the specified node, or add a new entry in the
// correct (sorted) location. The receiver is not mutated.
func (s observedTimestampSlice) update(
	nodeID NodeID, timestamp hlc.ClockTimestamp,
) observedTimestampSlice {
	i := s.index(nodeID)
	if i < len(s) && s[i].NodeID == nodeID {
		if timestamp.Less(s[i].Timestamp) {
			// The input slice is immutable, so copy and update.
			cpy := make(observedTimestampSlice, len(s))
			copy(cpy, s)
			cpy[i].Timestamp = timestamp
			return cpy
		}
		return s
	}
	// The input slice is immutable, so copy and update. Don't append to
	// avoid an allocation. Doing so could invalidate a previous update
	// to this receiver.
	cpy := make(observedTimestampSlice, len(s)+1)
	copy(cpy[:i], s[:i])
	cpy[i] = ObservedTimestamp{NodeID: nodeID, Timestamp: timestamp}
	copy(cpy[i+1:], s[i:])
	return cpy
}

// SequencedWriteBySeq implements sorting of a slice of SequencedWrites
// by sequence number.
type SequencedWriteBySeq []SequencedWrite

// Len implements sort.Interface.
func (s SequencedWriteBySeq) Len() int { return len(s) }

// Less implements sort.Interface.
func (s SequencedWriteBySeq) Less(i, j int) bool { return s[i].Sequence < s[j].Sequence }

// Swap implements sort.Interface.
func (s SequencedWriteBySeq) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

var _ sort.Interface = SequencedWriteBySeq{}

// Find searches for the index of the SequencedWrite with the provided
// sequence number. Returns -1 if no corresponding write is found.
func (s SequencedWriteBySeq) Find(seq enginepb.TxnSeq) int {
	if util.RaceEnabled {
		if !sort.IsSorted(s) {
			panic("SequencedWriteBySeq must be sorted")
		}
	}
	if i := sort.Search(len(s), func(i int) bool {
		return s[i].Sequence >= seq
	}); i < len(s) && s[i].Sequence == seq {
		return i
	}
	return -1
}

// Silence unused warning.
var _ = (SequencedWriteBySeq{}).Find

func init() {
	// Inject the format dependency into the enginepb package.
	enginepb.FormatBytesAsKey = func(k []byte) redact.RedactableString {
		return redact.Sprint(Key(k))
	}
}

// SafeValue implements the redact.SafeValue interface.
func (ReplicaChangeType) SafeValue() {}

func (ri RangeInfo) String() string {
	return fmt.Sprintf("desc: %s, lease: %s, closed_timestamp_policy: %s",
		ri.Desc, ri.Lease, ri.ClosedTimestampPolicy)
}

// Add adds another RowCount to the receiver.
func (r *RowCount) Add(other RowCount) {
	r.DataSize += other.DataSize
	r.Rows += other.Rows
	r.IndexEntries += other.IndexEntries
}

func (tid *TenantID) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var id uint64
	if err := unmarshal(&id); err == nil {
		tid.InternalValue = id
		return nil
	} else {
		return unmarshal(tid)
	}
}
