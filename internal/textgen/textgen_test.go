package textgen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phanngoc/browser-ai/internal/jev"
	"github.com/phanngoc/browser-ai/internal/snapshot"
)

func server(t *testing.T, content string, check func(body map[string]any)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if check != nil {
			check(body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
}

func ctxFixture() Context {
	page := &snapshot.Page{Title: "T", Text: "visible"}
	txt := "x"
	return FieldContext("Type Zurich", snapshot.Action{Label: "Where from?", Role: "combobox", Value: ""}, page,
		[]jev.Recent{{Action: "a"}, {Action: "b", Text: &txt}})
}

func TestTextValid(t *testing.T) {
	srv := server(t, `{"text":"Zurich"}`, func(b map[string]any) {
		if b["response_format"].(map[string]any)["type"] != "json_object" {
			t.Error("response_format missing")
		}
		if r, ok := b["reasoning"].(map[string]any); !ok || r["enabled"] != false {
			t.Errorf("reasoning=none not applied: %v", b["reasoning"])
		}
		msgs := b["messages"].([]any)
		var user Context
		_ = json.Unmarshal([]byte(msgs[1].(map[string]any)["content"].(string)), &user)
		if user.Field.Label != "Where from?" || len(user.RecentActions) != 2 || user.Page.Text != "visible" {
			t.Errorf("context %+v", user)
		}
	})
	defer srv.Close()
	c := New("k", srv.URL, "m", "none")
	v, meta, err := c.Text(context.Background(), ctxFixture())
	if err != nil || v != "Zurich" || meta.Usage.InputTokens != 5 || meta.Model != "m" {
		t.Fatalf("%q %+v %v", v, meta, err)
	}
}

func TestTextRejects(t *testing.T) {
	long := make([]byte, 2100)
	for i := range long {
		long[i] = 'a'
	}
	for name, content := range map[string]string{
		"null":       `{"text":null}`,
		"empty":      `{"text":"  "}`,
		"extra keys": `{"text":"a","note":"b"}`,
		"not json":   `Zurich`,
		"too long":   `{"text":"` + string(long) + `"}`,
		"number":     `{"text":5}`,
	} {
		srv := server(t, content, nil)
		c := New("k", srv.URL, "m", "none")
		_, _, err := c.Text(context.Background(), ctxFixture())
		srv.Close()
		if err != ErrNoValue {
			t.Errorf("%s: want ErrNoValue, got %v", name, err)
		}
	}
}

func TestNoKey(t *testing.T) {
	if _, _, err := New("", "", "", "").Text(context.Background(), ctxFixture()); err != ErrNoKey {
		t.Fatal(err)
	}
}

func TestContextEqualityForReuse(t *testing.T) {
	a, b := ctxFixture(), ctxFixture()
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("identical contexts must serialize identically")
	}
}
