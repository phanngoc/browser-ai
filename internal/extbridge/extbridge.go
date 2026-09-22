// Package extbridge is the agent side of the extension bridge: a loopback
// WebSocket server the browser extension connects to. The accepted connection
// is a cdp.Transport; the extension turns each message into a
// chrome.debugger call and streams events back.
package extbridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/ws"
)

// Hello is the first message the extension sends.
type Hello struct {
	Version   string `json:"version"`
	UserAgent string `json:"ua"`
	Browser   string `json:"browser,omitempty"`
}

// Client is an accepted extension connection.
type Client struct {
	cdp.Transport
	Hello  Hello
	Origin string
	conn   *ws.Conn
	done   chan struct{}
}

// Close stops the keep-alive pings and closes the connection.
func (c *Client) Close() error {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return c.conn.Close()
}

// Server listens for one extension at a time.
type Server struct {
	Addr  string
	Token string

	ln      net.Listener
	srv     *http.Server
	mu      sync.Mutex
	active  *Client
	pending chan *Client
}

// NewToken returns a random hex token.
func NewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Listen binds a loopback address ("127.0.0.1:9223" when addr is empty).
func Listen(addr, token string) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:9223"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("extbridge: refusing to listen on non-loopback address %q", addr)
	}
	if token == "" {
		return nil, errors.New("extbridge: token required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Addr: ln.Addr().String(), Token: token, ln: ln, pending: make(chan *Client)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ext", s.handle)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "browser-ai extension bridge; connect the extension to ws://"+s.Addr+"/ext")
	})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// URL is what the extension should connect to.
func (s *Server) URL() string { return "ws://" + s.Addr + "/ext?token=" + s.Token }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.Token)) != 1 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !strings.HasPrefix(o, "chrome-extension://") && !strings.HasPrefix(o, "moz-extension://") {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	busy := s.active != nil
	s.mu.Unlock()
	if busy {
		http.Error(w, "another client is connected", http.StatusConflict)
		return
	}
	c, err := ws.Upgrade(w, r)
	if err != nil {
		return
	}
	// First frame must be hello.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := c.ReadMessage()
	_ = c.SetReadDeadline(time.Time{})
	var env struct {
		Hello *Hello `json:"hello"`
	}
	if err != nil || json.Unmarshal(first, &env) != nil || env.Hello == nil {
		c.Close()
		return
	}
	client := &Client{Transport: c, Hello: *env.Hello, Origin: r.Header.Get("Origin"), conn: c, done: make(chan struct{})}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		c.Close()
		return
	}
	s.active = client
	s.mu.Unlock()
	go s.keepAlive(client)
	select {
	case s.pending <- client:
	case <-time.After(30 * time.Second):
		s.release(client)
		c.Close()
	}
}

// keepAlive pings every 20 s so the MV3 service worker is not put to sleep
// mid-run, and clears the slot when the connection dies.
func (s *Server) keepAlive(c *Client) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			s.release(c)
			return
		case <-t.C:
			if err := c.conn.Ping([]byte("browser-ai")); err != nil {
				s.release(c)
				return
			}
		}
	}
}

func (s *Server) release(c *Client) {
	s.mu.Lock()
	if s.active == c {
		s.active = nil
	}
	s.mu.Unlock()
}

// Accept waits for the extension to connect and say hello.
func (s *Server) Accept(ctx context.Context) (*Client, error) {
	select {
	case c := <-s.pending:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops listening and drops the active client.
func (s *Server) Close() error {
	s.mu.Lock()
	c := s.active
	s.active = nil
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
	return s.srv.Close()
}
