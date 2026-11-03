// Package ping implements the Minecraft Java Edition Server List Ping
// (status) protocol. It reports whether the server is reachable and what it
// advertises, without logging in, so it never wakes a sleeping LazyMC proxy.
// Wake is the one exception: it starts a login on purpose, to wake it.
//
// Protocol reference: https://minecraft.wiki/w/Java_Edition_protocol/Server_List_Ping
package ping

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxPacketLength is the largest packet length the protocol allows (the
// largest value a 3-byte VarInt can hold).
const maxPacketLength = 1<<21 - 1

// Inner errors carry no "ping:" prefix; Status adds it once.
var errVarIntTooLong = errors.New("VarInt is longer than 5 bytes")

// Response is the JSON payload returned by a status request.
type Response struct {
	Version     Version     `json:"version"`
	Players     Players     `json:"players"`
	Description Description `json:"description"`
}

// Version is the server's advertised game version and protocol number.
type Version struct {
	Name     string `json:"name"`
	Protocol int    `json:"protocol"`
}

// Players is the online/max player count and an optional sample of names.
type Players struct {
	Max    int      `json:"max"`
	Online int      `json:"online"`
	Sample []Player `json:"sample,omitempty"`
}

// Player is one entry in the players sample.
type Player struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// Description is the server MOTD. Servers may send it as a plain string or a
// chat component object, so it needs custom JSON decoding.
type Description struct {
	Text string
}

// UnmarshalJSON accepts either a JSON string or a chat component object.
// It never fails: the MOTD is cosmetic, and an odd one must not turn a
// server that answered into a failed ping.
func (d *Description) UnmarshalJSON(data []byte) error {
	d.Text = componentText(data)
	return nil
}

// componentText flattens a chat component (a string, an object with "text"
// and "extra", or an array of components) into its plain text. Legacy §
// formatting codes inside the text are left as-is. Anything else (null, a
// number, a bool) counts as no text, and so does a field of the wrong type.
func componentText(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return ""
	}

	switch data[0] {
	case '"':
		var s string
		json.Unmarshal(data, &s)
		return s
	case '[':
		var parts []json.RawMessage
		json.Unmarshal(data, &parts)
		return joinComponents(parts)
	case '{':
		// On a type mismatch json.Unmarshal still fills the fields it
		// can, so a bad "text" keeps the "extra" and vice versa.
		var c struct {
			Text  string            `json:"text"`
			Extra []json.RawMessage `json:"extra"`
		}
		json.Unmarshal(data, &c)
		return c.Text + joinComponents(c.Extra)
	default:
		return ""
	}
}

func joinComponents(parts []json.RawMessage) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(componentText(p))
	}
	return sb.String()
}

// Status connects to addr ("host:port"), performs the handshake and status
// request, and returns the decoded response.
func Status(ctx context.Context, addr string) (*Response, error) {
	var resp *Response
	err := dial(ctx, addr, func(conn net.Conn, host string, port uint16) (err error) {
		resp, err = exchange(conn, host, port)
		return err
	})
	return resp, err
}

// Wake starts a login as name on addr and hangs up, which makes a sleeping
// LazyMC start Paper. LazyMC starts it as soon as it reads the Login Start
// packet, and carries on if the client leaves, so Wake doesn't wait for
// Paper. Against Paper itself, or a LazyMC that's already awake, it's an
// abandoned login and does nothing.
func Wake(ctx context.Context, addr, name string) error {
	return dial(ctx, addr, func(conn net.Conn, host string, port uint16) error {
		// bufio.Writer keeps the first write error and returns it from
		// Flush, so the writes don't need checking one by one.
		w := bufio.NewWriter(conn)
		writePacket(w, handshakePacket(host, port, nextLogin))
		writePacket(w, loginStartPacket(name))
		return w.Flush()
	})
}

