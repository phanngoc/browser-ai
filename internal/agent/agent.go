// Package agent runs the loop: observe → choose → (generate text) → act →
// settle → observe. Decisions are consumed once, execution is logged before
// the next observation, and a stale page never retries a mutation.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/phanngoc/browser-ai/internal/browser"
	"github.com/phanngoc/browser-ai/internal/jev"
	"github.com/phanngoc/browser-ai/internal/snapshot"
	"github.com/phanngoc/browser-ai/internal/textgen"
)

// Chooser picks the next operation and target.
type Chooser interface {
	Choose(ctx context.Context, page *snapshot.Page, goal string, history []jev.Recent) (*jev.Decision, error)
}

// TextGen writes a field value.
type TextGen interface {
	Text(ctx context.Context, fc textgen.Context) (string, textgen.Meta, error)
}

// Timing is where one step's wall time went.
type Timing struct {
	Snapshot time.Duration `json:"snapshot"`
	Model    time.Duration `json:"model"`
	Text     time.Duration `json:"text"`
	Act      time.Duration `json:"act"`
	Settle   time.Duration `json:"settle"`
	Total    time.Duration `json:"total"`
	Restable int           `json:"restable"` // extra snapshots taken until the page held still
}

// Step is one executed action.
type Step struct {
	N           int           `json:"n"`
	Operation   string        `json:"operation"`
	Target      string        `json:"target,omitempty"`
	Choice      string        `json:"choice"`
	Action      string        `json:"action"`
	Kind        string        `json:"kind"`
	Text        *string       `json:"text"`
	TextModel   string        `json:"text_model,omitempty"`
	Probability float64       `json:"probability"`
	Confidence  float64       `json:"confidence"`
	PageChanged *bool         `json:"page_changed"`
	URL         string        `json:"url"`
	Timing      Timing        `json:"timing"`
	Usage       jev.Usage     `json:"usage"`
	TextUsage   jev.Usage     `json:"text_usage"`
	Elapsed     time.Duration `json:"elapsed"`
	Screenshot  []byte        `json:"-"`
}

// DecisionRecord is every model call, including ones that went stale.
type DecisionRecord struct {
	Elapsed     time.Duration   `json:"elapsed"`
	Fingerprint string          `json:"fingerprint"`
	Operation   string          `json:"operation"`
	Target      string          `json:"target,omitempty"`
	Choice      string          `json:"choice"`
	Confidence  float64         `json:"confidence"`
	Latency     time.Duration   `json:"latency"`
	Executed    bool            `json:"executed"`
	Refused     string          `json:"refused,omitempty"` // why a non-executed decision was dropped
	Request     json.RawMessage `json:"request,omitempty"`
	Answers     json.RawMessage `json:"answers,omitempty"`
}

// Result summarises a run.
type Result struct {
	Status     string           `json:"status"` // done | blocked | error
	Reason     string           `json:"reason,omitempty"`
	Goal       string           `json:"goal"`
	StartURL   string           `json:"start_url"`
	FinalURL   string           `json:"final_url"`
	FinalTitle string           `json:"final_title"`
	Steps      []Step           `json:"steps"`
	Decisions  []DecisionRecord `json:"decisions"`
	Stale      int              `json:"stale"`
	Setup      time.Duration    `json:"setup"`   // first observation
	Elapsed    time.Duration    `json:"elapsed"` // from first decision to stop
	CDPCalls   int64            `json:"cdp_calls"`
	Err        error            `json:"-"`
}

// Options configure a run.
type Options struct {
	Goal        string
	MaxSteps    int // executed actions; model calls are capped at 2×
	Screenshots bool
	OnStep      func(Step)
	KeepRequest bool // retain request/answers JSON in DecisionRecord
	// RefuseLimit is how many consecutive refusals hide a target from the
	// model (default 2; 0 uses the default, -1 disables).
	RefuseLimit int
}

// Agent drives one browser toward one goal.
type Agent struct {
	br   *browser.Browser
	jev  Chooser
	text TextGen
	opts Options

	page    *snapshot.Page
	recent  []jev.Recent
	pending *pendingText
	res     Result
	started time.Time
	// refused counts consecutive executor refusals per target (node+kind).
	// A target refused RefuseLimit times is withheld from the next decision,
	// so the model cannot pick a covered button forever. Cleared on execution.
	refused map[string]int
}

type pendingText struct {
	key  string
	text string
	meta textgen.Meta
}

