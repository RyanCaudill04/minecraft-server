package nbt_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt"
	"github.com/ryancaudill04/minecraft-server/web/internal/nbt/nbttest"
)

// sample uses every tag type.
var sample = map[string]any{
	"byte":   int8(-3),
	"short":  int16(300),
	"int":    int32(-70000),
	"long":   int64(1 << 40),
	"float":  float32(1.5),
	"double": float64(-2.25),
	"string": "héllo",
	"bytes":  []byte{1, 2, 3},
	"ints":   []int32{192, 76, -192},
	"longs":  []int64{-1, 1 << 50},
	"list":   []any{"a", "b"},
	"empty":  []any{},
	"nested": map[string]any{"inner": map[string]any{"n": int32(7)}},
	"objs":   []any{map[string]any{"x": int8(1)}, map[string]any{"x": int8(2)}},
}

func TestDecodeRoundTrip(t *testing.T) {
	for _, compress := range []bool{false, true} {
		got, err := nbt.Decode(bytes.NewReader(nbttest.Encode(sample, compress)))
		if err != nil {
			t.Fatalf("compress=%v: %v", compress, err)
		}
		if !reflect.DeepEqual(got, sample) {
			t.Errorf("compress=%v:\n got %#v\nwant %#v", compress, got, sample)
		}
	}
}

func TestReadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "level.dat")
	nbttest.WriteFile(t, path, map[string]any{"Data": map[string]any{"Time": int64(42)}})

	got, err := nbt.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := nbt.Int(got, "Data", "Time"); !ok || n != 42 {
		t.Errorf("Data.Time = %d, %v; want 42, true", n, ok)
	}

	if _, err := nbt.ReadFile(filepath.Join(t.TempDir(), "missing.dat")); err == nil {
		t.Error("ReadFile of a missing file: want an error")
	}
}

func TestHelpers(t *testing.T) {
	m := sample
	for _, key := range []string{"byte", "short", "int", "long"} {
		if _, ok := nbt.Int(m, key); !ok {
			t.Errorf("Int(%q) not ok", key)
		}
	}
	if n, ok := nbt.Int(m, "nested", "inner", "n"); !ok || n != 7 {
		t.Errorf("Int(nested.inner.n) = %d, %v; want 7, true", n, ok)
	}
	if _, ok := nbt.Int(m, "string"); ok {
		t.Error("Int of a string: want ok = false")
	}
	if _, ok := nbt.Int(m, "nested", "missing", "n"); ok {
		t.Error("Int of a missing path: want ok = false")
	}
	if s, ok := nbt.String(m, "string"); !ok || s != "héllo" {
		t.Errorf("String = %q, %v", s, ok)
	}
	if v := nbt.Get(m, "int", "deeper"); v != nil {
		t.Errorf("Get through a non-compound = %v, want nil", v)
	}
}

// header is a root compound with an empty name, ready for a body.
func header() *bytes.Buffer {
	return bytes.NewBuffer([]byte{10, 0, 0})
}

// arrayHeader is a document holding one array of the given tag, named "a",
// that claims n elements but has no data after the length.
func arrayHeader(tag byte, n int32) []byte {
	b := header()
	b.Write([]byte{tag, 0, 1, 'a'})
	binary.Write(b, binary.BigEndian, n)
	return b.Bytes()
}

