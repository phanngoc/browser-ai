package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/phanngoc/browser-ai/internal/snapshot"
)

func fixturePage() *snapshot.Page {
	return &snapshot.Page{URL: "https://x/", Title: "T", Text: "hello", Actions: []snapshot.Action{
		{ID: "e1", Kind: "fill", Node: 1, Role: "textbox", Label: "Name", Value: "old"},
		{ID: "e2", Kind: "click", Node: 1, Role: "textbox", Label: "Open Name", Value: "old"},
		{ID: "e3", Kind: "click", Node: 2, Role: "button", Label: "Submit"},
		{ID: "e4", Kind: "select", Node: 3, Role: "combobox", Label: "Color → Red", Value: "red", CurrentValue: "Green"},
		{ID: "e5", Kind: "select", Node: 3, Role: "combobox", Label: "Color → Blue", Value: "blue", CurrentValue: "Green"},
		{ID: "scroll_down", Kind: "scroll", Label: "Scroll down", Delta: 560},
		{ID: "wait", Kind: "wait", Label: "Wait for the page to update"},
	}}
}

func TestActionSpace(t *testing.T) {
	sp := ActionSpace(fixturePage().Actions)
	if len(sp.Elements) != 3 {
		t.Fatalf("elements %d", len(sp.Elements))
	}
	name := sp.Elements[0]
	if name.Index != "1" || strings.Join(name.Operations, ",") != "TYPE_TEXT,CLICK" || name.Value != "old" {
		t.Errorf("%+v", name)
	}
	color := sp.Elements[2]
	if color.Label != "Color" || color.Value != "Green" || len(color.Options) != 2 || color.Options[1].Index != "3:2" {
		t.Errorf("%+v", color)
	}
	if sp.Targets["CLICK"]["1"].ID != "e2" || sp.Targets["TYPE_TEXT"]["1"].ID != "e1" || sp.Targets["SELECT"]["3:1"].ID != "e4" {
		t.Errorf("targets %+v", sp.Targets)
	}
	if sp.Controls["SCROLL_DOWN"].ID != "scroll_down" || sp.Controls["WAIT"].ID != "wait" {
		t.Errorf("controls %+v", sp.Controls)
	}
	if got := strings.Join(sp.OperationNames(), ","); got != "CLICK,SCROLL_DOWN,SELECT,TYPE_TEXT,WAIT,DONE,BLOCKED" {
		t.Errorf("ops %s", got)
	}
}

func TestBuildRequestShape(t *testing.T) {
	body, _ := BuildRequest("jev-latest", fixturePage(), "do it", nil)
	data, _ := json.Marshal(body)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	q := m["questions"].(map[string]any)
	for _, k := range []string{"operation", "click_target", "type_text_target", "select_target"} {
		if q[k] == nil {
			t.Errorf("missing question %s", k)
		}
	}
	op := q["operation"].(map[string]any)["criteria"].(map[string]any)
	for _, k := range []string{"CLICK", "TYPE_TEXT", "SELECT", "SCROLL_DOWN", "WAIT", "DONE", "BLOCKED"} {
		if op[k] == nil {
			t.Errorf("missing operation %s", k)
		}
	}
	sel := q["select_target"].(map[string]any)["criteria"].(map[string]any)["3:2"].(map[string]any)
	if sel["element"] != "[3:2] Color → Blue" || sel["current_value"] != "Green" {
		t.Errorf("%v", sel)
	}
	st := m["state"].(map[string]any)
	if st["recent_actions"] == nil || st["page"].(map[string]any)["text"] != "hello" {
		t.Errorf("state %v", st)
	}
}

func probs(ids []string, pick string) map[string]float64 {
	m := map[string]float64{}
	rest := 0.1 / float64(len(ids)-1)
	for _, id := range ids {
		m[id] = rest
	}
	m[pick] = 0.9
	return m
}

func serve(t *testing.T, handler func(q map[string]any) map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		ans := handler(req["questions"].(map[string]any))
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-test", "answers": ans, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1}})
	}))
}

func keys(q map[string]any, name string) []string {
	var ids []string
	for k := range q[name].(map[string]any)["criteria"].(map[string]any) {
		ids = append(ids, k)
	}
	return ids
}

