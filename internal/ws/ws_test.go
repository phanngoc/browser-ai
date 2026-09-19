package ws

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoServer is a tiny server-side framer for tests only.
func echoServer(t *testing.T, fragment bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "not ws", 400)
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + guid))
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijack")
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
			base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
		go serve(conn, rw.Reader, fragment)
	}))
}

func serve(conn net.Conn, r *bufio.Reader, fragment bool) {
	defer conn.Close()
	// send a ping first; client must answer with pong
	_, _ = conn.Write(append([]byte{0x80 | opPing, 4}, "ping"...))
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return
		}
		op := hdr[0] & 0x0F
		masked := hdr[1]&0x80 != 0
		n := int(hdr[1] & 0x7F)
		if n == 126 {
			var b [2]byte
			_, _ = io.ReadFull(r, b[:])
			n = int(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			var b [8]byte
			_, _ = io.ReadFull(r, b[:])
			n = int(binary.BigEndian.Uint64(b[:]))
		}
		var mask [4]byte
		if masked {
			_, _ = io.ReadFull(r, mask[:])
		}
		p := make([]byte, n)
		_, _ = io.ReadFull(r, p)
		if masked {
			applyMask(p, mask)
		}
		switch op {
		case opPong:
			if string(p) != "ping" {
				_, _ = conn.Write([]byte{0x80 | opClose, 0})
				return
			}
		case opClose:
			_, _ = conn.Write([]byte{0x80 | opClose, 0})
			return
		case opText:
			if fragment && len(p) > 4 {
				h := len(p) / 2
				_, _ = conn.Write(serverFrame(opText, false, p[:h]))
				_, _ = conn.Write(serverFrame(opContinuation, true, p[h:]))
			} else {
				_, _ = conn.Write(serverFrame(opText, true, p))
			}
		}
	}
}

func serverFrame(op byte, fin bool, p []byte) []byte {
	b := []byte{op}
	if fin {
		b[0] |= 0x80
	}
	n := len(p)
	switch {
	case n < 126:
		b = append(b, byte(n))
	case n < 1<<16:
		b = append(b, 126, byte(n>>8), byte(n))
	default:
		b = append(b, 127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	return append(b, p...)
}

func dial(t *testing.T, srv *httptest.Server) *Conn {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/devtools/browser/x")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEchoSizes(t *testing.T) {
	srv := echoServer(t, false)
	defer srv.Close()
	c := dial(t, srv)
	defer c.Close()
	for _, n := range []int{0, 1, 125, 126, 65535, 65536, 300000} {
		msg := strings.Repeat("x", n)
		if err := c.WriteMessage([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		got, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if string(got) != msg {
			t.Fatalf("n=%d: mismatch (%d bytes back)", n, len(got))
		}
	}
}

func TestFragmentedRead(t *testing.T) {
	srv := echoServer(t, true)
	defer srv.Close()
	c := dial(t, srv)
	defer c.Close()
	msg := `{"id":1,"result":{"value":"hello world"}}`
	if err := c.WriteMessage([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadMessage()
	if err != nil || string(got) != msg {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestCloseHandshake(t *testing.T) {
	srv := echoServer(t, false)
	defer srv.Close()
	c := dial(t, srv)
	if err := c.WriteMessage([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage([]byte("b")); err != ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestBadScheme(t *testing.T) {
	if _, err := Dial(context.Background(), "ftp://x"); err == nil {
		t.Fatal("expected error")
	}
}