func TestDecodeRejectsBadInput(t *testing.T) {
	valid := nbttest.Encode(sample, false)

	unknownTag := header()
	unknownTag.Write([]byte{99, 0, 1, 'a'})

	// Lists of lists, nested far deeper than any real file. The document is
	// complete, so only the depth limit can reject it.
	deepLists := header()
	deepLists.Write([]byte{nbt.TagList, 0, 1, 'a'})
	for range 1000 {
		deepLists.WriteByte(nbt.TagList) // element type
		binary.Write(deepLists, binary.BigEndian, int32(1))
	}
	deepLists.WriteByte(nbt.TagEnd) // the innermost list is empty
	binary.Write(deepLists, binary.BigEndian, int32(0))
	deepLists.WriteByte(nbt.TagEnd) // closes the root

	// The same with compounds inside compounds.
	deepCompounds := header()
	for range 1000 {
		deepCompounds.Write([]byte{nbt.TagCompound, 0, 1, 'a'})
	}
	for range 1001 { // every nested compound, then the root
		deepCompounds.WriteByte(nbt.TagEnd)
	}

	// Byte arrays adding up to more than maxSize, each within maxLength.
	big := map[string]any{}
	for i := range 20 {
		big[fmt.Sprint(i)] = make([]byte, 60000)
	}

	// Flip a bit in the gzip CRC, which sits after all the NBT data.
	badCRC := nbttest.Encode(sample, true)
	badCRC[len(badCRC)-8] ^= 1

	tests := []struct {
		name string
		data []byte
		want string // part of the error message
	}{
		{"empty", []byte{}, "nbt: EOF"},
		{"root not compound", []byte{8, 0, 0, 0, 1, 'x'}, "root tag is 8"},
		{"truncated", valid[:len(valid)/2], "unexpected EOF"},
		{"missing end", valid[:len(valid)-1], "unexpected EOF"},
		{"cut after root tag", []byte{10}, "unexpected EOF"},
		{"negative length", arrayHeader(nbt.TagIntArray, -1), "invalid length -1"},
		{"huge length", arrayHeader(nbt.TagByteArray, 1<<30), "invalid length 1073741824"},
		{"just over max length", arrayHeader(nbt.TagLongArray, 1<<16+1), "invalid length 65537"},
		{"unknown tag", unknownTag.Bytes(), "unknown tag type 99"},
		{"lists too deep", deepLists.Bytes(), "nesting too deep"},
		{"compounds too deep", deepCompounds.Bytes(), "nesting too deep"},
		{"too large", nbttest.Encode(big, false), "larger than"},
		{"too large gzipped", nbttest.Encode(big, true), "larger than"},
		{"bad gzip", []byte{0x1f, 0x8b, 0, 0, 0}, "nbt: "},
		{"bad gzip checksum", badCRC, "checksum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := nbt.Decode(bytes.NewReader(tt.data))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.HasPrefix(err.Error(), "nbt: ") {
				t.Errorf("error %q lacks the nbt: prefix", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A corrupt length must fail on the missing data without first allocating
// room for everything it claims.
func TestDecodeTruncatedArrayAllocatesLittle(t *testing.T) {
	for _, tt := range []struct {
		name string
		tag  byte
	}{
		{"byte array", nbt.TagByteArray},
		{"int array", nbt.TagIntArray},
		{"long array", nbt.TagLongArray},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := arrayHeader(tt.tag, 1<<16) // the most allowed: 512 KiB of longs
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := nbt.Decode(bytes.NewReader(data))
			runtime.ReadMemStats(&after)
			if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
				t.Errorf("error %v, want unexpected EOF", err)
			}
			if n := after.TotalAlloc - before.TotalAlloc; n > 32<<10 {
				t.Errorf("allocated %d bytes for an %d-byte input, want under 32 KiB", n, len(data))
			}
		})
	}
}

func TestDecodeLargeArrays(t *testing.T) {
	// Arrays spanning several read chunks, with lengths that don't divide
	// evenly into them.
	raw := make([]byte, 10001)
	ints := make([]int32, 3001)
	longs := make([]int64, 1501)
	for i := range raw {
		raw[i] = byte(i)
	}
	for i := range ints {
		ints[i] = int32(i) * -7
	}
	for i := range longs {
		longs[i] = int64(i) << 40
	}
	want := map[string]any{"b": raw, "i": ints, "l": longs}
	got, err := nbt.Decode(bytes.NewReader(nbttest.Encode(want, true)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("large arrays didn't round-trip")
	}
}

func TestReadFileErrorNamesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weather.dat")
	if err := os.WriteFile(path, []byte{10}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := nbt.ReadFile(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error %v, want it to name %s", err, path)
	}
}
