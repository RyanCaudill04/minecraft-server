// Package rcon is a client for the Source RCON protocol as Minecraft speaks
// it, so admin and mc-rcon can run server commands now that Paper runs
// under systemd without a console.
//
// Each packet is a little-endian int32 length, then an int32 request ID, an
// int32 type and a NUL-terminated body, plus one more NUL. Minecraft splits
// long command output into packets of at most 4096 bytes and doesn't mark
// the last one, so Command sends an empty packet of an unknown type right
// after the command. The server answers packets in order, so its reply to
// that one ("Unknown request 0") marks the end of the command's output.
//
// RCON only answers while Paper is awake. LazyMC keeps Paper asleep until
// someone joins, and doesn't proxy RCON, so Dial fails while it sleeps.
package rcon

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Packet types.
const (
	typeResponse = 0 // command output
	typeCommand  = 2 // run a command; also the type of an auth reply
	typeAuth     = 3 // log in with the password
)

const (
	// MaxCommand is the longest command, in bytes, Minecraft accepts.
	MaxCommand = 1446
	// maxPacket bounds what's read for one packet. Minecraft's bodies are
	// at most 4096 bytes; the rest leaves room for other servers.
	maxPacket = 16 << 10
	// maxOutput bounds one command's output, so a broken server can't make
	// the client buffer forever.
	maxOutput = 1 << 20
	// DefaultTimeout applies when a context has no deadline.
	DefaultTimeout = 5 * time.Second
)

var (
	// ErrAuth means the server refused the password.
	ErrAuth = errors.New("rcon: wrong password")
	// ErrClosed means the client was closed.
	ErrClosed = errors.New("rcon: client closed")
	// ErrUnreachable means nothing accepted the connection, which is
	// normal while LazyMC keeps Paper asleep.
	ErrUnreachable = errors.New("rcon: server not reachable")
)

// Client is one logged-in RCON connection. Its methods are safe to call
// from several goroutines; commands run one at a time.
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	nextID int32
	closed bool
}

// Dial connects to addr and logs in with password. ctx bounds both; without
// a deadline, DefaultTimeout applies.
func Dial(ctx context.Context, addr, password string) (*Client, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn), nextID: 1}
	if err := c.auth(ctx, password); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) auth(ctx context.Context, password string) error {
	if err := validBody(password); err != nil {
		return err
	}
	stop := c.deadline(ctx)
	defer stop()

	id := c.id()
	if err := c.write(id, typeAuth, password); err != nil {
		return c.ctxErr(ctx, err)
	}
	// Some servers send an empty response before the auth reply.
	for {
		p, err := c.read()
		if err != nil {
			return c.ctxErr(ctx, err)
		}
		if p.typ != typeCommand {
			continue
		}
		if p.id == -1 {
			return ErrAuth
		}
		if p.id != id {
			return fmt.Errorf("auth reply for request %d, want %d", p.id, id)
		}
		return nil
	}
}

// Command runs cmd (without a leading slash) and returns its output. ctx
// bounds the whole exchange; without a deadline, DefaultTimeout applies.
// After an error the connection is in an unknown state, so it is closed.
func (c *Client) Command(ctx context.Context, cmd string) (string, error) {
	if cmd == "" {
		return "", errors.New("rcon: empty command")
	}
	if len(cmd) > MaxCommand {
		return "", fmt.Errorf("rcon: command is %d bytes, longer than %d", len(cmd), MaxCommand)
	}
	if err := validBody(cmd); err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", ErrClosed
	}
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	stop := c.deadline(ctx)
	defer stop()

	out, err := c.command(cmd)
	if err != nil {
		c.closeLocked()
		return "", c.ctxErr(ctx, err)
	}
	return out, nil
}

func (c *Client) command(cmd string) (string, error) {
	id, end := c.id(), c.id()
	if err := c.write(id, typeCommand, cmd); err != nil {
		return "", err
	}
	sentEnd := false
	var out strings.Builder
	for {
		p, err := c.read()
		if err != nil {
			return "", err
		}
		switch p.id {
		case id:
			if !sentEnd {
				if err := c.write(end, typeResponse, ""); err != nil {
					return "", err
				}
				sentEnd = true
			}
			if out.Len()+len(p.body) > maxOutput {
				return "", fmt.Errorf("output longer than %d bytes", maxOutput)
			}
			out.WriteString(p.body)
		case end:
			return out.String(), nil
		case -1:
			return "", ErrAuth
		default:
			return "", fmt.Errorf("reply for request %d, want %d", p.id, id)
		}
	}
}

// Close closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *Client) closeLocked() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// Run dials addr, runs one command and closes the connection. Admin uses
// it so a Paper restart never leaves it holding a dead connection.
func Run(ctx context.Context, addr, password, cmd string) (string, error) {
	ctx, cancel := withDefaultTimeout(ctx)
	defer cancel()
	c, err := Dial(ctx, addr, password)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Command(ctx, cmd)
}

func (c *Client) id() int32 {
	id := c.nextID
	c.nextID++
	if c.nextID <= 0 { // -1 means auth failed, so never use it
		c.nextID = 1
	}
	return id
}

// deadline applies ctx's deadline to the connection, and cuts it short if
// ctx is cancelled first. The returned func undoes the cancel hook.
func (c *Client) deadline(ctx context.Context) func() {
	if d, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Unix(1, 0)) })
	return func() { stop() }
}

// ctxErr reports a cancelled or expired ctx instead of the I/O error it
// caused.
func (c *Client) ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("rcon: %w", ctx.Err())
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("rcon: %w", context.DeadlineExceeded)
	}
	if errors.Is(err, ErrAuth) {
		return err
	}
	return fmt.Errorf("rcon: %w", err)
}

type packet struct {
	id   int32
	typ  int32
	body string
}

func (c *Client) write(id, typ int32, body string) error {
	buf := make([]byte, 0, 14+len(body))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(10+len(body)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ))
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	_, err := c.conn.Write(buf)
	return err
}

func (c *Client) read() (packet, error) {
	var head [4]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return packet{}, err
	}
	n := int32(binary.LittleEndian.Uint32(head[:]))
	if n < 10 || n > maxPacket {
		return packet{}, fmt.Errorf("bad packet length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return packet{}, err
	}
	if buf[n-1] != 0 || buf[n-2] != 0 {
		return packet{}, errors.New("packet isn't NUL-terminated")
	}
	return packet{
		id:   int32(binary.LittleEndian.Uint32(buf[0:4])),
		typ:  int32(binary.LittleEndian.Uint32(buf[4:8])),
		body: string(buf[8 : n-2]),
	}, nil
}

// validBody rejects text the protocol can't carry: a NUL would end the
// body early.
func validBody(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return errors.New("rcon: text contains a NUL byte")
	}
	return nil
}

func withDefaultTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, DefaultTimeout)
}
