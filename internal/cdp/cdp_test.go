package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeTransport scripts the browser side.
type fakeTransport struct {
	in     chan []byte // messages the client reads
	out    chan []byte // messages the client wrote
	closed chan struct{}
}

func newFake() *fakeTransport {
	return &fakeTransport{in: make(chan []byte, 16), out: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakeTransport) Read() ([]byte, error) {
	select {
	case m := <-f.in:
		return m, nil
	case <-f.closed:
		return nil, errors.New("eof")
	}
}
func (f *fakeTransport) Write(p []byte) error { f.out <- append([]byte(nil), p...); return nil }
func (f *fakeTransport) Close() error         { close(f.closed); return nil }

func (f *fakeTransport) nextRequest(t *testing.T) request {
	t.Helper()
	select {
	case m := <-f.out:
		var r request
		if err := json.Unmarshal(m, &r); err != nil {
			t.Fatal(err)
		}
		return r
	case <-time.After(time.Second):
		t.Fatal("no request written")
	}
	return request{}
}

func TestCallRoundTrip(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	defer c.Close()
	go func() {
		r := f.nextRequest(t)
		if r.Method != "Browser.getVersion" || r.SessionID != "" {
			t.Errorf("bad request %+v", r)
		}
		f.in <- []byte(`{"id":` + itoa(r.ID) + `,"result":{"product":"Chrome/1"}}`)
	}()
	res, err := c.Call(context.Background(), "", "Browser.getVersion", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"product":"Chrome/1"}` {
		t.Fatalf("got %s", res)
	}
	if c.Calls() != 1 {
		t.Fatalf("calls=%d", c.Calls())
	}
}

func TestOutOfOrderRepliesAndSession(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	defer c.Close()
	s := c.Session("S1", "T1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		r1 := f.nextRequest(t)
		r2 := f.nextRequest(t)
		if r1.SessionID != "S1" || r2.SessionID != "S1" {
			t.Errorf("sessionId not propagated: %+v %+v", r1, r2)
		}
		// reply to the second first
		f.in <- []byte(`{"id":` + itoa(r2.ID) + `,"sessionId":"S1","result":{"v":2}}`)
		f.in <- []byte(`{"id":` + itoa(r1.ID) + `,"sessionId":"S1","result":{"v":1}}`)
	}()
	type out struct {
		res json.RawMessage
		err error
	}
	ch1, ch2 := make(chan out, 1), make(chan out, 1)
	go func() { r, e := s.Call(context.Background(), "A", nil); ch1 <- out{r, e} }()
	time.Sleep(10 * time.Millisecond)
	go func() { r, e := s.Call(context.Background(), "B", nil); ch2 <- out{r, e} }()
	o1, o2 := <-ch1, <-ch2
	<-done
	if o1.err != nil || o2.err != nil {
		t.Fatal(o1.err, o2.err)
	}
	if string(o1.res) != `{"v":1}` || string(o2.res) != `{"v":2}` {
		t.Fatalf("mismatched: %s %s", o1.res, o2.res)
	}
}

func TestProtocolError(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	defer c.Close()
	go func() {
		r := f.nextRequest(t)
		f.in <- []byte(`{"id":` + itoa(r.ID) + `,"error":{"code":-32601,"message":"nope"}}`)
	}()
	_, err := c.Call(context.Background(), "", "X", nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != -32601 {
		t.Fatalf("want protocol error, got %v", err)
	}
}

func TestContextTimeoutRemovesPending(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, "", "Slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending leaked: %d", n)
	}
}

func TestEventsAndDrop(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	defer c.Close()
	sub := c.Subscribe("S1", "Page.loadEventFired", 2)
	all := c.Subscribe("S1", "*", 8)
	other := c.Subscribe("S2", "Page.loadEventFired", 8)
	for i := 0; i < 4; i++ {
		f.in <- []byte(`{"method":"Page.loadEventFired","sessionId":"S1","params":{"i":` + itoa(int64(i)) + `}}`)
	}
	f.in <- []byte(`{"method":"Runtime.consoleAPICalled","sessionId":"S1","params":{}}`)
	time.Sleep(30 * time.Millisecond)
	if got := len(sub.C); got != 2 {
		t.Fatalf("buffered %d, want 2 (rest dropped)", got)
	}
	if got := len(all.C); got != 5 {
		t.Fatalf("wildcard got %d, want 5", got)
	}
	if len(other.C) != 0 {
		t.Fatal("event leaked across sessions")
	}
	if c.DroppedEvents() != 2 {
		t.Fatalf("drops=%d", c.DroppedEvents())
	}
	ev := <-sub.C
	if ev.Method != "Page.loadEventFired" || string(ev.Params) != `{"i":0}` {
		t.Fatalf("bad event %+v", ev)
	}
	sub.Cancel()
	if _, ok := <-sub.C; ok {
		// drain remaining then expect closed
		for range sub.C {
		}
	}
}

func TestTransportFailureFailsPending(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	errc := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "", "X", nil); errc <- err }()
	f.nextRequest(t)
	f.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected error")
		}
	case <-time.After(time.Second):
		t.Fatal("pending call not failed")
	}
	<-c.Done()
	if _, err := c.Call(context.Background(), "", "Y", nil); err == nil {
		t.Fatal("call after failure should error")
	}
}

func TestEvaluateException(t *testing.T) {
	f := newFake()
	c := NewConn(f)
	defer c.Close()
	s := c.Session("S", "T")
	go func() {
		r := f.nextRequest(t)
		f.in <- []byte(`{"id":` + itoa(r.ID) + `,"result":{"result":{"type":"object","subtype":"error"},"exceptionDetails":{"text":"Uncaught","exception":{"description":"ReferenceError: x"}}}}`)
	}()
	_, err := s.Evaluate(context.Background(), "x", false)
	var ee *ExceptionError
	if !errors.As(err, &ee) || ee.Exception.Description != "ReferenceError: x" {
		t.Fatalf("got %v", err)
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }
