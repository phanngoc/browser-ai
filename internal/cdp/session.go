package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Session is a flat session bound to one target.
type Session struct {
	conn     *Conn
	ID       string
	TargetID string
}

// Session wraps an existing sessionId.
func (c *Conn) Session(id, targetID string) *Session {
	return &Session{conn: c, ID: id, TargetID: targetID}
}

// Conn returns the underlying connection.
func (s *Session) Conn() *Conn { return s.conn }

// Call sends a command on this session.
func (s *Session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.conn.Call(ctx, s.ID, method, params)
}

// Subscribe listens for events on this session.
func (s *Session) Subscribe(method string, buffer int) *Subscription {
	return s.conn.Subscribe(s.ID, method, buffer)
}

// NewPage creates a page target and attaches to it with a flat session.
// background=true opens it without stealing the user's active tab.
func (c *Conn) NewPage(ctx context.Context, url string, background bool) (*Session, error) {
	if url == "" {
		url = "about:blank"
	}
	res, err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": url, "background": background})
	if err != nil {
		return nil, fmt.Errorf("createTarget: %w", err)
	}
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(res, &t); err != nil {
		return nil, err
	}
	res, err = c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true})
	if err != nil {
		return nil, fmt.Errorf("attachToTarget: %w", err)
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &a); err != nil {
		return nil, err
	}
	return c.Session(a.SessionID, t.TargetID), nil
}

// Close closes the page target owned by this session.
func (s *Session) Close(ctx context.Context) error {
	if s.TargetID == "" {
		return nil
	}
	_, err := s.conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": s.TargetID})
	return err
}

// ExceptionError is a JavaScript exception thrown during Evaluate.
type ExceptionError struct {
	Text        string `json:"text"`
	Description string `json:"description"`
	Exception   *struct {
		Description string `json:"description"`
	} `json:"exception"`
}

func (e *ExceptionError) Error() string {
	if e.Exception != nil && e.Exception.Description != "" {
		return "cdp: evaluate: " + e.Exception.Description
	}
	return "cdp: evaluate: " + e.Text
}

// Evaluate runs expression with returnByValue and returns the raw value.
// A thrown exception is returned as *ExceptionError.
func (s *Session) Evaluate(ctx context.Context, expression string, awaitPromise bool) (json.RawMessage, error) {
	res, err := s.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *ExceptionError `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	if out.ExceptionDetails != nil {
		return nil, out.ExceptionDetails
	}
	return out.Result.Value, nil
}

// Navigate loads url in the session's page.
func (s *Session) Navigate(ctx context.Context, url string) error {
	res, err := s.Call(ctx, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return err
	}
	var out struct {
		ErrorText string `json:"errorText"`
	}
	_ = json.Unmarshal(res, &out)
	if out.ErrorText != "" {
		return fmt.Errorf("navigate: %s", out.ErrorText)
	}
	return nil
}

// WaitReady polls document.readyState until "complete" or ctx expires.
func (s *Session) WaitReady(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		v, err := s.Evaluate(ctx, "document.readyState", false)
		if err == nil && string(v) == `"complete"` {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
