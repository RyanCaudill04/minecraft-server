// Package rcon is a minimal client for the Source RCON protocol that
// Minecraft servers speak when enable-rcon=true. It is used by the world
// backup to pause saving while it copies the world.
//
// Protocol reference: https://minecraft.wiki/w/RCON
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Packet types.
const (
	typeResponse = 0
	typeCommand  = 2
	typeLogin    = 3
)

// maxRequest is the largest request body Minecraft accepts, and
// maxResponse a generous cap on a reply (the server sends at most 4096
// bytes of body).
const (
	maxRequest  = 1446
	maxResponse = 1 << 16
)

// ErrAuth is returned when the server rejects the password.
var ErrAuth = errors.New("rcon: wrong password")

// Client is one authenticated RCON connection. It is not safe for
// concurrent use.
type Client struct {
	conn net.Conn
	stop func() bool // detaches the context watcher
	id   int32
}

// Dial connects to addr ("host:port") and logs in with password. The
// context bounds the dial and every later command.
func Dial(ctx context.Context, addr, password string) (*Client, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("rcon: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	// Unblock any pending read or write if the context is cancelled.
	stop := context.AfterFunc(ctx, func() { conn.Close() })

	c := &Client{conn: conn, stop: stop}
	id, _, err := c.roundTrip(typeLogin, password)
	if err != nil {
		c.Close()
		return nil, err
	}
	if id == -1 {
		c.Close()
		return nil, ErrAuth
	}
	return c, nil
}

// Command runs cmd (without a leading slash) and returns the server's reply.
func (c *Client) Command(cmd string) (string, error) {
	_, body, err := c.roundTrip(typeCommand, cmd)
	return body, err
}

// Close closes the connection.
func (c *Client) Close() error {
	c.stop()
	return c.conn.Close()
}

// roundTrip sends one request and reads one reply. Minecraft answers every
// request with exactly one packet carrying the request's ID, or -1 for a
// failed login.
func (c *Client) roundTrip(typ int32, body string) (int32, string, error) {
	if len(body) > maxRequest {
		return 0, "", fmt.Errorf("rcon: request is %d bytes, the limit is %d", len(body), maxRequest)
	}
	c.id++
	if err := writePacket(c.conn, c.id, typ, body); err != nil {
		return 0, "", fmt.Errorf("rcon: %w", err)
	}
	id, rtyp, reply, err := readPacket(c.conn)
	if err != nil {
		return 0, "", fmt.Errorf("rcon: %w", err)
	}
	if id != -1 && id != c.id {
		return 0, "", fmt.Errorf("rcon: reply has ID %d, want %d", id, c.id)
	}
	// A login reply is typed 2 (the same value as a command); a command
	// reply is typed 0.
	if typ == typeCommand && rtyp != typeResponse {
		return 0, "", fmt.Errorf("rcon: unexpected reply type %d", rtyp)
	}
	return id, reply, nil
}

// writePacket writes length, ID, type, the body and two NUL bytes, all
// integers little-endian.
func writePacket(w io.Writer, id, typ int32, body string) error {
	buf := make([]byte, 0, 14+len(body))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(10+len(body)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ))
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	_, err := w.Write(buf)
	return err
}

func readPacket(r io.Reader) (id, typ int32, body string, err error) {
	var length int32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return 0, 0, "", err
	}
	if length < 10 || length > maxResponse {
		return 0, 0, "", fmt.Errorf("invalid packet length %d", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(data[0:4]))
	typ = int32(binary.LittleEndian.Uint32(data[4:8]))
	return id, typ, string(data[8 : len(data)-2]), nil
}
