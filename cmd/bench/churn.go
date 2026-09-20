package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/phanngoc/browser-ai/internal/browser"
	"github.com/phanngoc/browser-ai/internal/snapshot"
)

// benchChurn observes a page repeatedly without touching it and reports which
// freshness inputs change on their own: marker, page_key and per-node guards.
// A page that churns here will make decisions go stale.
// fill is "LABEL=TEXT": before measuring, type TEXT into the first editable
// field whose label contains LABEL (empty LABEL → first editable field), using
// the real executor, so autocomplete dropdowns and re-renders are exercised.
// check is a click label: after fill, repeatedly observe, wait interval (a
// model call's worth of time) and run the executor's freshness check for that
// click, printing which guard/page_key parts changed — the exact stale path.
func benchChurn(ctx context.Context, n int, interval time.Duration, url, attach string, headless bool, fill, check string) error {
	var e *endpoint
	var err error
	if attach != "" {
		e, err = attachTo(ctx, attach)
	} else {
		e, err = launch(ctx, false, headless)
	}
	if err != nil {
		return err
	}
	defer e.close()
	br, err := browser.New(ctx, e.conn, url, browser.Options{})
	if err != nil {
		return err
	}
	defer br.Close(context.Background())
	time.Sleep(500 * time.Millisecond)
	if fill != "" {
		label, text, _ := strings.Cut(fill, "=")
		p, _, err := br.Observe(ctx)
		if err != nil {
			return err
		}
		var target *snapshot.Action
		for i, a := range p.Actions {
			if a.Kind == "fill" && strings.Contains(strings.ToLower(a.Label), strings.ToLower(label)) {
				target = &p.Actions[i]
				break
			}
		}
		if target == nil {
			return fmt.Errorf("churn: no editable field matching %q", label)
		}
		if _, err := br.Act(ctx, *target, p, text); err != nil {
			return fmt.Errorf("churn: fill %q: %w", target.Label, err)
		}
		fmt.Printf("typed %q into [%s] %s\n", text, target.ID, target.Label)
	}

	if check != "" {
		return churnCheck(ctx, br, n, interval, check)
	}
	var prev *snapshot.Page
	markerChanges, keyChanges := 0, 0
	keyReasons := map[string]int{}
	guardChanges := map[string]int{} // label → count
	guardReasons := map[string]int{} // label:field → count
	nodeChurn := 0                   // nodes present before, gone now
	labels := map[string]string{}
	for i := 0; i < n; i++ {
		p, _, err := br.Observe(ctx)
		if err != nil {
			time.Sleep(interval)
			continue
		}
		for _, a := range p.Actions {
			if a.Node > 0 {
				labels[fmt.Sprint(a.Node)] = a.Role + " " + a.Label
			}
		}
		if prev != nil {
			if !jsonEq(prev.Marker, p.Marker) {
				markerChanges++
			}
			if !jsonEq(prev.PageKey, p.PageKey) {
				keyChanges++
				for _, r := range pageKeyDiff(prev.PageKey, p.PageKey) {
					keyReasons[r]++
				}
			}
			for node, g := range prev.Guards {
				cur, ok := p.Guards[node]
				if !ok {
					nodeChurn++
					continue
				}
				if !jsonEq(g, cur) {
					guardChanges[labels[node]]++
					for _, f := range guardDiff(g, cur) {
						guardReasons[labels[node]+" · "+f]++
					}
				}
			}
		}
		prev = p
		time.Sleep(interval)
	}
	fmt.Printf("%s\n%s · %d observations, %s apart\n", e.name, url, n, interval)
	fmt.Printf("marker changed   %d/%d   (DONE/BLOCKED freshness)\n", markerChanges, n-1)
	fmt.Printf("page_key changed %d/%d   (click/fill/select freshness)\n", keyChanges, n-1)
	for _, k := range sortedKeys(keyReasons) {
		fmt.Printf("    %-40s %d\n", k, keyReasons[k])
	}
	fmt.Printf("nodes vanished   %d      (observed node no longer connected → stale)\n", nodeChurn)
	fmt.Println("guards changed:")
	for _, k := range sortedKeys(guardReasons) {
		fmt.Printf("    %-70s %d\n", trunc(k, 70), guardReasons[k])
	}
	return nil
}

var guardFields = []string{"identity", "role", "name", "value", "checked", "selectedIndex", "readOnly", "disabled", "aria-disabled", "aria-expanded", "aria-checked", "aria-selected", "href", "scopeText"}

