package snapshot

import "testing"

const raw = `{"url":"https://x/","title":"T","w":1120,"h":780,"text":"hello",
"scroll":{"y":0,"height":900},
"actions":[{"id":"e1","kind":"click","node":1,"role":"button","label":"Go","value":"","rect":{"x":1,"y":2,"w":3,"h":4}},
{"id":"e2","kind":"select","node":2,"role":"combobox","label":"Color → Red","value":"red","current_value":"Green","rect":{"x":1,"y":2,"w":3,"h":4}},
{"id":"scroll_down","kind":"scroll","label":"Scroll down","delta":560},{"id":"wait","kind":"wait","label":"Wait"}],
"marker":[1,"https://x/"],"page_key":[1],"guards":{"1":[1,"button","Go"]},"omitted_actions":0}`

func TestDecodeAndFingerprint(t *testing.T) {
	p, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 4 || p.Actions[1].CurrentValue != "Green" || p.Actions[2].Delta != 560 {
		t.Fatalf("%+v", p.Actions)
	}
	if p.Actions[0].Rect == nil || p.Actions[0].Rect.W != 3 {
		t.Fatal("rect not decoded")
	}
	if string(p.Guards["1"]) != `[1,"button","Go"]` {
		t.Fatalf("guards %s", p.Guards["1"])
	}
	fp := p.Fingerprint
	// Geometry must not affect the fingerprint; semantics must.
	moved, _ := Decode([]byte(replace(raw, `"x":1,"y":2`, `"x":9,"y":9`)))
	if moved.Fingerprint != fp {
		t.Error("fingerprint changed with geometry")
	}
	relabeled, _ := Decode([]byte(replace(raw, `"label":"Go"`, `"label":"Stop"`)))
	if relabeled.Fingerprint == fp {
		t.Error("fingerprint ignored label change")
	}
	scrolled, _ := Decode([]byte(replace(raw, `"y":0,"height"`, `"y":5,"height"`)))
	if scrolled.Fingerprint == fp {
		t.Error("fingerprint ignored scroll change")
	}
	if MarkerExpr == "" || Expr == "" {
		t.Fatal("script not embedded")
	}
}

func replace(s, old, nu string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + nu
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
