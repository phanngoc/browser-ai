// Package llmchooser is a stand-in for Jev: a general chat LLM picks the
// operation and target from the same indexed action space. It is slower and
// pricier than Jev; it exists so the loop can run on real sites without a
// TypeSafe key. Validation is identical: only offered indices can execute.
package llmchooser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/phanngoc/browser-ai/internal/jev"
	"github.com/phanngoc/browser-ai/internal/snapshot"
)

// ErrInvalidAnswer means the LLM did not pick an offered operation/target.
var ErrInvalidAnswer = errors.New("llmchooser: invalid answer after retry; no action executed")

// Client is an OpenAI-compatible chooser.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Key     string
	Model   string
	// MaxText caps the visible page text sent per decision.
	MaxText int
}

// New builds a client. Defaults: OpenRouter, gemini-2.5-flash-lite.
func New(key, base, model string) *Client {
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	if model == "" {
		model = "google/gemini-2.5-flash-lite"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	return &Client{HTTP: &http.Client{Transport: tr, Timeout: 40 * time.Second},
		BaseURL: strings.TrimRight(base, "/"), Key: key, Model: model, MaxText: 4000}
}

// Warm opens the connection early.
func (c *Client) Warm(ctx context.Context) time.Duration {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil); err == nil {
		if resp, err := c.HTTP.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return time.Since(start)
}

const system = `You control a web browser to accomplish the user's goal, one operation per turn.
You see the page's visible text, an indexed table of interactive elements with their current values,
and the actions already taken. Reply with JSON only, no prose, exactly:
{"operation": "<OPERATION>", "target": "<element index or null>", "reason": "<one short sentence>"}
"target" is required for CLICK, TYPE_TEXT and SELECT and must be one of the indices offered for that
operation. For SELECT use the option id "index:option". For TYPE_TEXT do not write the text; a separate
helper writes it. For SCROLL_UP, SCROLL_DOWN, WAIT, DONE, BLOCKED set "target" to null.

Rules:
` + jev.NextActionRules + "\n" + jev.TargetRules

type answer struct {
	Operation string          `json:"operation"`
	RawTarget json.RawMessage `json:"target"`
	Reason    string          `json:"reason"`
	Target    *string
	parseErr  error
}

// normalize accepts "2", 2, "[2]", "3:1" or null as the target.
func (a *answer) normalize() {
	raw := strings.TrimSpace(string(a.RawTarget))
	if raw == "" || raw == "null" {
		return
	}
	var s string
	if json.Unmarshal(a.RawTarget, &s) != nil {
		var n float64
		if json.Unmarshal(a.RawTarget, &n) != nil {
			s = raw
		} else {
			s = strings.TrimSuffix(fmt.Sprintf("%.0f", n), ".0")
		}
	}
	a.Target = &s
}

// Choose implements agent.Chooser.
func (c *Client) Choose(ctx context.Context, page *snapshot.Page, goal string, history []jev.Recent) (*jev.Decision, error) {
	if c.Key == "" {
		return nil, errors.New("llmchooser: TEXT_MODEL_API_KEY is not set")
	}
	sp := jev.ActionSpace(page.Actions)
	prompt := c.render(sp, page, goal, history)
	messages := []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": prompt}}
	start := time.Now()
	var usage jev.Usage
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ans, raw, u, err := c.ask(ctx, messages)
		usage.InputTokens += u.InputTokens
		usage.OutputTokens += u.OutputTokens
		if err != nil {
			return nil, err
		}
		var d *jev.Decision
		verr := ans.parseErr
		if verr == nil {
			d, verr = toDecision(sp, ans)
		}
		if verr == nil {
			d.Model = c.Model
			d.Usage = usage
			d.Latency = time.Since(start)
			d.Request, _ = json.Marshal(map[string]any{"model": c.Model, "messages": messages})
			d.RawAnswers = raw
			return d, nil
		}
		lastErr = verr
		messages = append(messages,
			map[string]string{"role": "assistant", "content": string(raw)},
			map[string]string{"role": "user", "content": "Invalid: " + verr.Error() + ". Reply again with valid JSON using only offered operations and indices."})
	}
	return nil, fmt.Errorf("%w: %v", ErrInvalidAnswer, lastErr)
}

// requestBody adapts to the provider: OpenRouter takes a "reasoning" object
// and "max_tokens"; api.openai.com wants "max_completion_tokens", rejects
// "temperature" on gpt-5 models and takes "reasoning_effort" instead.
func (c *Client) requestBody(messages []map[string]string) map[string]any {
	body := map[string]any{
		"model":           c.Model,
		"response_format": map[string]string{"type": "json_object"},
		"messages":        messages,
	}
	if strings.Contains(c.BaseURL, "api.openai.com") {
		body["max_completion_tokens"] = 300
		if strings.HasPrefix(c.Model, "gpt-5") || strings.HasPrefix(c.Model, "o") {
			body["reasoning_effort"] = lowestReasoning(c.Model)
		} else {
			body["temperature"] = 0
		}
		return body
	}
	body["max_tokens"] = 300
	body["temperature"] = 0
	body["reasoning"] = map[string]any{"enabled": false}
	return body
}

