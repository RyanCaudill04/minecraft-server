// Package nbttest encodes NBT documents so tests can build synthetic
// Minecraft files. It accepts the same Go types package nbt decodes to.
package nbttest

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Tag type IDs, matching package nbt.
const (
	tagEnd = iota
	tagByte
	tagShort
	tagInt
	tagLong
	tagFloat
	tagDouble
	tagByteArray
	tagString
	tagList
	tagCompound
	tagIntArray
	tagLongArray
)

// Encode returns root as an NBT document with an empty root name, gzipped if
// compress is true. Compound keys are written in sorted order so output is
// deterministic. It panics on unsupported types, which is a bug in the test.
func Encode(root map[string]any, compress bool) []byte {
	var b bytes.Buffer
	b.WriteByte(tagCompound)
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
		return tagByte
	case int16:
		return tagShort
	case int32:
		return tagInt
	case int64:
		return tagLong
	case float32:
		return tagFloat
	case float64:
		return tagDouble
	case []byte:
		return tagByteArray
	case string:
		return tagString
	case []any:
		return tagList
	case map[string]any:
		return tagCompound
	case []int32:
		return tagIntArray
	case []int64:
		return tagLongArray
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
	b.WriteByte(tagEnd)
}

func writeValue(b *bytes.Buffer, v any) {
	switch v := v.(type) {
	case int8, int16, int32, int64, float32, float64, []int32, []int64:
		if s, ok := v.([]int32); ok {
			binary.Write(b, binary.BigEndian, int32(len(s)))
		}
		if s, ok := v.([]int64); ok {
			binary.Write(b, binary.BigEndian, int32(len(s)))
		}
		binary.Write(b, binary.BigEndian, v)
	case []byte:
		binary.Write(b, binary.BigEndian, int32(len(v)))
		b.Write(v)
	case string:
		writeString(b, v)
	case []any:
		elem := byte(tagEnd)
		if len(v) > 0 {
			elem = tagOf(v[0])
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
	binary.Write(b, binary.BigEndian, uint16(len(s)))
	b.WriteString(s)
}
