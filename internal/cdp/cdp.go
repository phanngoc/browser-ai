// Package cdp is a minimal Chrome DevTools Protocol client.
//
// One reader goroutine decodes only the message envelope (id, sessionId,
// method, error); result and event payloads stay as raw JSON for the caller.
// Sessions are flat (Target.attachToTarget flatten:true). Event subscribers
// have bounded buffers and drop when full, so a slow consumer can never stall
// the reader.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Transport moves whole JSON messages in both directions.
type Transport interface {
	Read() ([]byte, error)
	Write(p []byte) error
	Close() error
}

// Error is a protocol-level error returned by the browser.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("cdp: %s (code %d)", e.Message, e.Code) }

// ErrClosed is returned once the connection is gone.
var ErrClosed = errors.New("cdp: connection closed")

type envelope struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}

type request struct {
	ID        int64  `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    error
}

// Event is a protocol notification.
type Event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

// Conn multiplexes calls and events over one Transport.
type Conn struct {
	t      Transport
	nextID atomic.Int64
	calls  atomic.Int64
	drops  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan reply
	subs    map[string][]*Subscription
	err     error
	done    chan struct{}
}

// NewConn starts reading from t immediately.
func NewConn(t Transport) *Conn {
	c := &Conn{
		t:       t,
		pending: make(map[int64]chan reply),
		subs:    make(map[string][]*Subscription),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Calls reports how many requests have been sent.
func (c *Conn) Calls() int64 { return c.calls.Load() }

// DroppedEvents reports events discarded because a subscriber buffer was full.
func (c *Conn) DroppedEvents() int64 { return c.drops.Load() }

// Done is closed when the connection fails or is closed.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns the terminal error after Done is closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) readLoop() {
	for {
		data, err := c.t.Read()
		if err != nil {
			c.fail(err)
			return
		}
		var m envelope
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ok {
				if m.Error != nil {
					ch <- reply{err: m.Error}
				} else {
					ch <- reply{result: m.Result}
				}
			}
			continue
		}
		if m.Method != "" {
			c.dispatch(Event{SessionID: m.SessionID, Method: m.Method, Params: m.Params})
		}
	}
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	close(c.done)
	pending := c.pending
	c.pending = map[int64]chan reply{}
	subs := c.subs
	c.subs = map[string][]*Subscription{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: err}
	}
	for _, list := range subs {
		for _, s := range list {
			s.closeOnce()
		}
	}
}

// Close shuts the transport down and fails every pending call.
func (c *Conn) Close() error {
	err := c.t.Close()
	c.fail(ErrClosed)
	return err
}

// Call sends method with params on sessionID ("" for the browser target) and
// waits for the reply.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	data, err := json.Marshal(request{ID: id, SessionID: sessionID, Method: method, Params: params})
	if err != nil {
		c.drop(id)
		return nil, err
	}
	c.calls.Add(1)
	if err := c.t.Write(data); err != nil {
		c.drop(id)
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.drop(id)
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}
}

func (c *Conn) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Subscription receives events for one (session, method) pair. Method "*"
// matches every event on the session.
type Subscription struct {
	C    <-chan Event
	ch   chan Event
	key  string
	conn *Conn

	mu     sync.RWMutex // guards ch against send-after-close
	closed bool
}

func subKey(sessionID, method string) string { return sessionID + "\x00" + method }

// Subscribe registers a buffered subscriber. Events are dropped when the
// buffer is full.
func (c *Conn) Subscribe(sessionID, method string, buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	s := &Subscription{C: ch, ch: ch, key: subKey(sessionID, method), conn: c}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		close(ch)
		return s
	}
	c.subs[s.key] = append(c.subs[s.key], s)
	c.mu.Unlock()
	return s
}

// Cancel removes the subscription and closes C.
func (s *Subscription) Cancel() {
	c := s.conn
	c.mu.Lock()
	list := c.subs[s.key]
	for i, x := range list {
		if x == s {
			c.subs[s.key] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(c.subs[s.key]) == 0 {
		delete(c.subs, s.key)
	}
	c.mu.Unlock()
	s.closeOnce()
}

func (s *Subscription) closeOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// deliver enqueues without blocking; false when full or closed.
func (s *Subscription) deliver(ev Event) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

func (c *Conn) dispatch(ev Event) {
	c.mu.Lock()
	targets := append(append([]*Subscription(nil), c.subs[subKey(ev.SessionID, ev.Method)]...),
		c.subs[subKey(ev.SessionID, "*")]...)
	c.mu.Unlock()
	for _, s := range targets {
		if !s.deliver(ev) {
			c.drops.Add(1)
		}
	}
}
