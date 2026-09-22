package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/phanngoc/browser-ai/internal/chrome"
	"github.com/phanngoc/browser-ai/internal/snapshot"
)

type env struct {
	t   *testing.T
	ctx context.Context
	b   *Browser
}

func setup(t *testing.T) *env { return setupWith(t, Options{}) }

func setupWith(t *testing.T, opts Options) *env {
	t.Helper()
	if _, err := chrome.FindBinary(); err != nil || os.Getenv("BROWSER_AI_SKIP_CHROME") != "" {
		t.Skip("no Chrome")
	}
	html, err := os.ReadFile("testdata/fixture.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/page2" {
			_, _ = w.Write([]byte("<title>Page 2</title><h1>Second</h1>"))
			return
		}
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
	b, err := New(ctx, ch.Conn, srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, b: b}
}

func (e *env) observe() *snapshot.Page {
	e.t.Helper()
	p, _, err := e.b.Observe(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) eval(expr string) string {
	e.t.Helper()
	v, err := e.b.Session().Evaluate(e.ctx, expr, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(v)
}

func find(t *testing.T, p *snapshot.Page, kind, label string) snapshot.Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Kind == kind && a.Label == label {
			return a
		}
	}
	var got []string
	for _, a := range p.Actions {
		got = append(got, a.Kind+":"+a.Label)
	}
	t.Fatalf("no action %s:%q in %v", kind, label, got)
	return snapshot.Action{}
}

func has(p *snapshot.Page, kind, label string) bool {
	for _, a := range p.Actions {
		if a.Kind == kind && a.Label == label {
			return true
		}
	}
	return false
}

func TestSnapshotClassification(t *testing.T) {
	e := setup(t)
	p := e.observe()
	if p.Title != "Fixture" {
		t.Fatalf("title %q", p.Title)
	}
	find(t, p, "fill", "Name")
	find(t, p, "click", "Open Name")
	find(t, p, "fill", "Search here")
	find(t, p, "fill", "Notes")
	find(t, p, "fill", "Rich")
	find(t, p, "click", "Submit")
	find(t, p, "click", "Go to page 2")
	find(t, p, "click", "Agree to terms")
	find(t, p, "click", "Option B")
	sel := find(t, p, "select", "Color → Red")
	if sel.Value != "red" || sel.CurrentValue != "Green" {
		t.Errorf("select %+v", sel)
	}
	for _, bad := range []string{"select:Color → Green", "select:Color → X", "select:Color → Blue",
		"fill:Secret", "fill:Disabled field", "fill:Readonly field", "fill:Agree to terms", "click:Hidden button"} {
		kv := strings.SplitN(bad, ":", 2)
		if has(p, kv[0], kv[1]) {
			t.Errorf("unexpected action %s", bad)
		}
	}
	cb := find(t, p, "click", "Agree to terms")
	if cb.Role != "checkbox" || cb.Checked != "false" {
		t.Errorf("checkbox %+v", cb)
	}
	if !has(p, "scroll", "Scroll down") || has(p, "scroll", "Scroll up") || !has(p, "wait", "Wait for the page to update") {
		t.Error("scroll/wait controls wrong")
	}
	if !strings.Contains(p.Text, "Fixture page") || !strings.Contains(p.Text, "Footer text v1") {
		t.Errorf("text missing: %q", p.Text)
	}
	if len(p.Fingerprint) != 64 || len(p.Marker) == 0 || len(p.Guards) == 0 {
		t.Error("fingerprint/marker/guards missing")
	}
	p2 := e.observe()
	if p2.Fingerprint != p.Fingerprint {
		t.Error("fingerprint unstable across identical observations")
	}
}

func TestObserveWaitsForLateRender(t *testing.T) {
	e := setupWith(t, Options{StableChecks: 3})
	// Content that appears shortly after the first read must be in the
	// observation, otherwise the decision made on it would be stale.
	e.eval(`setTimeout(() => { document.getElementById('late').textContent = 'Late panel rendered'; }, 25)`)
	p, tm, err := e.b.Observe(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "Late panel rendered") {
		t.Fatalf("late content missed (restable=%d): %q", tm.Restable, p.Text)
	}
	// Restable is 1 when the first read raced the timer and 0 when the
	// content had already landed; either way the observation is complete.
	t.Logf("restable=%d", tm.Restable)
	if ok, _ := e.b.Fresh(e.ctx, p, nil); !ok {
		t.Error("stabilised observation should be fresh")
	}
	// A page that holds still costs exactly one confirmation read.
	_, tm, err = e.b.Observe(e.ctx)
	if err != nil || tm.Restable != 0 {
		t.Fatalf("quiet page: restable=%d err=%v", tm.Restable, err)
	}
}

func TestFillReplacesAndSettles(t *testing.T) {
	e := setup(t)
	p := e.observe()
	a := find(t, p, "fill", "Name")
	if a.Value != "old value" {
		t.Fatalf("value %q", a.Value)
	}
	if _, err := e.b.Act(e.ctx, a, p, "Zurich"); err != nil {
		t.Fatal(err)
	}
	p = e.observe()
	if got := find(t, p, "fill", "Name").Value; got != "Zurich" {
		t.Fatalf("after fill value %q", got)
	}
}

func TestComboboxWaitsForOptions(t *testing.T) {
	e := setup(t)
	p := e.observe()
	a := find(t, p, "fill", "City")
	start := time.Now()
	if _, err := e.b.Act(e.ctx, a, p, "Zu"); err != nil {
		t.Fatal(err)
	}
	p, tm, err := e.b.Observe(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !has(p, "click", "Zurich") || !has(p, "click", "Zug") {
		t.Fatalf("options not observed after settle (%s): %v", tm.Settle, p.Actions)
	}
	if el := time.Since(start); el > 400*time.Millisecond {
		t.Errorf("settle too slow: %s", el)
	}
	if tm.Settle < 90*time.Millisecond {
		t.Errorf("settle did not wait for options: %s", tm.Settle)
	}
}

func TestSelectFiresChange(t *testing.T) {
	e := setup(t)
	p := e.observe()
	a := find(t, p, "select", "Color → Red")
	if _, err := e.b.Act(e.ctx, a, p, ""); err != nil {
		t.Fatal(err)
	}
	if v := e.eval("window.__changed"); v != `"red"` {
		t.Fatalf("change not fired: %s", v)
	}
	p = e.observe()
	if has(p, "select", "Color → Red") || !has(p, "select", "Color → Green") {
		t.Error("select options not re-derived from new value")
	}
}

func TestClickCheckboxAndSubmit(t *testing.T) {
	e := setup(t)
	p := e.observe()
	if _, err := e.b.Act(e.ctx, find(t, p, "click", "Agree to terms"), p, ""); err != nil {
		t.Fatal(err)
	}
	p = e.observe()
	if find(t, p, "click", "Agree to terms").Checked != "true" {
		t.Error("checkbox not toggled")
	}
	if _, err := e.b.Act(e.ctx, find(t, p, "click", "Submit"), p, ""); err != nil {
		t.Fatal(err)
	}
	if v := e.eval("window.__submitted"); v != "true" {
		t.Fatalf("submit not fired: %s", v)
	}
}

func TestNavigationClick(t *testing.T) {
	e := setup(t)
	p := e.observe()
	if _, err := e.b.Act(e.ctx, find(t, p, "click", "Go to page 2"), p, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p = e.observe()
		if p.Title == "Page 2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("navigation not observed: %q", p.Title)
		}
	}
	if !strings.HasSuffix(p.URL, "/page2") {
		t.Errorf("url %s", p.URL)
	}
}

func TestStaleGuards(t *testing.T) {
	e := setup(t)
	p := e.observe()
	submit := find(t, p, "click", "Submit")
	name := find(t, p, "fill", "Name")

	// Unrelated content change: click/fill guards still fresh, marker is not.
	e.eval(`document.getElementById('footer').textContent='Footer text v2'`)
	if ok, _ := e.b.Fresh(e.ctx, p, &submit); !ok {
		t.Error("click guard should ignore unrelated footer change")
	}
	if ok, _ := e.b.Fresh(e.ctx, p, &name); !ok {
		t.Error("fill guard should ignore unrelated footer change")
	}
	if ok, _ := e.b.Fresh(e.ctx, p, nil); ok {
		t.Error("marker should detect footer change")
	}
	// A change to the field's own label goes stale for fill too.
	e.eval(`document.querySelector('label[for=name]').textContent='Full name'`)
	if ok, _ := e.b.Fresh(e.ctx, p, &name); ok {
		t.Error("fill guard should detect target label change")
	}

	// Target label change: click guard goes stale, Act refuses.
	p = e.observe()
	submit = find(t, p, "click", "Submit")
	e.eval(`document.getElementById('submit').textContent='Delete everything'`)
	if ok, _ := e.b.Fresh(e.ctx, p, &submit); ok {
		t.Error("click guard should detect target label change")
	}
	if _, err := e.b.Act(e.ctx, submit, p, ""); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
	if v := e.eval("typeof window.__submitted"); v != `"undefined"` {
		t.Fatal("stale click must not execute")
	}

	// Form value change elsewhere in the same form: page key changes → stale.
	p = e.observe()
	submit = find(t, p, "click", "Delete everything")
	e.eval(`document.getElementById('name').value='typed by someone else'`)
	if ok, _ := e.b.Fresh(e.ctx, p, &submit); ok {
		t.Error("page key should detect form value change")
	}
}

func TestOcclusionRefused(t *testing.T) {
	e := setup(t)
	p := e.observe()
	submit := find(t, p, "click", "Submit")
	e.eval(`document.getElementById('overlay').style.display='block'`)
	_, err := e.b.Act(e.ctx, submit, p, "")
	if !errors.Is(err, ErrStale) {
		t.Fatalf("covered target must be refused, got %v", err)
	}
	if v := e.eval("typeof window.__submitted"); v != `"undefined"` {
		t.Fatal("covered click must not execute")
	}
}

func TestPartiallyCoveredTargetStillClickable(t *testing.T) {
	e := setup(t)
	p := e.observe()
	submit := find(t, p, "click", "Submit")
	// A floating widget over the centre of the button, like a chat bubble.
	e.eval(`(() => { const r=document.getElementById('submit').getBoundingClientRect(), w=document.getElementById('widget');
	  w.style.left=(r.x+r.width/2-20)+'px'; w.style.top=(r.y+r.height/2-20)+'px'; w.style.display='block'; })()`)
	if _, err := e.b.Act(e.ctx, submit, p, ""); err != nil {
		t.Fatalf("edge of a partially covered button should be clickable: %v", err)
	}
	if v := e.eval("typeof window.__submitted"); v != `"boolean"` {
		t.Fatal("click did not reach the button")
	}
}

func TestScroll(t *testing.T) {
	e := setup(t)
	p := e.observe()
	down := find(t, p, "scroll", "Scroll down")
	if _, err := e.b.Act(e.ctx, down, p, ""); err != nil {
		t.Fatal(err)
	}
	p = e.observe()
	if p.Scroll.Y <= 0 || !has(p, "scroll", "Scroll up") {
		t.Fatalf("scroll not applied: %+v", p.Scroll)
	}
}

func TestWaitAndScreenshot(t *testing.T) {
	e := setup(t)
	p := e.observe()
	w := find(t, p, "wait", "Wait for the page to update")
	e.eval(`document.getElementById('footer').textContent='changed while deciding'`)
	start := time.Now()
	if _, err := e.b.Act(e.ctx, w, p, ""); err != nil {
		t.Fatalf("wait must not go stale: %v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("wait too short")
	}
	img, err := e.b.Screenshot(e.ctx)
	if err != nil || len(img) < 1000 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Fatalf("screenshot: %d bytes %v", len(img), err)
	}
	var m any
	if err := json.Unmarshal(p.Marker, &m); err != nil {
		t.Fatal(err)
	}
}
