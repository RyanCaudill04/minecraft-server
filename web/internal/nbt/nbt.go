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

// Tag type IDs. They are exported so package nbttest writes the same ones.
const (
	TagEnd = iota
	TagByte
	TagShort
	TagInt
	TagLong
	TagFloat
	TagDouble
	TagByteArray
	TagString
	TagList
	TagCompound
	TagIntArray
	TagLongArray
)

// Limits that stop a corrupt file from forcing huge allocations or deep
// recursion. The files this package reads (level.dat, the clock and weather
// files) are a few KiB with no large arrays or lists, so these are generous.
const (
	maxLength = 1 << 16 // elements in one array or list
	maxSize   = 1 << 20 // bytes in a whole document, after gunzipping
	maxDepth  = 512     // nested lists and compounds
	chunkSize = 4096    // bytes of an array read at a time
)

// ReadFile decodes the NBT file at path. See Decode.
func ReadFile(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	root, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return root, nil
}

// Decode reads one NBT document, gzip-compressed or not, and returns its root
// compound.
func Decode(r io.Reader) (map[string]any, error) {
	br := bufio.NewReader(r)
	var src io.Reader = br
	var gz *gzip.Reader
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err = gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("nbt: %w", err)
		}
		defer gz.Close()
		src = bufio.NewReader(gz)
	}

	d := decoder{r: &limitReader{r: src, n: maxSize}}
	// An empty file is a plain EOF. Past the first byte, running out of
	// input means the file was cut short; see readFull.
	var tag [1]byte
	if _, err := io.ReadFull(d.r, tag[:]); err != nil {
		return nil, fmt.Errorf("nbt: %w", err)
	}
	if tag[0] != TagCompound {
		return nil, fmt.Errorf("nbt: root tag is %d, want a compound", tag[0])
	}
	if _, err := d.string(); err != nil { // the root's name, usually empty
		return nil, fmt.Errorf("nbt: %w", err)
	}
	root, err := d.compound(0)
	if err != nil {
		return nil, fmt.Errorf("nbt: %w", err)
	}
	// Decoding stops at the root's End tag, before the gzip trailer. Reading
	// on to the end makes gzip check its CRC, so damage in the last bytes
	// isn't missed.
	if gz != nil {
		if _, err := io.Copy(io.Discard, d.r); err != nil {
			return nil, fmt.Errorf("nbt: %w", err)
		}
	}
	return root, nil
}

var errTooLarge = fmt.Errorf("document larger than %d bytes", maxSize)

// limitReader is like io.LimitReader, but it fails with errTooLarge instead
// of ending, so an oversized file isn't mistaken for a truncated one.
type limitReader struct {
	r io.Reader
	n int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

type decoder struct {
	r   io.Reader
	buf [8]byte
}

// readFull fills p. It is only used after the root tag byte, so io.EOF
// always means the document ended early.
func (d *decoder) readFull(p []byte) error {
	_, err := io.ReadFull(d.r, p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (d *decoder) read(n int) ([]byte, error) {
	return d.buf[:n], d.readFull(d.buf[:n])
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

// length reads the element count of an array or list and rejects nonsense
// values.
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
	if err := d.readFull(s); err != nil {
		return "", err
	}
	return string(s), nil
}

// array reads n elements of size bytes each and passes them to add a chunk
// at a time. n comes from the file, so slices only grow as data arrives
// instead of being allocated up front from a length that may be corrupt.
func (d *decoder) array(n, size int, add func(chunk []byte)) error {
	var buf [chunkSize]byte
	for left := n * size; left > 0; {
		c := buf[:min(left, chunkSize)]
		if err := d.readFull(c); err != nil {
			return err
		}
		add(c)
		left -= len(c)
	}
	return nil
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
		if tag == TagEnd {
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
	case TagByte:
		b, err := d.u8()
		return int8(b), err
	case TagShort:
		return d.i16()
	case TagInt:
		return d.i32()
	case TagLong:
		return d.i64()
	case TagFloat:
		v, err := d.i32()
		return math.Float32frombits(uint32(v)), err
	case TagDouble:
		v, err := d.i64()
		return math.Float64frombits(uint64(v)), err
	case TagString:
		return d.string()
	case TagByteArray:
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		a := make([]byte, 0, min(n, chunkSize))
		err = d.array(n, 1, func(c []byte) { a = append(a, c...) })
		return a, err
	case TagIntArray:
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		a := make([]int32, 0, min(n, chunkSize/4))
		err = d.array(n, 4, func(c []byte) {
			for i := 0; i < len(c); i += 4 {
				a = append(a, int32(binary.BigEndian.Uint32(c[i:])))
			}
		})
		return a, err
	case TagLongArray:
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		a := make([]int64, 0, min(n, chunkSize/8))
		err = d.array(n, 8, func(c []byte) {
			for i := 0; i < len(c); i += 8 {
				a = append(a, int64(binary.BigEndian.Uint64(c[i:])))
			}
		})
		return a, err
	case TagList:
		return d.list(depth)
	case TagCompound:
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