// New wires the pieces. The browser must already be on the start page.
func New(br *browser.Browser, chooser Chooser, text TextGen, opts Options) *Agent {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 60
	}
	if opts.RefuseLimit == 0 {
		opts.RefuseLimit = 2
	}
	return &Agent{br: br, jev: chooser, text: text, opts: opts, res: Result{Goal: opts.Goal}, refused: map[string]int{}}
}

// Run executes until DONE, BLOCKED, budget exhaustion or an error.
func (a *Agent) Run(ctx context.Context) *Result {
	if a.opts.Goal == "" {
		return a.fail(errors.New("agent: empty goal"))
	}
	t0 := time.Now()
	page, _, err := a.br.Observe(ctx)
	if err != nil {
		return a.fail(err)
	}
	a.page = page
	a.res.StartURL = page.URL
	a.res.Setup = time.Since(t0)
	a.started = time.Now()
	for {
		if len(a.res.Steps) >= a.opts.MaxSteps {
			return a.stop("blocked", fmt.Sprintf("reached the %d-action budget", a.opts.MaxSteps))
		}
		if len(a.res.Decisions) >= a.opts.MaxSteps*2 {
			return a.stop("blocked", "reached the model-call budget")
		}
		done, err := a.tick(ctx)
		if err != nil {
			return a.fail(err)
		}
		if done {
			return a.finish()
		}
	}
}

// tick performs one predict→act cycle. A stale page returns (false, nil) after
// re-observing, without executing anything.
func (a *Agent) tick(ctx context.Context) (bool, error) {
	stepStart := time.Now()
	var tm Timing
	page := a.page

	if fresh, err := a.br.Fresh(ctx, page, nil); err != nil {
		return false, err
	} else if !fresh {
		var err error
		if page, tm.Snapshot, err = a.observe(ctx, &tm); err != nil {
			return false, err
		}
		a.page = page
	}

	offered := a.withoutRefused(page)
	d, err := a.jev.Choose(ctx, offered, a.opts.Goal, a.recent)
	if err != nil {
		return false, err
	}
	tm.Model = d.Latency
	rec := DecisionRecord{Elapsed: time.Since(a.started), Fingerprint: page.Fingerprint, Operation: d.Operation,
		Target: d.Target, Choice: d.Choice, Confidence: d.Confidence, Latency: d.Latency}
	if a.opts.KeepRequest {
		rec.Request, rec.Answers = d.Request, d.RawAnswers
	}
	a.res.Decisions = append(a.res.Decisions, rec)
	recIdx := len(a.res.Decisions) - 1

	if d.Choice == "DONE" || d.Choice == "BLOCKED" {
		fresh, err := a.br.Fresh(ctx, page, nil)
		if err != nil {
			return false, err
		}
		if !fresh {
			return false, a.stale(ctx, recIdx, "page changed before "+d.Choice)
		}
		a.res.Decisions[recIdx].Executed = true
		a.res.Status = map[string]string{"DONE": "done", "BLOCKED": "blocked"}[d.Choice]
		return true, nil
	}

	action, ok := findAction(page.Actions, d.Choice)
	if !ok {
		return false, fmt.Errorf("agent: decision %q is not an observed action", d.Choice)
	}

	var text *string
	var textMeta textgen.Meta
	if action.Kind == "fill" {
		fresh, err := a.br.Fresh(ctx, page, nil)
		if err != nil {
			return false, err
		}
		if !fresh {
			return false, a.stale(ctx, recIdx, "page changed before text generation")
		}
		fc := textgen.FieldContext(a.opts.Goal, action, page, a.recent)
		key, _ := json.Marshal(fc)
		if a.pending != nil && a.pending.key == string(key) {
			text, textMeta = &a.pending.text, a.pending.meta
		} else {
			if a.text == nil {
				return false, textgen.ErrNoKey
			}
			v, meta, err := a.text.Text(ctx, fc)
			if errors.Is(err, textgen.ErrNoValue) {
				// One malformed reply is a model flake, not a page problem.
				var meta2 textgen.Meta
				v, meta2, err = a.text.Text(ctx, fc)
				meta.Latency += meta2.Latency
				meta.Usage.InputTokens += meta2.Usage.InputTokens
				meta.Usage.OutputTokens += meta2.Usage.OutputTokens
				meta.Model = meta2.Model
			}
			if err != nil {
				return false, err
			}
			tm.Text = meta.Latency
			a.pending = &pendingText{key: string(key), text: v, meta: meta}
			text, textMeta = &v, meta
		}
	}

	value := ""
	if text != nil {
		value = *text
	}
	bt, err := a.br.Act(ctx, action, page, value)
	tm.Act = bt.Act
	if errors.Is(err, browser.ErrStale) {
		a.refused[refuseKey(action)]++
		return false, a.stale(ctx, recIdx, err.Error())
	}
	if err != nil {
		return false, err
	}
	a.pending = nil
	a.refused = map[string]int{}
	a.res.Decisions[recIdx].Executed = true

	// Log execution before observing: a navigation must not erase the action.
	step := Step{N: len(a.res.Steps) + 1, Operation: d.Operation, Target: d.Target, Choice: d.Choice,
		Action: action.Label, Kind: action.Kind, Text: text, TextModel: textMeta.Model,
		Probability: d.Probabilities[d.Choice], Confidence: d.Confidence, URL: page.URL,
		Usage: d.Usage, TextUsage: textMeta.Usage, Elapsed: time.Since(a.started)}
	a.res.Steps = append(a.res.Steps, step)
	a.recent = append(a.recent, jev.Recent{Action: action.Label, Kind: action.Kind, Text: text})
	idx := len(a.res.Steps) - 1

	next, snap, err := a.observe(ctx, &tm)
	if err != nil {
		return false, err
	}
	tm.Snapshot += snap
	changed := next.Fingerprint != page.Fingerprint
	a.res.Steps[idx].PageChanged = &changed
	a.recent[idx].PageChanged = &changed
	a.res.Steps[idx].URL = next.URL
	a.page = next
	if a.opts.Screenshots {
		if img, err := a.br.Screenshot(ctx); err == nil {
			a.res.Steps[idx].Screenshot = img
		}
	}
	tm.Total = time.Since(stepStart)
	a.res.Steps[idx].Timing = tm
	a.res.Steps[idx].Elapsed = time.Since(a.started)
	if a.opts.OnStep != nil {
		a.opts.OnStep(a.res.Steps[idx])
	}

	if n := len(a.res.Steps); n >= 3 {
		stuck := true
		for _, s := range a.res.Steps[n-3:] {
			if s.Kind == "wait" || s.PageChanged == nil || *s.PageChanged {
				stuck = false
			}
		}
		if stuck {
			a.res.Status, a.res.Reason = "blocked", "three consecutive actions changed nothing"
			return true, nil
		}
	}
	return false, nil
}

