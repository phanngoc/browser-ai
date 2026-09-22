// Package ws is a minimal RFC 6455 WebSocket client (text frames, client
// masking, fragmentation, ping/pong, close). It exists so the agent can attach
// to a running Chrome without pulling in a dependency.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// ErrClosed is returned after the peer sent a close frame.
var ErrClosed = errors.New("ws: closed")

// Conn is a WebSocket connection. Client connections mask outgoing frames;
// server connections (from Upgrade) send them unmasked and require masked
// input, per RFC 6455.
type Conn struct {
	nc      net.Conn
	r       *bufio.Reader
	wmu     sync.Mutex
	closed  bool
	maxSize int64
	server  bool
}

// Upgrade completes the server side of the opening handshake on an HTTP
// request and returns the connection. The ResponseWriter must support
// hijacking. The caller has already checked path, token and Origin.
func Upgrade(w http.ResponseWriter, req *http.Request) (*Conn, error) {
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || req.Header.Get("Sec-WebSocket-Key") == "" {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("ws: not a websocket upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, errors.New("ws: hijack unsupported")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + guid))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := io.WriteString(nc, resp); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{nc: nc, r: rw.Reader, maxSize: 256 << 20, server: true}, nil
}

// Ping sends a ping control frame (used by servers to keep an extension's
// service worker awake).
func (c *Conn) Ping(payload []byte) error { return c.writeFrame(opPing, payload) }

// Dial connects to a ws:// or wss:// URL and completes the opening handshake.
func Dial(ctx context.Context, rawURL string) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var secure bool
	switch u.Scheme {
	case "ws", "http":
	case "wss", "https":
		secure = true
	default:
		return nil, fmt.Errorf("ws: unsupported scheme %q", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if secure {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	d := net.Dialer{}
	nc, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	if secure {
		tc := tls.Client(nc, &tls.Config{ServerName: u.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		nc.Close()
		return nil, err
	}
	keyB64 := base64.StdEncoding.EncodeToString(key)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&sb, "Host: %s\r\n", u.Host)
	sb.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&sb, "Sec-WebSocket-Key: %s\r\n", keyB64)
	sb.WriteString("Sec-WebSocket-Version: 13\r\n\r\n")
	if _, err := io.WriteString(nc, sb.String()); err != nil {
		nc.Close()
		return nil, err
	}
	r := bufio.NewReaderSize(nc, 1<<16)
	resp, err := http.ReadResponse(r, &http.Request{Method: "GET"})
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		nc.Close()
		return nil, fmt.Errorf("ws: handshake failed: %s", resp.Status)
	}
	sum := sha1.Sum([]byte(keyB64 + guid))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		nc.Close()
		return nil, errors.New("ws: bad Sec-WebSocket-Accept")
	}
	_ = nc.SetDeadline(time.Time{})
	return &Conn{nc: nc, r: r, maxSize: 256 << 20}, nil
}

// ReadMessage returns the next complete text or binary message. Control
// frames are handled internally.
func (c *Conn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opText, opBinary:
			if msg != nil {
				return nil, errors.New("ws: unexpected new data frame during fragmented message")
			}
			if fin {
				return payload, nil
			}
			msg = payload
		case opContinuation:
			if msg == nil {
				return nil, errors.New("ws: continuation without start")
			}
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
		case opClose:
			_ = c.writeFrame(opClose, payload)
			c.markClosed()
			return nil, ErrClosed
		default:
			return nil, fmt.Errorf("ws: unknown opcode %d", op)
		}
	}
}

// WriteMessage sends one text frame.
func (c *Conn) WriteMessage(p []byte) error { return c.writeFrame(opText, p) }

// Close sends a close frame and closes the socket.
func (c *Conn) Close() error {
	c.wmu.Lock()
	if !c.closed {
		c.closed = true
		_ = c.writeFrameLocked(opClose, []byte{0x03, 0xE8})
	}
	c.wmu.Unlock()
	return c.nc.Close()
}

func (c *Conn) markClosed() {
	c.wmu.Lock()
	c.closed = true
	c.wmu.Unlock()
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.r, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		err = errors.New("ws: reserved bits set")
		return
	}
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.r, b[:]); err != nil {
			return
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	if n < 0 || n > c.maxSize {
		err = fmt.Errorf("ws: frame too large (%d)", n)
		return
	}
	if c.server && !masked {
		err = errors.New("ws: client frame not masked")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.r, payload); err != nil {
		return
	}
	if masked {
		applyMask(payload, mask)
	}
	return
}

func (c *Conn) writeFrame(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return ErrClosed
	}
	return c.writeFrameLocked(op, p)
}

func (c *Conn) writeFrameLocked(op byte, p []byte) error {
	n := len(p)
	buf := make([]byte, 0, 14+n)
	buf = append(buf, 0x80|op)
	var maskBit byte
	if !c.server {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		buf = append(buf, maskBit|byte(n))
	case n < 1<<16:
		buf = append(buf, maskBit|126, byte(n>>8), byte(n))
	default:
		buf = append(buf, maskBit|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	if c.server {
		buf = append(buf, p...)
		_, err := c.nc.Write(buf)
		return err
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	buf = append(buf, mask[:]...)
	start := len(buf)
	buf = append(buf, p...)
	applyMask(buf[start:], mask)
	_, err := c.nc.Write(buf)
	return err
}

func applyMask(p []byte, mask [4]byte) {
	for i := range p {
		p[i] ^= mask[i&3]
	}
}

// SetReadDeadline bounds the next reads on the underlying socket.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.nc.SetReadDeadline(t) }

// Read implements cdp.Transport.
func (c *Conn) Read() ([]byte, error) { return c.ReadMessage() }

// Write implements cdp.Transport.
func (c *Conn) Write(p []byte) error { return c.WriteMessage(p) }
