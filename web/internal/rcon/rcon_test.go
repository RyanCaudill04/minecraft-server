package rcon_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ryancaudill04/minecraft-server/web/internal/rcon"
	"github.com/ryancaudill04/minecraft-server/web/internal/rcon/rcontest"
)

func TestCommand(t *testing.T) {
	s := rcontest.New(t, "hunter2")
	c, err := rcon.Dial(context.Background(), s.Addr, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for _, cmd := range []string{"list", "say hi there"} {
		out, err := c.Command(context.Background(), cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if out != "ran "+cmd {
			t.Errorf("%s = %q, want %q", cmd, out, "ran "+cmd)
		}
	}
	if got := s.Commands(); len(got) != 2 || got[0] != "list" || got[1] != "say hi there" {
		t.Errorf("server ran %q", got)
	}
}

func TestMultiPacketOutput(t *testing.T) {
	s := rcontest.New(t, "pw")
	// Three full packets and a partial one; also exactly one full packet,
	// where only the end marker tells the client the output is over.
	long := strings.Repeat("abcdefghij", 1300)
	exact := strings.Repeat("x", rcontest.MaxBody)
	s.SetHandle(func(cmd string) string {
		if cmd == "exact" {
			return exact
		}
		return long
	})

	for cmd, want := range map[string]string{"long": long, "exact": exact} {
		out, err := rcon.Run(context.Background(), s.Addr, "pw", cmd)
		if err != nil {
			t.Fatal(err)
		}
		if out != want {
			t.Errorf("%s: got %d bytes, want %d", cmd, len(out), len(want))
		}
	}
}

func TestEmptyOutput(t *testing.T) {
	s := rcontest.New(t, "pw")
	s.SetHandle(func(string) string { return "" })
	out, err := rcon.Run(context.Background(), s.Addr, "pw", "save-all")
	if err != nil || out != "" {
		t.Errorf("Run = %q, %v; want empty output", out, err)
	}
}

func TestWrongPassword(t *testing.T) {
	s := rcontest.New(t, "s3cret-right")
	_, err := rcon.Dial(context.Background(), s.Addr, "s3cret-wrong")
	if !errors.Is(err, rcon.ErrAuth) {
		t.Fatalf("Dial with wrong password: %v, want ErrAuth", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %q mentions a password", err)
	}
	if len(s.Commands()) != 0 {
		t.Errorf("server ran %q after a failed login", s.Commands())
	}
}

func TestUnreachable(t *testing.T) {
	// Grab a free port, then close it, so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	_, err = rcon.Run(context.Background(), addr, "pw", "list")
	if !errors.Is(err, rcon.ErrUnreachable) {
		t.Errorf("Run against a closed port: %v, want ErrUnreachable", err)
	}
}

func TestTimeout(t *testing.T) {
	s := rcontest.New(t, "pw")

	// Stalling during login.
	s.Stall.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rcon.Dial(ctx, s.Addr, "pw")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Dial to a stalled server: %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Dial took %v with a 100ms deadline", d)
	}
}

func TestTimeoutDuringCommand(t *testing.T) {
	s := rcontest.New(t, "pw")
	c, err := rcon.Dial(context.Background(), s.Addr, "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s.Stall.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Command(ctx, "list"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Command on a stalled server: %v, want DeadlineExceeded", err)
	}
	// The connection may hold half a reply now, so it's closed.
	if _, err := c.Command(context.Background(), "list"); !errors.Is(err, rcon.ErrClosed) {
		t.Errorf("Command after a timeout: %v, want ErrClosed", err)
	}
}

func TestCancel(t *testing.T) {
	s := rcontest.New(t, "pw")
	c, err := rcon.Dial(context.Background(), s.Addr, "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s.Stall.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := c.Command(ctx, "list"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Command: %v, want Canceled", err)
	}
}

func TestBadCommands(t *testing.T) {
	s := rcontest.New(t, "pw")
	c, err := rcon.Dial(context.Background(), s.Addr, "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for name, cmd := range map[string]string{
		"empty":    "",
		"NUL":      "say a\x00b",
		"too long": strings.Repeat("a", rcon.MaxCommand+1),
	} {
		if _, err := c.Command(context.Background(), cmd); err == nil {
			t.Errorf("%s command: no error", name)
		}
	}
	if len(s.Commands()) != 0 {
		t.Errorf("server ran %q", s.Commands())
	}
	// Rejected commands are never sent, so the connection still works.
	if _, err := c.Command(context.Background(), "list"); err != nil {
		t.Errorf("Command after rejected ones: %v", err)
	}
}

// A server that sends garbage gets an error, not a hang or a huge buffer.
func TestBadPacket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Read(make([]byte, 64))
		conn.Write([]byte{0xff, 0xff, 0xff, 0x7f}) // a 2 GB packet
	}()
	_, err = rcon.Dial(context.Background(), ln.Addr().String(), "pw")
	if err == nil || !strings.Contains(err.Error(), "bad packet length") {
		t.Errorf("Dial to a server sending garbage: %v", err)
	}
}

func TestConcurrentCommands(t *testing.T) {
	s := rcontest.New(t, "pw")
	c, err := rcon.Dial(context.Background(), s.Addr, "pw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			cmd := strings.Repeat("a", i+1)
			out, err := c.Command(context.Background(), cmd)
			if err == nil && out != "ran "+cmd {
				err = errors.New("got " + out + " for " + cmd)
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
