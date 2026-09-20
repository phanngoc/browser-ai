// Package browser executes observed actions in an owned tab. Model output
// never becomes a selector, a coordinate, or code: every action refers to a
// node id the snapshot script assigned, and geometry is re-resolved and
// hit-tested immediately before input.
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"time"

	"github.com/phanngoc/browser-ai/internal/cdp"
	"github.com/phanngoc/browser-ai/internal/snapshot"
)

// ErrStale means the page no longer matches the observation a decision was
// made on. Observe again; nothing was executed.
var ErrStale = errors.New("browser: page changed since this decision; observe again")

// ErrSelectUnconfirmed means a native <select> change may or may not have
// fired. Do not retry blindly.
var ErrSelectUnconfirmed = errors.New("browser: dropdown execution was not confirmed; inspect before retrying")

// Timing records where browser time went.
type Timing struct {
	Settle   time.Duration // post-input wait before the snapshot
	Snapshot time.Duration // snapshot.js evaluate + decode, including stabilisation re-reads
	Act      time.Duration // freshness check + input dispatch
	// Restable counts how many extra snapshots Observe took before the page held still.
	Restable int
}

// Options for New.
type Options struct {
	Width, Height int
	LoadTimeout   time.Duration
	// StableChecks is how many times Observe re-reads the page (after two
	// animation frames) until two consecutive markers agree. Late-rendering
	// panels, autocomplete refreshes and post-navigation layout would
	// otherwise make the next decision stale. 0 uses the default of 3; -1 disables.
	StableChecks int
}

// Browser owns one page target.
type Browser struct {
	sess         *cdp.Session
	afterInput   *snapshot.Action
	stableChecks int
}

// New opens a background tab, applies viewport/focus emulation, navigates and
// waits for the document to be complete.
func New(ctx context.Context, conn *cdp.Conn, url string, opts Options) (*Browser, error) {
	w, h := opts.Width, opts.Height
	if w == 0 {
		w = 1120
	}
	if h == 0 {
		h = 780
	}
	if opts.LoadTimeout == 0 {
		opts.LoadTimeout = 15 * time.Second
	}
	sess, err := conn.NewPage(ctx, "about:blank", true)
	if err != nil {
		return nil, err
	}
	b := &Browser{sess: sess, stableChecks: opts.StableChecks}
	if b.stableChecks == 0 {
		b.stableChecks = 3
	}
	if _, err := sess.Call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false}); err != nil {
		b.Close(ctx)
		return nil, err
	}
	// Keep rAF and menus rendering in a tab the user is not looking at.
	_, _ = sess.Call(ctx, "Emulation.setFocusEmulationEnabled", map[string]any{"enabled": true})
	if err := sess.Navigate(ctx, url); err != nil {
		b.Close(ctx)
		return nil, err
	}
	lctx, cancel := context.WithTimeout(ctx, opts.LoadTimeout)
	defer cancel()
	if err := sess.WaitReady(lctx, 20*time.Millisecond); err != nil && ctx.Err() != nil {
		b.Close(ctx)
		return nil, err
	}
	return b, nil
}

// Session exposes the underlying CDP session.
func (b *Browser) Session() *cdp.Session { return b.sess }

// Close closes the owned tab.
func (b *Browser) Close(ctx context.Context) error { return b.sess.Close(ctx) }

// Observe waits for the page to settle after the previous input, then reads
// one atomic snapshot. Returns ErrStale if the document keeps navigating.
func (b *Browser) Observe(ctx context.Context) (*snapshot.Page, Timing, error) {
	var t Timing
	if a := b.afterInput; a != nil {
		b.afterInput = nil
		// Read-only, after execution was logged; a navigation may interrupt it.
		start := time.Now()
		_, _ = b.sess.Evaluate(ctx, settleJS(a), true)
		t.Settle = time.Since(start)
	}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		raw, err := b.sess.Evaluate(ctx, snapshot.Expr, false)
		var ex *cdp.ExceptionError
		switch {
		case errors.As(err, &ex), err == nil && (len(raw) == 0 || string(raw) == "null"):
			if attempt == 9 {
				return nil, t, fmt.Errorf("%w: document did not settle", ErrStale)
			}
			select {
			case <-ctx.Done():
				return nil, t, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			continue
		case err != nil:
			return nil, t, err
		}
		p, err := snapshot.Decode(raw)
		if err != nil {
			return nil, t, err
		}
		p, t.Restable, err = b.stabilize(ctx, p)
		if err != nil {
			return nil, t, err
		}
		t.Snapshot = time.Since(start)
		return p, t, nil
	}
}

