package nbt_test

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"reflect"
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

func TestDecodeRejectsBadInput(t *testing.T) {
	valid := nbttest.Encode(sample, false)

	negativeLength := header()
	negativeLength.Write([]byte{11, 0, 1, 'a'}) // Int Array named "a"
	binary.Write(negativeLength, binary.BigEndian, int32(-1))

	hugeLength := header()
	hugeLength.Write([]byte{7, 0, 1, 'a'}) // Byte Array named "a"
	binary.Write(hugeLength, binary.BigEndian, int32(1<<30))

	unknownTag := header()
	unknownTag.Write([]byte{99, 0, 1, 'a'})

	// Lists of lists, nested far deeper than any real file.
	deep := header()
	deep.Write([]byte{9, 0, 1, 'a'})
	for range 1000 {
		deep.WriteByte(9) // element type: list
		binary.Write(deep, binary.BigEndian, int32(1))
	}

	tests := map[string][]byte{
		"empty":             {},
		"root not compound": {8, 0, 0, 0, 1, 'x'},
		"truncated":         valid[:len(valid)/2],
		"missing end":       valid[:len(valid)-1],
		"negative length":   negativeLength.Bytes(),
		"huge length":       hugeLength.Bytes(),
		"unknown tag":       unknownTag.Bytes(),
		"too deep":          deep.Bytes(),
		"bad gzip":          {0x1f, 0x8b, 0, 0, 0},
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := nbt.Decode(bytes.NewReader(data)); err == nil {
				t.Error("want an error")
			} else if !strings.HasPrefix(err.Error(), "nbt: ") {
				t.Errorf("error %q lacks the nbt: prefix", err)
			}
		})
	}
}
