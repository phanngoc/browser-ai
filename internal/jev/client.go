// Package jev talks to TypeSafe's System One endpoint: one request decides the
// operation and, speculatively, a target for every operation offered.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/phanngoc/browser-ai/internal/snapshot"
)

// DefaultEndpoint is the System One URL.
const DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"

// Recent is one executed action as sent to the model.
type Recent struct {
	Action      string  `json:"action"`
	Kind        string  `json:"kind"`
	Text        *string `json:"text"`
	PageChanged *bool   `json:"page_changed"`
}

// Usage is token accounting from the response.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Decision is a validated model answer.
type Decision struct {
	Choice               string             // action id (e7, scroll_down, wait) or DONE/BLOCKED
	Operation            string             // CLICK, TYPE_TEXT, SELECT, SCROLL_DOWN, SCROLL_UP, WAIT, DONE, BLOCKED
	Target               string             // element index, "" for non-target operations
	Confidence           float64            // operation head
	Probabilities        map[string]float64 // by action id, for the executed head
	OperationProbability map[string]float64
	TargetProbability    map[string]float64
	TargetConfidence     float64
	Model                string
	Usage                Usage
	Latency              time.Duration
	Request              json.RawMessage
	RawAnswers           json.RawMessage
}

// Client posts decisions to TypeSafe.
type Client struct {
	HTTP     *http.Client
	Endpoint string
	Key      string
	Model    string
}

// New builds a client with HTTP/2 keep-alive and a 25 s timeout.
func New(key, model string) *Client {
	if model == "" {
		model = "jev-latest"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.MaxIdleConnsPerHost = 4
	return &Client{HTTP: &http.Client{Transport: tr, Timeout: 25 * time.Second}, Endpoint: DefaultEndpoint, Key: key, Model: model}
}

// Warm opens the TLS/HTTP2 connection ahead of the first decision.
func (c *Client) Warm(ctx context.Context) time.Duration {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint, nil)
	if err != nil {
		return 0
	}
	if resp, err := c.HTTP.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return time.Since(start)
}

type choiceQuestion struct {
	Type         string `json:"type"`
	Criteria     any    `json:"criteria"`
	Instructions any    `json:"instructions"`
}

type answer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

// BuildRequest assembles the state and questions for one decision.
func BuildRequest(model string, page *snapshot.Page, goal string, history []Recent) (map[string]any, Space) {
	sp := ActionSpace(page.Actions)
	operations := map[string]string{}
	for op := range sp.Targets {
		operations[op] = operationLabels[op]
	}
	for op, a := range sp.Controls {
		operations[op] = a.Label
	}
	operations["DONE"] = "Every requirement is visibly satisfied."
	operations["BLOCKED"] = "No supported operation can progress."

	questions := map[string]any{
		"operation": choiceQuestion{Type: "choice", Criteria: operations,
			Instructions: map[string]any{"goal": goal, "rules": NextActionRules}},
	}
	for op, candidates := range sp.Targets {
		criteria := map[string]any{}
		for index, a := range candidates {
			crit := map[string]any{"element": fmt.Sprintf("[%s] %s", index, a.Label)}
			if a.CurrentValue != "" {
				crit["current_value"] = a.CurrentValue
			} else {
				crit["current_value"] = a.Value
			}
			if a.Role != "" {
				crit["role"] = a.Role
			}
			if a.Checked != "" {
				crit["checked"] = a.Checked
			}
			if a.Selected != "" {
				crit["selected"] = a.Selected
			}
			if a.Expanded != "" {
				crit["expanded"] = a.Expanded
			}
			criteria[index] = crit
		}
		questions[strings.ToLower(op)+"_target"] = choiceQuestion{Type: "choice", Criteria: criteria,
			Instructions: map[string]any{"goal": goal, "operation": op, "rules": []string{NextActionRules, TargetRules}}}
	}
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	if history == nil {
		history = []Recent{}
	}
	body := map[string]any{
		"model": model,
		"state": map[string]any{
			"page":           map[string]any{"url": page.URL, "title": page.Title, "text": page.Text},
			"elements":       sp.Elements,
			"recent_actions": history,
		},
		"questions": questions,
	}
	return body, sp
}