// lowestReasoning is the cheapest reasoning_effort a model accepts:
// gpt-5.4 and later take "none", earlier gpt-5 / o-series take "minimal".
func lowestReasoning(model string) string {
	for _, p := range []string{"gpt-5.4", "gpt-5.5", "gpt-5.6", "gpt-5.7", "gpt-5.8", "gpt-5.9", "gpt-6"} {
		if strings.HasPrefix(model, p) {
			return "none"
		}
	}
	return "minimal"
}

func (c *Client) ask(ctx context.Context, messages []map[string]string) (*answer, json.RawMessage, jev.Usage, error) {
	body, _ := json.Marshal(c.requestBody(messages))
	data, err := jev.PostJSON(ctx, c.HTTP, c.BaseURL+"/chat/completions", c.Key, body)
	if err != nil {
		return nil, nil, jev.Usage{}, err
	}
	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &res) != nil || len(res.Choices) == 0 {
		return nil, nil, jev.Usage{}, errors.New("llmchooser: empty completion")
	}
	u := jev.Usage{InputTokens: res.Usage.PromptTokens, OutputTokens: res.Usage.CompletionTokens}
	content := strings.TrimSpace(res.Choices[0].Message.Content)
	if i, j := strings.Index(content, "{"), strings.LastIndex(content, "}"); i >= 0 && j > i {
		content = content[i : j+1]
	}
	var a answer
	if err := json.Unmarshal([]byte(content), &a); err != nil {
		a.parseErr = fmt.Errorf("reply is not a JSON object (%v): %s", err, snip(content, 300))
	}
	a.normalize()
	return &a, json.RawMessage(content), u, nil
}

func toDecision(sp jev.Space, a *answer) (*jev.Decision, error) {
	op := strings.ToUpper(strings.TrimSpace(a.Operation))
	offered := sp.OperationNames()
	found := false
	for _, o := range offered {
		if o == op {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("operation %q not offered (offered: %s); reply: %s", a.Operation, strings.Join(offered, ", "), snip(a.Reason, 200))
	}
	d := &jev.Decision{Operation: op, Confidence: 1, Probabilities: map[string]float64{},
		OperationProbability: map[string]float64{op: 1}}
	if candidates, ok := sp.Targets[op]; ok {
		if a.Target == nil {
			return nil, fmt.Errorf("%s needs a target index", op)
		}
		t := strings.Trim(strings.TrimSpace(*a.Target), "[]")
		act, ok := candidates[t]
		if !ok {
			ids := make([]string, 0, len(candidates))
			for id := range candidates {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return nil, fmt.Errorf("target %q not offered for %s (offered: %s)", *a.Target, op, strings.Join(ids, ", "))
		}
		d.Target, d.Choice = t, act.ID
		d.TargetConfidence = 1
		d.TargetProbability = map[string]float64{t: 1}
	} else if act, ok := sp.Controls[op]; ok {
		d.Choice = act.ID
	} else {
		d.Choice = op
	}
	d.Probabilities[d.Choice] = 1
	return d, nil
}

func (c *Client) render(sp jev.Space, page *snapshot.Page, goal string, history []jev.Recent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GOAL: %s\n\nPAGE: %s\nURL: %s\n\n", goal, page.Title, page.URL)
	text := page.Text
	if len(text) > c.MaxText {
		text = text[:c.MaxText] + "…"
	}
	fmt.Fprintf(&b, "VISIBLE TEXT:\n%s\n\nELEMENTS:\n", text)
	for _, e := range sp.Elements {
		fmt.Fprintf(&b, "[%s] %s %q", e.Index, e.Role, e.Label)
		if e.Value != "" {
			fmt.Fprintf(&b, " value=%q", e.Value)
		}
		for k, v := range map[string]string{"checked": e.Checked, "selected": e.Selected, "expanded": e.Expanded} {
			if v != "" {
				fmt.Fprintf(&b, " %s=%s", k, v)
			}
		}
		fmt.Fprintf(&b, " ops=%s", strings.Join(e.Operations, ","))
		if len(e.Options) > 0 {
			b.WriteString(" options:")
			for _, o := range e.Options {
				_, label, _ := strings.Cut(o.Label, " → ")
				fmt.Fprintf(&b, " %s=%q", o.Index, label)
			}
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\nOPERATIONS AVAILABLE: %s\n", strings.Join(sp.OperationNames(), ", "))
	if page.Scroll.Height > 0 {
		fmt.Fprintf(&b, "SCROLL: y=%.0f of %.0f\n", page.Scroll.Y, page.Scroll.Height)
	}
	b.WriteString("\nRECENT ACTIONS:")
	if len(history) == 0 {
		b.WriteString(" none")
	}
	b.WriteByte('\n')
	for i, h := range history {
		fmt.Fprintf(&b, "%d. %s %q", i+1, strings.ToUpper(h.Kind), h.Action)
		if h.Text != nil {
			fmt.Fprintf(&b, " text=%q", *h.Text)
		}
		if h.PageChanged != nil {
			if *h.PageChanged {
				b.WriteString(" (page changed)")
			} else {
				b.WriteString(" (no change)")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func snip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