// dial connects to addr and runs talk on the connection, with the
// context's deadline and cancellation applied to every read and write.
func dial(ctx context.Context, addr string, talk func(conn net.Conn, host string, port uint16) error) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return fmt.Errorf("ping: invalid port %q: %w", portStr, err)
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	defer conn.Close()

	// Apply the context to reads and writes, not just the dial: honor its
	// deadline, and unblock any pending I/O if it is cancelled.
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := talk(conn, host, uint16(port)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ping: %w", ctxErr)
		}
		// The connection deadline is the context's deadline, so a read can
		// time out a moment before the context itself reports expiry.
		if _, ok := ctx.Deadline(); ok && errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("ping: %w", context.DeadlineExceeded)
		}
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// exchange sends the handshake and status request on conn and decodes the
// status response.
func exchange(conn net.Conn, host string, port uint16) (*Response, error) {
	// bufio.Writer keeps the first write error and returns it from Flush,
	// so the writes don't need checking one by one.
	w := bufio.NewWriter(conn)
	writePacket(w, handshakePacket(host, port, nextStatus))
	writePacket(w, []byte{0x00}) // Status Request: packet ID 0, no fields
	if err := w.Flush(); err != nil {
		return nil, err
	}

	id, payload, err := readPacket(bufio.NewReader(conn))
	if err != nil {
		return nil, err
	}
	if id != 0x00 {
		return nil, fmt.Errorf("unexpected packet ID 0x%02x in status response", id)
	}

	raw, err := readString(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("decoding status JSON: %w", err)
	}
	return &resp, nil
}

// The handshake's next state: status for a ping, login to wake LazyMC.
const (
	nextStatus = 1
	nextLogin  = 2
)

// handshakePacket builds the body of the Handshake packet (ID 0x00).
func handshakePacket(host string, port uint16, next int32) []byte {
	var b bytes.Buffer
	writeVarInt(&b, 0x00) // packet ID
	// Protocol version: -1 is conventional for status pings, and LazyMC
	// doesn't check it on a login either.
	writeVarInt(&b, -1)
	writeString(&b, host)
	b.WriteByte(byte(port >> 8))
	b.WriteByte(byte(port))
	writeVarInt(&b, next)
	return b.Bytes()
}

// loginStartPacket builds the body of the Login Start packet (ID 0x00): the
// player's name, then their UUID, which is left zero. Clients before 1.19
// sent only the name; LazyMC reads the name and ignores the rest.
func loginStartPacket(name string) []byte {
	var b bytes.Buffer
	writeVarInt(&b, 0x00)
	writeString(&b, name)
	b.Write(make([]byte, 16))
	return b.Bytes()
}

// writePacket frames body as VarInt(len(body)) followed by body and writes it
// to w.
func writePacket(w *bufio.Writer, body []byte) {
	writeVarInt(w, int32(len(body)))
	w.Write(body)
}

// reader is what the read helpers need: bufio.Reader and bytes.Reader are
// both one.
type reader interface {
	io.Reader
	io.ByteReader
}

// readPacket reads one length-prefixed packet from r and returns its packet ID
// and remaining payload.
func readPacket(r reader) (id int32, payload []byte, err error) {
	length, err := readVarInt(r)
	if err != nil {
		return 0, nil, err
	}
	if length < 1 || length > maxPacketLength {
		return 0, nil, fmt.Errorf("invalid packet length %d", length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}

	body := bytes.NewReader(data)
	id, err = readVarInt(body)
	if err != nil {
		return 0, nil, err
	}
	return id, data[len(data)-body.Len():], nil
}

// writer is what the write helpers need. Only bytes.Buffer and bufio.Writer
// are passed in, and both report errors elsewhere (never, or from Flush), so
// the helpers return none.
type writer interface {
	io.ByteWriter
	io.StringWriter
}

// writeVarInt writes v using Minecraft's VarInt encoding: 7 bits per byte,
// least-significant group first, high bit set when more bytes follow.
func writeVarInt(w io.ByteWriter, v int32) {
	// Shift as unsigned so negative values terminate after 5 bytes instead of
	// sign-extending forever.
	u := uint32(v)
	for u >= 0x80 {
		w.WriteByte(byte(u&0x7f) | 0x80)
		u >>= 7
	}
	w.WriteByte(byte(u))
}

// readVarInt reads a VarInt from r. It returns an error if the value is longer
// than 5 bytes.
func readVarInt(r io.ByteReader) (int32, error) {
	var u uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			if i > 0 && err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		u |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int32(u), nil
		}
	}
	return 0, errVarIntTooLong
}

// writeString writes s as VarInt(byte length) followed by its UTF-8 bytes.
func writeString(w writer, s string) {
	writeVarInt(w, int32(len(s)))
	w.WriteString(s)
}

// readString reads a VarInt length-prefixed UTF-8 string from r.
func readString(r reader) (string, error) {
	n, err := readVarInt(r)
	if err != nil {
		return "", err
	}
	if n < 0 || n > maxPacketLength {
		return "", fmt.Errorf("invalid string length %d", n)
	}
	b := make([]byte, n)
	_, err = io.ReadFull(r, b)
	return string(b), err
}