// Choose asks the model for the next operation and target.
func (c *Client) Choose(ctx context.Context, page *snapshot.Page, goal string, history []Recent) (*Decision, error) {
	if c.Key == "" {
		return nil, errors.New("jev: TYPESAFE_API_KEY is not set")
	}
	body, sp := BuildRequest(c.Model, page, goal, history)
	reqJSON, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	raw, err := postJSON(ctx, c.HTTP, c.Endpoint, c.Key, reqJSON)
	if err != nil {
		return nil, err
	}
	latency := time.Since(start)
	var res struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   Usage                      `json:"usage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("jev: bad response: %w", err)
	}
	opIDs := sp.OperationNames()
	opAns, err := validateChoice(res.Answers["operation"], opIDs)
	if err != nil {
		return nil, err
	}
	d := &Decision{Operation: opAns.Choice, Confidence: opAns.Confidence, OperationProbability: opAns.Probabilities,
		Model: res.Model, Usage: res.Usage, Latency: latency, Request: reqJSON, Probabilities: map[string]float64{}}
	d.RawAnswers, _ = json.Marshal(res.Answers)
	if candidates, ok := sp.Targets[d.Operation]; ok {
		// Only the head matching the chosen operation can cause an action.
		ids := make([]string, 0, len(candidates))
		for id := range candidates {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		tAns, err := validateChoice(res.Answers[strings.ToLower(d.Operation)+"_target"], ids)
		if err != nil {
			return nil, err
		}
		d.Target = tAns.Choice
		d.TargetConfidence = tAns.Confidence
		d.TargetProbability = tAns.Probabilities
		d.Choice = candidates[d.Target].ID
		for index, a := range candidates {
			d.Probabilities[a.ID] = tAns.Probabilities[index]
		}
	} else if a, ok := sp.Controls[d.Operation]; ok {
		d.Choice = a.ID
		d.Probabilities[a.ID] = opAns.Probabilities[d.Operation]
	} else {
		d.Choice = d.Operation // DONE / BLOCKED
		d.Probabilities[d.Choice] = opAns.Probabilities[d.Operation]
	}
	return d, nil
}

// ErrInvalidAnswer means the model output failed validation; nothing executes.
var ErrInvalidAnswer = errors.New("jev: invalid TypeSafe response; no action executed")

func validateChoice(raw json.RawMessage, ids []string) (*answer, error) {
	var a answer
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil {
		return nil, ErrInvalidAnswer
	}
	if len(a.Probabilities) != len(ids) {
		return nil, ErrInvalidAnswer
	}
	sum, maxP := 0.0, 0.0
	for _, id := range ids {
		p, ok := a.Probabilities[id]
		if !ok || !finite01(p) {
			return nil, ErrInvalidAnswer
		}
		sum += p
		maxP = math.Max(maxP, p)
	}
	pc, ok := a.Probabilities[a.Choice]
	if !ok || !finite01(a.Confidence) || math.Abs(sum-1) >= 0.02 || pc < maxP-1e-6 {
		return nil, ErrInvalidAnswer
	}
	return &a, nil
}

func finite01(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1 }

// HTTPError is a non-retryable provider status.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("model provider returned HTTP %d; no action executed", e.Status)
}

// postJSON posts with bearer auth, retrying 429/503/529 with backoff.
func postJSON(ctx context.Context, hc *http.Client, url, key string, body []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("model connection failed; no action executed: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case (resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 529) && attempt < 2:
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(500*(1<<attempt)) * time.Millisecond):
			}
			continue
		case resp.StatusCode >= 400:
			return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
		}
		return data, nil
	}
}

// PostJSON is exported for the text helper, which shares the retry policy.
func PostJSON(ctx context.Context, hc *http.Client, url, key string, body []byte) ([]byte, error) {
	return postJSON(ctx, hc, url, key, body)
}
