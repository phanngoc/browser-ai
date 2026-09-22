package extbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/ws"
)

func listen(t *testing.T) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1:0", "tok")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fakeExtension connects like the real one and answers CDP calls.
func fakeExtension(t *testing.T, url string) *ws.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := ws.Dial(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage([]byte(`{"hello":{"version":"0.1","ua":"Chrome/153"}}`)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAcceptAndCall(t *testing.T) {
	s := listen(t)
	ext := fakeExtension(t, s.URL())
	defer ext.Close()
	go func() {
		for {
			msg, err := ext.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(msg, &req)
			_ = ext.WriteMessage([]byte(`{"method":"Page.loadEventFired","sessionId":"7","params":{}}`))
			_ = ext.WriteMessage([]byte(`{"id":` + itoa(req.ID) + `,"result":{"ok":"` + req.Method + `"}}`))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := s.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if client.Hello.Version != "0.1" || client.Hello.UserAgent != "Chrome/153" {
		t.Fatalf("hello %+v", client.Hello)
	}
	conn := cdp.NewConn(client)
	defer conn.Close()
	sub := conn.Subscribe("7", "Page.loadEventFired", 4)
	res, err := conn.Call(ctx, "7", "Runtime.evaluate", map[string]any{"expression": "1"})
	if err != nil || string(res) != `{"ok":"Runtime.evaluate"}` {
		t.Fatalf("%s %v", res, err)
	}
	select {
	case ev := <-sub.C:
		if ev.SessionID != "7" {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
}

func TestRejects(t *testing.T) {
	s := listen(t)
	base := "http://" + s.Addr + "/ext"
	// bad token
	if resp, _ := http.Get(base + "?token=nope"); resp == nil || resp.StatusCode != 401 {
		t.Fatalf("bad token: %v", resp)
	}
	// bad origin
	req, _ := http.NewRequest("GET", base+"?token=tok", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Upgrade", "websocket")
	if resp, _ := http.DefaultClient.Do(req); resp == nil || resp.StatusCode != 403 {
		t.Fatalf("bad origin: %v", resp)
	}
	// no hello → dropped
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := ws.Dial(ctx, s.URL())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.WriteMessage([]byte(`{"id":1,"method":"x"}`))
	if _, err := c.ReadMessage(); err == nil {
		t.Fatal("connection without hello should be closed")
	}
	// second client while one is active → 409
	ext := fakeExtension(t, s.URL())
	defer ext.Close()
	if _, err := s.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	if resp, _ := http.Get(base + "?token=tok"); resp == nil || resp.StatusCode != 409 {
		t.Fatalf("second client: %v", resp)
	}
}

func TestNonLoopbackRefused(t *testing.T) {
	if _, err := Listen("0.0.0.0:0", "tok"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("got %v", err)
	}
	if _, err := Listen("127.0.0.1:0", ""); err == nil {
		t.Fatal("empty token accepted")
	}
}

func itoa(i int64) string { b, _ := json.Marshal(i); return string(b) }