func TestChooseTargetHead(t *testing.T) {
	srv := serve(t, func(q map[string]any) map[string]any {
		return map[string]any{
			"operation":        map[string]any{"choice": "SELECT", "probabilities": probs(keys(q, "operation"), "SELECT"), "confidence": 0.8},
			"select_target":    map[string]any{"choice": "3:2", "probabilities": probs(keys(q, "select_target"), "3:2"), "confidence": 0.7},
			"click_target":     map[string]any{"choice": "nonsense"}, // unused head may be garbage
			"type_text_target": map[string]any{"choice": "1", "probabilities": map[string]float64{"1": 1}, "confidence": 1},
		}
	})
	defer srv.Close()
	c := New("k", "")
	c.Endpoint = srv.URL
	d, err := c.Choose(context.Background(), fixturePage(), "pick blue", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Choice != "e5" || d.Operation != "SELECT" || d.Target != "3:2" || d.Probabilities["e5"] != 0.9 || d.Model != "jev-test" || d.Usage.InputTokens != 10 {
		t.Fatalf("%+v", d)
	}
}

func TestChooseControlAndDone(t *testing.T) {
	for _, op := range []string{"WAIT", "DONE", "SCROLL_DOWN"} {
		srv := serve(t, func(q map[string]any) map[string]any {
			return map[string]any{"operation": map[string]any{"choice": op, "probabilities": probs(keys(q, "operation"), op), "confidence": 0.9}}
		})
		c := New("k", "")
		c.Endpoint = srv.URL
		d, err := c.Choose(context.Background(), fixturePage(), "g", []Recent{{Action: "x", Kind: "click"}})
		srv.Close()
		if err != nil {
			t.Fatal(op, err)
		}
		want := map[string]string{"WAIT": "wait", "DONE": "DONE", "SCROLL_DOWN": "scroll_down"}[op]
		if d.Choice != want || d.Target != "" {
			t.Errorf("%s → %+v", op, d)
		}
	}
}

func TestInvalidAnswersRejected(t *testing.T) {
	cases := map[string]func(ids []string) map[string]any{
		"choice not offered": func(ids []string) map[string]any {
			return map[string]any{"choice": "FLY", "probabilities": probs(ids, "CLICK"), "confidence": 0.5}
		},
		"missing probability": func(ids []string) map[string]any {
			p := probs(ids, "CLICK")
			delete(p, "WAIT")
			return map[string]any{"choice": "CLICK", "probabilities": p, "confidence": 0.5}
		},
		"sum off": func(ids []string) map[string]any {
			p := probs(ids, "CLICK")
			p["CLICK"] = 0.5
			return map[string]any{"choice": "CLICK", "probabilities": p, "confidence": 0.5}
		},
		"choice not argmax": func(ids []string) map[string]any {
			return map[string]any{"choice": "WAIT", "probabilities": probs(ids, "CLICK"), "confidence": 0.5}
		},
		"confidence out of range": func(ids []string) map[string]any {
			return map[string]any{"choice": "CLICK", "probabilities": probs(ids, "CLICK"), "confidence": 1.5}
		},
		"missing head": func(ids []string) map[string]any { return nil },
	}
	for name, mk := range cases {
		srv := serve(t, func(q map[string]any) map[string]any {
			a := mk(keys(q, "operation"))
			if a == nil {
				return map[string]any{}
			}
			return map[string]any{"operation": a}
		})
		c := New("k", "")
		c.Endpoint = srv.URL
		_, err := c.Choose(context.Background(), fixturePage(), "g", nil)
		srv.Close()
		if err != ErrInvalidAnswer {
			t.Errorf("%s: want ErrInvalidAnswer, got %v", name, err)
		}
	}
}

func TestRetryOn429ThenHTTPError(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()
	c := New("k", "")
	c.Endpoint = srv.URL
	_, err := c.Choose(context.Background(), fixturePage(), "g", nil)
	he, ok := err.(*HTTPError)
	if !ok || he.Status != 422 || n.Load() != 3 {
		t.Fatalf("got %v after %d attempts", err, n.Load())
	}
}
