// Package snapshot embeds the DOM snapshot script and its typed result.
package snapshot

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
)

//go:embed snapshot.js
var script string

// Expr is the IIFE that returns the page state (or null while navigating).
var Expr = script

// MarkerExpr returns only the semantic marker, for cheap freshness checks.
var MarkerExpr = "(() => { const state=" + script + "; return state?.marker ?? null; })()"

// Rect is element geometry at observation time (never used for execution).
type Rect struct {
	X, Y, W, H float64
}

// Action is one executable candidate observed on the page.
type Action struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"` // click | fill | select | scroll | wait
	Node         int    `json:"node,omitempty"`
	Role         string `json:"role,omitempty"`
	Label        string `json:"label"`
	Value        string `json:"value,omitempty"`
	CurrentValue string `json:"current_value,omitempty"`
	Checked      string `json:"checked,omitempty"`
	Selected     string `json:"selected,omitempty"`
	Expanded     string `json:"expanded,omitempty"`
	Rect         *Rect  `json:"rect,omitempty"`
	Delta        int    `json:"delta,omitempty"`
}

// Scroll is the page scroll position.
type Scroll struct {
	Y      float64 `json:"y"`
	Height float64 `json:"height"`
}

// Page is one observation.
type Page struct {
	URL            string                     `json:"url"`
	Title          string                     `json:"title"`
	W              int                        `json:"w"`
	H              int                        `json:"h"`
	Text           string                     `json:"text"`
	Scroll         Scroll                     `json:"scroll"`
	Actions        []Action                   `json:"actions"`
	Marker         json.RawMessage            `json:"marker"`
	PageKey        json.RawMessage            `json:"page_key"`
	Guards         map[string]json.RawMessage `json:"guards"`
	OmittedActions int                        `json:"omitted_actions"`
	Fingerprint    string                     `json:"fingerprint"`
}

// Decode parses the raw evaluate value and computes the fingerprint.
func Decode(raw []byte) (*Page, error) {
	var p Page
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p.Fingerprint = fingerprint(&p)
	return &p, nil
}

// fingerprint hashes url, text, actions and scroll (geometry excluded).
func fingerprint(p *Page) string {
	type semantic struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		Node         int    `json:"node,omitempty"`
		Role         string `json:"role,omitempty"`
		Label        string `json:"label"`
		Value        string `json:"value,omitempty"`
		CurrentValue string `json:"current_value,omitempty"`
		Checked      string `json:"checked,omitempty"`
		Selected     string `json:"selected,omitempty"`
		Expanded     string `json:"expanded,omitempty"`
		Delta        int    `json:"delta,omitempty"`
	}
	acts := make([]semantic, len(p.Actions))
	for i, a := range p.Actions {
		acts[i] = semantic{a.ID, a.Kind, a.Node, a.Role, a.Label, a.Value, a.CurrentValue, a.Checked, a.Selected, a.Expanded, a.Delta}
	}
	b, _ := json.Marshal(struct {
		URL     string     `json:"url"`
		Text    string     `json:"text"`
		Actions []semantic `json:"actions"`
		Scroll  Scroll     `json:"scroll"`
	}{p.URL, p.Text, acts, p.Scroll})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
