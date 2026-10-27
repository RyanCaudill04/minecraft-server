// Package nbt decodes Minecraft's Named Binary Tag format into plain Go
// values. It is meant for small files such as level.dat, not region files.
//
// Tags map to Go types as follows: Byte int8, Short int16, Int int32, Long
// int64, Float float32, Double float64, String string, Byte Array []byte,
// Int Array []int32, Long Array []int64, List []any, Compound map[string]any.
//
// Format reference: https://minecraft.wiki/w/NBT_format
package nbt

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

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

// Limits that stop a corrupt file from forcing huge allocations or deep
// recursion.
const (
	maxLength = 1 << 24
	maxDepth  = 512
)

// ReadFile decodes the NBT file at path. See Decode.
func ReadFile(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Decode(f)
}

// Decode reads one NBT document, gzip-compressed or not, and returns its root
// compound.
func Decode(r io.Reader) (map[string]any, error) {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("nbt: %w", err)
		}
		defer gz.Close()
		src = bufio.NewReader(gz)
	}

	d := decoder{r: src}
	tag, err := d.u8()
	if err != nil {
		return nil, fmt.Errorf("nbt: %w", err)
	}
	if tag != tagCompound {
		return nil, fmt.Errorf("nbt: root tag is %d, want a compound", tag)
	}
	if _, err := d.string(); err != nil { // the root's name, usually empty
		return nil, fmt.Errorf("nbt: %w", err)
	}
	root, err := d.compound(0)
	if err != nil {
		return nil, fmt.Errorf("nbt: %w", err)
	}
	return root, nil
}

type decoder struct {
	r   io.Reader
	buf [8]byte
}

func (d *decoder) read(n int) ([]byte, error) {
	_, err := io.ReadFull(d.r, d.buf[:n])
	return d.buf[:n], err
}

func (d *decoder) u8() (byte, error) {
	b, err := d.read(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *decoder) i16() (int16, error) {
	b, err := d.read(2)
	return int16(binary.BigEndian.Uint16(b)), err
}

func (d *decoder) i32() (int32, error) {
	b, err := d.read(4)
	return int32(binary.BigEndian.Uint32(b)), err
}

func (d *decoder) i64() (int64, error) {
	b, err := d.read(8)
	return int64(binary.BigEndian.Uint64(b)), err
}

// length reads an Int array or list length and rejects nonsense values.
func (d *decoder) length() (int, error) {
	n, err := d.i32()
	if err != nil {
		return 0, err
	}
	if n < 0 || n > maxLength {
		return 0, fmt.Errorf("invalid length %d", n)
	}
	return int(n), nil
}

// string reads a length-prefixed string. NBT uses Java's modified UTF-8,
// which matches standard UTF-8 for everything except NUL and emoji; those are
// rare in the files this package reads, so the bytes are used as-is.
func (d *decoder) string() (string, error) {
	b, err := d.read(2)
	if err != nil {
		return "", err
	}
	s := make([]byte, binary.BigEndian.Uint16(b))
	if _, err := io.ReadFull(d.r, s); err != nil {
		return "", err
	}
	return string(s), nil
}

func (d *decoder) compound(depth int) (map[string]any, error) {
	if depth > maxDepth {
		return nil, errors.New("nesting too deep")
	}
	m := make(map[string]any)
	for {
		tag, err := d.u8()
		if err != nil {
			return nil, err
		}
		if tag == tagEnd {
			return m, nil
		}
		name, err := d.string()
		if err != nil {
			return nil, err
		}
		v, err := d.value(tag, depth+1)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		m[name] = v
	}
}

func (d *decoder) value(tag byte, depth int) (any, error) {
	switch tag {
	case tagByte:
		b, err := d.u8()
		return int8(b), err
	case tagShort:
		return d.i16()
	case tagInt:
		return d.i32()
	case tagLong:
		return d.i64()
	case tagFloat:
		v, err := d.i32()
		return math.Float32frombits(uint32(v)), err
	case tagDouble:
		v, err := d.i64()
		return math.Float64frombits(uint64(v)), err
	case tagString:
		return d.string()
	case tagByteArray:
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		b := make([]byte, n)
		_, err = io.ReadFull(d.r, b)
		return b, err
	case tagIntArray:
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		a := make([]int32, n)
		return a, binary.Read(d.r, binary.BigEndian, a)
	case tagLongArray:
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		a := make([]int64, n)
		return a, binary.Read(d.r, binary.BigEndian, a)
	case tagList:
		return d.list(depth)
	case tagCompound:
		return d.compound(depth)
	}
	return nil, fmt.Errorf("unknown tag type %d", tag)
}

func (d *decoder) list(depth int) ([]any, error) {
	if depth > maxDepth {
		return nil, errors.New("nesting too deep")
	}
	elem, err := d.u8()
	if err != nil {
		return nil, err
	}
	n, err := d.length()
	if err != nil {
		return nil, err
	}
	list := make([]any, 0, min(n, 1024))
	for range n {
		v, err := d.value(elem, depth+1)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	return list, nil
}

// Int returns the integer stored at the path of compound keys under m, for
// any NBT integer type. ok is false if the path is missing or not an integer.
func Int(m map[string]any, path ...string) (n int64, ok bool) {
	switch v := Get(m, path...).(type) {
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

// String returns the string stored at the path of compound keys under m.
func String(m map[string]any, path ...string) (s string, ok bool) {
	s, ok = Get(m, path...).(string)
	return s, ok
}

// Get follows a path of compound keys under m and returns the value found,
// or nil if any step is missing.
func Get(m map[string]any, path ...string) any {
	var v any = m
	for _, key := range path {
		c, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = c[key]
	}
	return v
}