// twoFrames resolves after two animation frames, or 50 ms if frames stall.
const twoFrames = `new Promise(r => { setTimeout(r, 50); requestAnimationFrame(() => requestAnimationFrame(r)); })`

// stabilize re-reads the page until two consecutive markers agree, so a
// decision is not made on a page that is still rendering. Bounded: on a page
// that never holds still it returns the latest read after stableChecks tries.
func (b *Browser) stabilize(ctx context.Context, p *snapshot.Page) (*snapshot.Page, int, error) {
	for i := 0; i < b.stableChecks; i++ {
		if _, err := b.sess.Evaluate(ctx, twoFrames, true); err != nil && ignoreException(err) != nil {
			return nil, i, err
		}
		raw, err := b.sess.Evaluate(ctx, snapshot.Expr, false)
		if err != nil || len(raw) == 0 || string(raw) == "null" {
			// Navigating: hand back what we have; the next Fresh check catches it.
			return p, i, ignoreException(err)
		}
		next, err := snapshot.Decode(raw)
		if err != nil {
			return nil, i, err
		}
		if jsonEqual(next.Marker, p.Marker) {
			return next, i, nil
		}
		p = next
	}
	return p, b.stableChecks, nil
}

// Fresh reports whether page still describes the live document. For click and
// select actions only the target's guard and the page key are compared, so
// unrelated animation does not force a new decision.
func (b *Browser) Fresh(ctx context.Context, page *snapshot.Page, action *snapshot.Action) (bool, error) {
	if action != nil && (action.Kind == "click" || action.Kind == "select") {
		if action.Node <= 0 {
			return false, nil
		}
		expr := fmt.Sprintf(`(() => { const c=window.__jevFast; return c ? [c.pageKey(), c.guard(c.nodes.get(%d))] : null; })()`, action.Node)
		raw, err := b.sess.Evaluate(ctx, expr, false)
		if err != nil {
			return false, ignoreException(err)
		}
		var cur []json.RawMessage
		if json.Unmarshal(raw, &cur) != nil || len(cur) != 2 {
			return false, nil
		}
		return jsonEqual(cur[0], page.PageKey) && jsonEqual(cur[1], page.Guards[strconv.Itoa(action.Node)]), nil
	}
	raw, err := b.sess.Evaluate(ctx, snapshot.MarkerExpr, false)
	if err != nil {
		return false, ignoreException(err)
	}
	return jsonEqual(raw, page.Marker), nil
}

func ignoreException(err error) error {
	var ex *cdp.ExceptionError
	if errors.As(err, &ex) {
		return nil
	}
	return err
}

