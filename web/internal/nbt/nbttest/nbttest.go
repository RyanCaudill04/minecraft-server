// Package nbttest encodes NBT documents so tests can build synthetic
// Minecraft files. It accepts the same Go types package nbt decodes to.
package nbttest

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt"
)

// Encode returns root as an NBT document with an empty root name, gzipped if
// compress is true. Compound keys are written in sorted order so output is
// deterministic. It panics on input NBT can't represent, which is a bug in
// the test.
func Encode(root map[string]any, compress bool) []byte {
	var b bytes.Buffer
	b.WriteByte(nbt.TagCompound)
	writeString(&b, "")
	writeCompound(&b, root)
	if !compress {
		return b.Bytes()
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(b.Bytes())
	w.Close()
	return gz.Bytes()
}

// WriteFile writes root as a gzipped NBT file at path, creating parent
// folders, and fails the test on error.
func WriteFile(t testing.TB, path string, root map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, Encode(root, true), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tagOf(v any) byte {
	switch v.(type) {
	case int8:
		return nbt.TagByte
	case int16:
		return nbt.TagShort
	case int32:
		return nbt.TagInt
	case int64:
		return nbt.TagLong
	case float32:
		return nbt.TagFloat
	case float64:
		return nbt.TagDouble
	case []byte:
		return nbt.TagByteArray
	case string:
		return nbt.TagString
	case []any:
		return nbt.TagList
	case map[string]any:
		return nbt.TagCompound
	case []int32:
		return nbt.TagIntArray
	case []int64:
		return nbt.TagLongArray
	}
	panic(fmt.Sprintf("nbttest: unsupported type %T", v))
}

func writeCompound(b *bytes.Buffer, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(tagOf(m[k]))
		writeString(b, k)
		writeValue(b, m[k])
	}
	b.WriteByte(nbt.TagEnd)
}

func writeValue(b *bytes.Buffer, v any) {
	switch v := v.(type) {
	case int8, int16, int32, int64, float32, float64:
		binary.Write(b, binary.BigEndian, v)
	case []byte:
		binary.Write(b, binary.BigEndian, int32(len(v)))
		b.Write(v)
	case []int32:
		binary.Write(b, binary.BigEndian, int32(len(v)))
		binary.Write(b, binary.BigEndian, v)
	case []int64:
		binary.Write(b, binary.BigEndian, int32(len(v)))
		binary.Write(b, binary.BigEndian, v)
	case string:
		writeString(b, v)
	case []any:
		elem := byte(nbt.TagEnd)
		if len(v) > 0 {
			elem = tagOf(v[0])
		}
		// Every element of an NBT list has the list's one tag type.
		for _, item := range v {
			if tagOf(item) != elem {
				panic(fmt.Sprintf("nbttest: list mixes %T and %T", v[0], item))
			}
		}
		b.WriteByte(elem)
		binary.Write(b, binary.BigEndian, int32(len(v)))
		for _, item := range v {
			writeValue(b, item)
		}
	case map[string]any:
		writeCompound(b, v)
	default:
		panic(fmt.Sprintf("nbttest: unsupported type %T", v))
	}
}

func writeString(b *bytes.Buffer, s string) {
	if len(s) > math.MaxUint16 {
		panic(fmt.Sprintf("nbttest: string of %d bytes is too long for NBT", len(s)))
	}
	binary.Write(b, binary.BigEndian, uint16(len(s)))
	b.WriteString(s)
}
