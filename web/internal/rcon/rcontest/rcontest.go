// Package rcontest runs a fake Minecraft RCON server for tests. It answers
// the way Minecraft does: auth replies of type 2 with ID -1 on a wrong
// password, command output split into packets of at most 4096 bytes, and
// "Unknown request" for packets of any other type.
package rcontest

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// MaxBody is the most output Minecraft puts in one packet.
const MaxBody = 4096

// Server is a fake RCON server listening on 127.0.0.1.
type Server struct {
	Addr     string
	Password string
	// Stall makes the server read packets but never answer them.
	Stall atomic.Bool

	ln net.Listener
	wg sync.WaitGroup

	mu       sync.Mutex
	handle   func(cmd string) string
	commands []string
	conns    []net.Conn
}

// New starts a server that echoes each command back, and closes it when
// the test ends.
func New(t testing.TB, password string) *Server {
	t.Helper()
	s, err := Start("127.0.0.1:0", password)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// Start starts a server on addr that echoes each command back.
func Start(addr, password string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Addr:     ln.Addr().String(),
		Password: password,
		handle:   func(cmd string) string { return "ran " + cmd },
		ln:       ln,
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// SetHandle replaces the function that returns a command's output.
func (s *Server) SetHandle(f func(cmd string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle = f
}

// Commands returns the commands run so far.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Close stops the server and drops its connections.
func (s *Server) Close() {
	s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			s.serveConn(conn)
		}()
	}
}

func (s *Server) serveConn(conn net.Conn) {
	r := bufio.NewReader(conn)
	authed := false
	for {
		id, typ, body, err := readPacket(r)
		if err != nil {
			return
		}
		if s.Stall.Load() {
			continue
		}
		switch {
		case typ == 3:
			if body != s.Password {
				writePacket(conn, -1, 2, "")
				continue
			}
			authed = true
			writePacket(conn, id, 2, "")
		case !authed:
			writePacket(conn, -1, 2, "")
		case typ == 2:
			s.mu.Lock()
			s.commands = append(s.commands, body)
			h := s.handle
			s.mu.Unlock()
			out := h(body)
			for {
				n := min(len(out), MaxBody)
				writePacket(conn, id, 0, out[:n])
				out = out[n:]
				if len(out) == 0 {
					break
				}
			}
		default:
			writePacket(conn, id, 0, fmt.Sprintf("Unknown request %x", typ))
		}
	}
}

func readPacket(r io.Reader) (id, typ int32, body string, err error) {
	var head [4]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return
	}
	n := binary.LittleEndian.Uint32(head[:])
	if n < 10 || n > 1460 {
		err = fmt.Errorf("bad length %d", n)
		return
	}
	buf := make([]byte, n)
	if _, err = io.ReadFull(r, buf); err != nil {
		return
	}
	id = int32(binary.LittleEndian.Uint32(buf[0:4]))
	typ = int32(binary.LittleEndian.Uint32(buf[4:8]))
	body = string(buf[8 : n-2])
	return
}

func writePacket(w io.Writer, id, typ int32, body string) {
	buf := binary.LittleEndian.AppendUint32(nil, uint32(10+len(body)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ))
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	w.Write(buf)
}