func guardDiff(a, b json.RawMessage) []string {
	var x, y []any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil || len(x) != len(y) {
		return []string{"shape"}
	}
	var out []string
	for i := range x {
		if !reflect.DeepEqual(x[i], y[i]) {
			name := fmt.Sprint(i)
			if i < len(guardFields) {
				name = guardFields[i]
			}
			out = append(out, name)
		}
	}
	return out
}

// pageKey = [timeOrigin, href, scrollX, scrollY, innerWidth, innerHeight, [[id,value,checked,selectedIndex,disabled,readOnly]…]]
func pageKeyDiff(a, b json.RawMessage) []string {
	var x, y []any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil || len(x) < 7 || len(y) < 7 {
		return []string{"shape"}
	}
	names := []string{"timeOrigin", "href", "scrollX", "scrollY", "innerWidth", "innerHeight"}
	var out []string
	for i := 0; i < 6; i++ {
		if !reflect.DeepEqual(x[i], y[i]) {
			out = append(out, names[i])
		}
	}
	fx, _ := x[6].([]any)
	fy, _ := y[6].([]any)
	if len(fx) != len(fy) {
		out = append(out, fmt.Sprintf("form control count %d→%d", len(fx), len(fy)))
	} else {
		for i := range fx {
			if !reflect.DeepEqual(fx[i], fy[i]) {
				ex, _ := fx[i].([]any)
				ey, _ := fy[i].([]any)
				if len(ex) > 0 && len(ey) > 0 && !reflect.DeepEqual(ex[0], ey[0]) {
					out = append(out, "form control identity (node re-created)")
				} else {
					out = append(out, "form control value/state")
				}
			}
		}
	}
	return out
}

func jsonEq(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return m[ks[i]] > m[ks[j]] || (m[ks[i]] == m[ks[j]] && ks[i] < ks[j]) })
	return ks
}

func churnCheck(ctx context.Context, br *browser.Browser, n int, interval time.Duration, label string) error {
	stale := 0
	for i := 0; i < n; i++ {
		p, tm, err := br.Observe(ctx)
		if err != nil {
			return err
		}
		var act *snapshot.Action
		for j, a := range p.Actions {
			if a.Kind == "click" && strings.Contains(strings.ToLower(a.Label), strings.ToLower(label)) {
				act = &p.Actions[j]
				break
			}
		}
		if act == nil {
			fmt.Printf("%2d  no click matching %q among %d actions\n", i+1, label, len(p.Actions))
			time.Sleep(interval)
			continue
		}
		time.Sleep(interval) // stand-in for the model call
		expr := fmt.Sprintf(`(() => { const c=window.__jevFast; return c ? [c.pageKey(), c.guard(c.nodes.get(%d))] : null; })()`, act.Node)
		raw, err := br.Session().Evaluate(ctx, expr, false)
		if err != nil {
			fmt.Printf("%2d  evaluate: %v\n", i+1, err)
			continue
		}
		var cur []json.RawMessage
		_ = json.Unmarshal(raw, &cur)
		fresh, why := true, []string{}
		if len(cur) != 2 {
			fresh, why = false, []string{"no cache/node"}
		} else {
			if !jsonEq(cur[0], p.PageKey) {
				fresh = false
				why = append(why, "page_key:"+strings.Join(pageKeyDiff(p.PageKey, cur[0]), ","))
			}
			if string(cur[1]) == "null" {
				fresh = false
				why = append(why, "guard:null (node disconnected or hidden)")
			} else if !jsonEq(cur[1], p.Guards[fmt.Sprint(act.Node)]) {
				fresh = false
				why = append(why, "guard:"+strings.Join(guardDiff(p.Guards[fmt.Sprint(act.Node)], cur[1]), ","))
			}
		}
		if fresh {
			pt, err := br.Resolve(ctx, *act)
			if err == nil && pt == nil {
				fresh = false
				why = append(why, "hit-test: covered / off-screen / hidden")
				covered, _ := br.Session().Evaluate(ctx, fmt.Sprintf(`(() => { const e=window.__jevFast?.nodes.get(%d); if(!e) return null; const r=e.getBoundingClientRect(); const t=document.elementFromPoint(r.x+r.width/2, r.y+r.height/2); return t ? t.tagName+'#'+t.id+'.'+String(t.className).slice(0,40) : null; })()`, act.Node), false)
				why = append(why, "by "+string(covered))
			}
		}
		if !fresh {
			stale++
		}
		fmt.Printf("%2d  [%s] node %d %-28q snapshot %3dms  fresh=%-5v %s\n", i+1, act.ID, act.Node, trunc(act.Label, 28), tm.Snapshot.Milliseconds(), fresh, strings.Join(why, " "))
	}
	fmt.Printf("stale %d/%d\n", stale, n)
	return nil
}