func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// Act executes action against page. text is required for kind "fill".
func (b *Browser) Act(ctx context.Context, action snapshot.Action, page *snapshot.Page, text string) (t Timing, err error) {
	start := time.Now()
	defer func() { t.Act = time.Since(start) }()
	ok, err := b.Fresh(ctx, page, &action)
	if err != nil {
		return t, err
	}
	if !ok {
		return t, ErrStale
	}
	switch action.Kind {
	case "wait":
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		return t, nil
	case "scroll":
		_, err := b.sess.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": 550, "y": 650, "deltaX": 0, "deltaY": action.Delta})
		if err != nil {
			return t, err
		}
		b.afterInput = &action
		return t, nil
	case "click", "fill", "select":
	default:
		return t, fmt.Errorf("browser: unknown action kind %q", action.Kind)
	}
	if action.Node <= 0 {
		return t, errors.New("browser: invalid observed node")
	}
	raw, err := b.sess.Evaluate(ctx, resolveJS(action), false)
	if err != nil {
		if action.Kind == "select" {
			return t, ErrSelectUnconfirmed
		}
		if ignoreException(err) == nil {
			return t, ErrStale
		}
		return t, err
	}
	var pt struct{ X, Y float64 }
	if string(raw) == "null" || json.Unmarshal(raw, &pt) != nil {
		if action.Kind == "select" {
			return t, ErrSelectUnconfirmed
		}
		return t, fmt.Errorf("%w: target moved or is covered", ErrStale)
	}
	if action.Kind != "select" {
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			if _, err := b.sess.Call(ctx, "Input.dispatchMouseEvent", map[string]any{
				"type": typ, "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1}); err != nil {
				return t, err
			}
		}
		if action.Kind == "fill" {
			mod := 2 // Ctrl
			if runtime.GOOS == "darwin" {
				mod = 4 // Meta
			}
			if _, err := b.sess.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
				"type": "keyDown", "key": "a", "code": "KeyA", "modifiers": mod, "commands": []string{"selectAll"}}); err != nil {
				return t, err
			}
			if _, err := b.sess.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
				"type": "keyUp", "key": "a", "code": "KeyA", "modifiers": mod}); err != nil {
				return t, err
			}
			if _, err := b.sess.Call(ctx, "Input.insertText", map[string]any{"text": text}); err != nil {
				return t, err
			}
		}
	}
	b.afterInput = &action
	return t, nil
}

// Screenshot captures a JPEG of the viewport.
func (b *Browser) Screenshot(ctx context.Context) ([]byte, error) {
	res, err := b.sess.Call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 72})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Data)
}

func actionJSON(a snapshot.Action) string {
	b, _ := json.Marshal(a)
	return string(b)
}

// resolveJS re-checks the observed node and returns its centre, or null.
// For select it also applies the value inside the page.
func resolveJS(a snapshot.Action) string {
	return `(action => {
  const e=window.__jevFast?.nodes.get(action.node);
  if (!e?.isConnected || e.matches(':disabled') || e.closest('[aria-disabled="true"],[inert]') ||
      !e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true})) return null;
  if (action.kind==='fill' && (e.readOnly || e.getAttribute('aria-readonly')==='true')) return null;
  const r=e.getBoundingClientRect(), x=r.x+r.width/2, y=r.y+r.height/2;
  if (!r.width || !r.height || x<0 || y<0 || x>=innerWidth || y>=innerHeight) return null;
  if (!e.contains(document.elementFromPoint(x,y))) return null;
  if (action.kind==='select') {
    if (e.tagName!=='SELECT' || ![...e.options].some(o=>o.value===action.value &&
        !o.disabled && !o.closest('optgroup[disabled]'))) return null;
    e.value=action.value;
    e.dispatchEvent(new Event('input',{bubbles:true}));
    e.dispatchEvent(new Event('change',{bubbles:true}));
  }
  return {x,y};
})(` + actionJSON(a) + `)`
}

// settleJS waits ≤2 animation frames or 50 ms; after typing into an editable
// combobox it waits for visible options, capped at 200 ms.
func settleJS(a *snapshot.Action) string {
	return `(action => new Promise(resolve => {
  const field=window.__jevFast?.nodes.get(action.node);
  const autocomplete=action.kind==='fill' && field?.getAttribute('role')==='combobox';
  let frames=0, stopped=false;
  const finish=()=>{stopped=true;resolve()};
  setTimeout(finish,autocomplete ? 200 : 50);
  const ready=()=>{
    if (stopped) return;
    const ids=(field?.getAttribute('aria-controls')||field?.getAttribute('aria-owns')||'')
      .split(/\s+/).filter(Boolean);
    const roots=ids.length ? ids.map(id=>document.getElementById(id)).filter(Boolean) : [document];
    const options=roots.flatMap(root=>[...root.querySelectorAll('[role="option"]')]);
    if (++frames>=2 && (!autocomplete || options.some(e=>{
      const r=e.getBoundingClientRect();
      return r.width && r.height && r.bottom>0 && r.top<innerHeight &&
        e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true});
    }))) finish();
    else requestAnimationFrame(ready);
  };
  requestAnimationFrame(ready);
}))(` + actionJSON(*a) + `)`
}
