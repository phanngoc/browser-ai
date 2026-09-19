package llmchooser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phanngoc/browser-ai/internal/snapshot"
)

func page() *snapshot.Page {
	return &snapshot.Page{URL: "https://x/", Title: "T", Text: "hello", Actions: []snapshot.Action{
		{ID: "e1", Kind: "fill", Node: 1, Role: "textbox", Label: "Name", Value: "old"},
		{ID: "e2", Kind: "click", Node: 1, Role: "textbox", Label: "Open Name", Value: "old"},
		{ID: "e3", Kind: "click", Node: 2, Role: "button", Label: "Submit"},
		{ID: "e4", Kind: "select", Node: 3, Role: "combobox", Label: "Color → Red", Value: "red", CurrentValue: "Green"},
		{ID: "scroll_down", Kind: "scroll", Label: "Scroll down", Delta: 560},
		{ID: "wait", Kind: "wait", Label: "Wait for the page to update"},
	}}
}

func server(t *testing.T, replies ...string) (*httptest.Server, *[]string) {
	var prompts []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		prompts = append(prompts, msgs[len(msgs)-1].(map[string]any)["content"].(string))
		reply := replies[min(n, len(replies)-1)]
		n++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 10},
		})
	}))
	return srv, &prompts
}

func TestChooseTargetAndPrompt(t *testing.T) {
	srv, prompts := server(t, "```json\n{\"operation\":\"type_text\",\"target\":\"[1]\",\"reason\":\"fill name\"}\n```")
	defer srv.Close()
	c := New("k", srv.URL, "m")
	d, err := c.Choose(context.Background(), page(), "Enter Zurich", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Choice != "e1" || d.Operation != "TYPE_TEXT" || d.Target != "1" || d.Probabilities["e1"] != 1 || d.Usage.InputTokens != 100 {
		t.Fatalf("%+v", d)
	}
	p := (*prompts)[0]
	for _, want := range []string{`[1] textbox "Name" value="old" ops=TYPE_TEXT,CLICK`, `[3] combobox "Color" value="Green" ops=SELECT options: 3:1="Red"`,
		"OPERATIONS AVAILABLE: CLICK, SCROLL_DOWN, SELECT, TYPE_TEXT, WAIT, DONE, BLOCKED", "GOAL: Enter Zurich"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestRetryOnInvalidThenValid(t *testing.T) {
	srv, prompts := server(t, `{"operation":"CLICK","target":"9"}`, `{"operation":"SELECT","target":"3:1"}`)
	defer srv.Close()
	d, err := New("k", srv.URL, "m").Choose(context.Background(), page(), "g", nil)
	if err != nil || d.Choice != "e4" || d.Target != "3:1" {
		t.Fatalf("%+v %v", d, err)
	}
	if len(*prompts) != 2 || !strings.Contains((*prompts)[1], `target "9" not offered for CLICK (offered: 1, 2)`) {
		t.Fatalf("feedback prompt wrong: %v", *prompts)
	}
	if d.Usage.InputTokens != 200 {
		t.Errorf("usage should sum both calls: %+v", d.Usage)
	}
}

func TestInvalidTwiceFails(t *testing.T) {
	srv, _ := server(t, `{"operation":"FLY"}`, `not json at all`)
	defer srv.Close()
	_, err := New("k", srv.URL, "m").Choose(context.Background(), page(), "g", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid answer") {
		t.Fatalf("got %v", err)
	}
}

func TestControlsAndDone(t *testing.T) {
	for reply, want := range map[string]string{
		`{"operation":"WAIT","target":null}`:   "wait",
		`{"operation":"scroll_down"}`:          "scroll_down",
		`{"operation":"DONE","target":"1"}`:    "DONE",
		`{"operation":"BLOCKED","reason":""}`:  "BLOCKED",
		`{"operation":"CLICK","target":2}`:     "e3",
		`{"operation":"CLICK","target":"[2]"}`: "e3",
	} {
		srv, _ := server(t, reply)
		d, err := New("k", srv.URL, "m").Choose(context.Background(), page(), "g", nil)
		srv.Close()
		if err != nil || d.Choice != want {
			t.Errorf("%s → %+v %v", reply, d, err)
		}
	}
}