func (a *Agent) observe(ctx context.Context, tm *Timing) (*snapshot.Page, time.Duration, error) {
	p, bt, err := a.br.Observe(ctx)
	if err != nil {
		return nil, 0, err
	}
	tm.Settle += bt.Settle
	tm.Restable += bt.Restable
	return p, bt.Snapshot, nil
}

func refuseKey(a snapshot.Action) string { return fmt.Sprintf("%d/%s/%s", a.Node, a.Kind, a.Label) }

// withoutRefused hides targets the executor has refused RefuseLimit times in
// a row. The returned page shares everything else with the original.
func (a *Agent) withoutRefused(page *snapshot.Page) *snapshot.Page {
	if a.opts.RefuseLimit < 0 || len(a.refused) == 0 {
		return page
	}
	kept := make([]snapshot.Action, 0, len(page.Actions))
	for _, act := range page.Actions {
		if a.refused[refuseKey(act)] >= a.opts.RefuseLimit {
			continue
		}
		kept = append(kept, act)
	}
	if len(kept) == len(page.Actions) {
		return page
	}
	copy := *page
	copy.Actions = kept
	return &copy
}

// stale drops the current decision and re-observes. Nothing was executed.
func (a *Agent) stale(ctx context.Context, recIdx int, why string) error {
	a.res.Decisions[recIdx].Refused = why
	a.res.Stale++
	p, _, err := a.br.Observe(ctx)
	if err != nil {
		return err
	}
	a.page = p
	return nil
}

func (a *Agent) stop(status, reason string) *Result {
	a.res.Status, a.res.Reason = status, reason
	return a.finish()
}

func (a *Agent) fail(err error) *Result {
	a.res.Status, a.res.Err, a.res.Reason = "error", err, err.Error()
	return a.finish()
}

func (a *Agent) finish() *Result {
	if !a.started.IsZero() {
		a.res.Elapsed = time.Since(a.started)
	}
	if a.page != nil {
		a.res.FinalURL, a.res.FinalTitle = a.page.URL, a.page.Title
	}
	a.res.CDPCalls = a.br.Session().Conn().Calls()
	return &a.res
}

func findAction(actions []snapshot.Action, id string) (snapshot.Action, bool) {
	for _, a := range actions {
		if a.ID == id {
			return a, true
		}
	}
	return snapshot.Action{}, false
}
