package ping

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// fakeServer listens on a random local port and answers one status
// request per connection with reply, which builds the response packet body.
// It records the handshake it received.
func fakeServer(t *testing.T, reply func(*bytes.Buffer)) (addr string, handshakes <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	got := make(chan []byte, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				_, handshake, err := readPacket(r)
				if err != nil {
					return
				}
				got <- handshake
				if _, _, err := readPacket(r); err != nil { // status request
					return
				}
				if reply == nil {
					time.Sleep(time.Second) // never answer
					return
				}
				var body bytes.Buffer
				reply(&body)
				writePacket(conn, body.Bytes())
			}()
		}
	}()
	return ln.Addr().String(), got
}

// statusReply answers with packet ID 0 and the given JSON.
func statusReply(json string) func(*bytes.Buffer) {
	return func(b *bytes.Buffer) {
		writeVarInt(b, 0x00)
		writeString(b, json)
	}
}

func TestStatus(t *testing.T) {
	addr, handshakes := fakeServer(t, statusReply(`{
		"version": {"name": "Paper 26.2", "protocol": 999},
		"players": {"max": 20, "online": 2, "sample": [{"name": "Alice", "id": "a-id"}]},
		"description": {"text": "Hello ", "extra": [{"text": "world"}]},
		"favicon": "data:image/png;base64,AAAA"
	}`))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := Status(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}

	if resp.Version.Name != "Paper 26.2" || resp.Version.Protocol != 999 {
		t.Errorf("Version = %+v", resp.Version)
	}
	if resp.Players.Online != 2 || resp.Players.Max != 20 || len(resp.Players.Sample) != 1 || resp.Players.Sample[0].Name != "Alice" {
		t.Errorf("Players = %+v", resp.Players)
	}
	if resp.Description.Text != "Hello world" {
		t.Errorf("Description = %q, want %q", resp.Description.Text, "Hello world")
	}

	// The handshake asks for the status state (1), which is what keeps a
	// sleeping LazyMC asleep. Logging in (2) would wake it.
	handshake := <-handshakes
	if next := handshake[len(handshake)-1]; next != 1 {
		t.Errorf("handshake next state = %d, want 1 (status)", next)
	}
}

func TestStatusErrors(t *testing.T) {
	tests := map[string]func(*bytes.Buffer){
		"wrong packet ID": func(b *bytes.Buffer) {
			writeVarInt(b, 0x05)
			writeString(b, "{}")
		},
		"invalid JSON":    statusReply(`{"players": `),
		"bad description": statusReply(`{"description": 42}`),
		"string length lies": func(b *bytes.Buffer) {
			writeVarInt(b, 0x00)
			writeVarInt(b, 1000) // claims 1000 bytes, sends 2
			b.WriteString("{}")
		},
	}
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			addr, _ := fakeServer(t, reply)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := Status(ctx, addr); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestStatusTimeout(t *testing.T) {
	addr, _ := fakeServer(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Status(ctx, addr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Status took %v; it should give up at the context deadline", elapsed)
	}
}

func TestStatusBadAddress(t *testing.T) {
	ctx := context.Background()
	for _, addr := range []string{"no-port", "127.0.0.1:99999", "127.0.0.1:notaport"} {
		if _, err := Status(ctx, addr); err == nil {
			t.Errorf("Status(%q): want an error", addr)
		}
	}
}

func TestStatusConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Status(ctx, addr); err == nil {
		t.Error("want an error")
	}
}

func TestVarIntRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, 127, 128, 255, 25565, 2097151, 1<<31 - 1, -1, -1 << 31} {
		var b bytes.Buffer
		writeVarInt(&b, v)
		if b.Len() > 5 {
			t.Errorf("%d encoded to %d bytes, max is 5", v, b.Len())
		}
		got, err := readVarInt(&b)
		if err != nil || got != v {
			t.Errorf("round trip %d: got %d, %v", v, got, err)
		}
	}
}

func TestReadVarIntErrors(t *testing.T) {
	if _, err := readVarInt(bytes.NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x01})); !errors.Is(err, errVarIntTooLong) {
		t.Errorf("6-byte VarInt: err = %v, want errVarIntTooLong", err)
	}
	if _, err := readVarInt(bytes.NewReader([]byte{0x80})); err == nil {
		t.Error("truncated VarInt: want an error")
	}
}

func TestReadPacketRejectsBadLengths(t *testing.T) {
	for name, data := range map[string][]byte{
		"zero length": {0x00},
		"too long":    {0xff, 0xff, 0xff, 0x7f}, // larger than maxPacketLength
		"truncated":   {0x05, 0x00, 0x01},
	} {
		if _, _, err := readPacket(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestComponentText(t *testing.T) {
	tests := []struct {
		json, want string
	}{
		{`"plain"`, "plain"},
		{`{"text": "a"}`, "a"},
		{`{"text": "a", "extra": ["b", {"text": "c"}]}`, "abc"},
		{`[{"text": "x"}, "y"]`, "xy"},
		{`"§7Server is sleeping"`, "§7Server is sleeping"},
		{``, ""},
	}
	for _, tt := range tests {
		got, err := componentText([]byte(tt.json))
		if err != nil || got != tt.want {
			t.Errorf("componentText(%s) = %q, %v; want %q", tt.json, got, err, tt.want)
		}
	}
	for _, bad := range []string{`42`, `{"text": 1}`, `[1]`} {
		if _, err := componentText([]byte(bad)); err == nil {
			t.Errorf("componentText(%s): want an error", bad)
		}
	}
}
