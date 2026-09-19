package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/phanngoc/browser-ai/internal/browser"
	"github.com/phanngoc/browser-ai/internal/chrome"
	"github.com/phanngoc/browser-ai/internal/jev"
	"github.com/phanngoc/browser-ai/internal/textgen"
)

// policy is a scripted stand-in for Jev: it reads the request's element table
// and answers with a valid distribution over the offered criteria.
type policy func(state map[string]any, questions map[string]any) (operation, target string)

func fakeJev(t *testing.T, p policy) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		state := req["state"].(map[string]any)
		questions := req["questions"].(map[string]any)
		op, target := p(state, questions)
		answers := map[string]any{"operation": dist(questions, "operation", op)}
		if target != "" {
			head := strings.ToLower(op) + "_target"
			answers[head] = dist(questions, head, target)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "answers": answers,
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 1}})
	}))
}

func dist(questions map[string]any, name, pick string) map[string]any {
	criteria := questions[name].(map[string]any)["criteria"].(map[string]any)
	probs := map[string]float64{}
	for k := range criteria {
		probs[k] = 0.05 / float64(len(criteria))
	}
	probs[pick] = 0.95
	return map[string]any{"choice": pick, "probabilities": probs, "confidence": 0.9}
}

func indexOf(state map[string]any, label string) string {
	for _, e := range state["elements"].([]any) {
		el := e.(map[string]any)
		if el["label"] == label {
			return el["index"].(string)
		}
	}
	return ""
}

func valueOf(state map[string]any, label string) string {
	for _, e := range state["elements"].([]any) {
		el := e.(map[string]any)
		if el["label"] == label {
			v, _ := el["value"].(string)
			return v
		}
	}
	return ""
}

func fakeText(t *testing.T, value string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `{"text":"` + value + `"}`}}},
			"usage":   map[string]int{"prompt_tokens": 50, "completion_tokens": 3},
		})
	}))
}

func setup(t *testing.T) (context.Context, *browser.Browser) {
	t.Helper()
	if _, err := chrome.FindBinary(); err != nil || os.Getenv("BROWSER_AI_SKIP_CHROME") != "" {
		t.Skip("no Chrome")
	}
	html, err := os.ReadFile("../browser/testdata/fixture.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ch, err := chrome.Launch(ctx, chrome.Options{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	br, err := browser.New(ctx, ch.Conn, srv.URL, browser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, br
}

func TestRunTypeClickDone(t *testing.T) {
	ctx, br := setup(t)
	jsrv := fakeJev(t, func(state, q map[string]any) (string, string) {
		recent := state["recent_actions"].([]any)
		switch {
		case valueOf(state, "Name") != "Zurich":
			return "TYPE_TEXT", indexOf(state, "Name")
		case len(recent) < 2:
			return "CLICK", indexOf(state, "Submit")
		default:
			return "DONE", ""
		}
	})
	defer jsrv.Close()
	tsrv := fakeText(t, "Zurich")
	defer tsrv.Close()
	jc := jev.New("k", "")
	jc.Endpoint = jsrv.URL
	tc := textgen.New("k", tsrv.URL, "m", "none")

	var seen []Step
	a := New(br, jc, tc, Options{Goal: "Enter Zurich as the name and submit the form.", OnStep: func(s Step) { seen = append(seen, s) }})
	res := a.Run(ctx)
	if res.Status != "done" {
		t.Fatalf("status %s reason %s err %v", res.Status, res.Reason, res.Err)
	}
	if len(res.Steps) != 2 || len(seen) != 2 {
		t.Fatalf("steps %d seen %d", len(res.Steps), len(seen))
	}
	s0, s1 := res.Steps[0], res.Steps[1]
	if s0.Operation != "TYPE_TEXT" || s0.Kind != "fill" || s0.Text == nil || *s0.Text != "Zurich" || s0.TextModel != "m" || s0.TextUsage.InputTokens != 50 {
		t.Errorf("step1 %+v", s0)
	}
	if s1.Operation != "CLICK" || s1.Action != "Submit" || s1.Text != nil {
		t.Errorf("step2 %+v", s1)
	}
	if s0.PageChanged == nil || !*s0.PageChanged {
		t.Error("typing should change the page fingerprint")
	}
	if s0.Timing.Model <= 0 || s0.Timing.Text <= 0 || s0.Timing.Snapshot <= 0 || s0.Timing.Act <= 0 || s0.Timing.Total < s0.Timing.Model {
		t.Errorf("timing %+v", s0.Timing)
	}
	if len(res.Decisions) != 3 || !res.Decisions[2].Executed || res.Decisions[2].Choice != "DONE" {
		t.Errorf("decisions %+v", res.Decisions)
	}
	if v, _ := br.Session().Evaluate(ctx, "document.getElementById('name').value+'|'+String(window.__submitted)", false); string(v) != `"Zurich|true"` {
		t.Fatalf("browser state %s", v)
	}
	if res.CDPCalls == 0 || res.Elapsed <= 0 || res.FinalTitle != "Fixture" {
		t.Errorf("result %+v", res)
	}
}

func TestBudgetAndRepeatedNoChange(t *testing.T) {
	ctx, br := setup(t)
	// Always click the readonly field: nothing changes → blocked after 3.
	jsrv := fakeJev(t, func(state, q map[string]any) (string, string) {
		return "CLICK", indexOf(state, "Readonly field")
	})
	defer jsrv.Close()
	jc := jev.New("k", "")
	jc.Endpoint = jsrv.URL
	res := New(br, jc, nil, Options{Goal: "g", MaxSteps: 10}).Run(ctx)
	if res.Status != "blocked" || len(res.Steps) != 3 || !strings.Contains(res.Reason, "three consecutive") {
		t.Fatalf("%s %s steps=%d", res.Status, res.Reason, len(res.Steps))
	}

	// WAIT is exempt from the no-change rule; the action budget stops it.
	jsrv2 := fakeJev(t, func(state, q map[string]any) (string, string) { return "WAIT", "" })
	defer jsrv2.Close()
	jc.Endpoint = jsrv2.URL
	res = New(br, jc, nil, Options{Goal: "g", MaxSteps: 4}).Run(ctx)
	if res.Status != "blocked" || len(res.Steps) != 4 || !strings.Contains(res.Reason, "budget") {
		t.Fatalf("%s %s steps=%d", res.Status, res.Reason, len(res.Steps))
	}
}

func TestTypeTextWithoutHelperFails(t *testing.T) {
	ctx, br := setup(t)
	jsrv := fakeJev(t, func(state, q map[string]any) (string, string) { return "TYPE_TEXT", indexOf(state, "Name") })
	defer jsrv.Close()
	jc := jev.New("k", "")
	jc.Endpoint = jsrv.URL
	res := New(br, jc, nil, Options{Goal: "g"}).Run(ctx)
	if res.Status != "error" || res.Err != textgen.ErrNoKey {
		t.Fatalf("%s %v", res.Status, res.Err)
	}
	if v, _ := br.Session().Evaluate(ctx, "document.getElementById('name').value", false); string(v) != `"old value"` {
		t.Fatal("nothing must be typed without a helper")
	}
}
