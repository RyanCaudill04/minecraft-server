package rcon

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// fakeServer accepts one connection and answers like Minecraft: a login
// with the right password echoes its ID, a wrong one gets -1, and each
// command gets reply(cmd) back under the command's ID.
func fakeServer(t *testing.T, password string, reply func(string) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			id, typ, body, err := readPacket(conn)
			if err != nil {
				return
			}
			switch typ {
			case typeLogin:
				if body != password {
					id = -1
				}
				writePacket(conn, id, typeCommand, "")
			case typeCommand:
				if reply == nil {
					time.Sleep(time.Second) // never answer
					return
				}
				writePacket(conn, id, typeResponse, reply(body))
			}
		}
	}()
	return ln.Addr().String()
}

func TestCommands(t *testing.T) {
	addr := fakeServer(t, "secret", func(cmd string) string { return "ran " + cmd })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := Dial(ctx, addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, cmd := range []string{"save-off", "save-all flush", "save-on"} {
		got, err := c.Command(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if want := "ran " + cmd; got != want {
			t.Errorf("%s: got %q, want %q", cmd, got, want)
		}
	}
}

func TestWrongPassword(t *testing.T) {
	addr := fakeServer(t, "secret", nil)
	_, err := Dial(context.Background(), addr, "nope")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
}

func TestTimeout(t *testing.T) {
	addr := fakeServer(t, "secret", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c, err := Dial(ctx, addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Command("save-all flush"); err == nil {
		t.Fatal("want an error when the server never answers")
	}
}

func TestTooLong(t *testing.T) {
	addr := fakeServer(t, "secret", func(string) string { return "" })
	c, err := Dial(context.Background(), addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Command(string(bytes.Repeat([]byte("a"), maxRequest+1))); err == nil {
		t.Fatal("want an error for an oversized command")
	}
}

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writePacket(&buf, 7, typeCommand, "list"); err != nil {
		t.Fatal(err)
	}
	// 4 length + 4 ID + 4 type + "list" + 2 NULs.
	if buf.Len() != 18 {
		t.Fatalf("packet is %d bytes, want 18", buf.Len())
	}
	id, typ, body, err := readPacket(&buf)
	if err != nil || id != 7 || typ != typeCommand || body != "list" {
		t.Fatalf("got %d %d %q %v", id, typ, body, err)
	}
}

func TestBadLength(t *testing.T) {
	// A length below the 10-byte minimum.
	if _, _, _, err := readPacket(bytes.NewReader([]byte{3, 0, 0, 0})); err == nil {
		t.Fatal("want an error for a short length")
	}
}
