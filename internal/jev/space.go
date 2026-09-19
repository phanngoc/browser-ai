package jev

import (
	"sort"
	"strconv"
	"strings"

	"github.com/phanngoc/browser-ai/internal/snapshot"
)

// Element is one observed node as the model sees it: one index per node even
// when it supports both clicking and typing.
type Element struct {
	Index      string   `json:"index"`
	Label      string   `json:"label"`
	Role       string   `json:"role,omitempty"`
	Value      string   `json:"value,omitempty"`
	Checked    string   `json:"checked,omitempty"`
	Selected   string   `json:"selected,omitempty"`
	Expanded   string   `json:"expanded,omitempty"`
	Operations []string `json:"operations"`
	Options    []Option `json:"options,omitempty"`
	hasOptions bool
}

// Option is a native dropdown choice with a code-owned "index:option" id.
type Option struct {
	Index string `json:"index"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// Space is the indexed action space for one observation.
type Space struct {
	Elements []Element
	// Targets[operation][targetIndex] → executable action.
	Targets map[string]map[string]snapshot.Action
	// Controls[OPERATION] → scroll/wait actions (SCROLL_DOWN, SCROLL_UP, WAIT).
	Controls map[string]snapshot.Action
}

// ActionSpace builds the element table and per-operation target heads.
func ActionSpace(actions []snapshot.Action) Space {
	sp := Space{Targets: map[string]map[string]snapshot.Action{}, Controls: map[string]snapshot.Action{}}
	indexOf := map[int]int{} // node → position in Elements
	for _, a := range actions {
		op, ok := kindToOperation[a.Kind]
		if !ok {
			sp.Controls[strings.ToUpper(a.ID)] = a
			continue
		}
		pos, seen := indexOf[a.Node]
		if !seen {
			pos = len(sp.Elements)
			indexOf[a.Node] = pos
			label, _, _ := strings.Cut(a.Label, " → ")
			el := Element{Index: strconv.Itoa(pos + 1), Label: label, Role: a.Role, Value: a.Value,
				Checked: a.Checked, Selected: a.Selected, Expanded: a.Expanded, Operations: []string{}}
			if a.Kind == "select" {
				el.Value = a.CurrentValue
				el.Options = []Option{}
			}
			sp.Elements = append(sp.Elements, el)
		}
		el := &sp.Elements[pos]
		if !contains(el.Operations, op) {
			el.Operations = append(el.Operations, op)
		}
		group := sp.Targets[op]
		if group == nil {
			group = map[string]snapshot.Action{}
			sp.Targets[op] = group
		}
		target := el.Index
		if a.Kind == "select" {
			target = el.Index + ":" + strconv.Itoa(len(el.Options)+1)
			el.Options = append(el.Options, Option{Index: target, Label: a.Label, Value: a.Value})
		}
		group[target] = a
	}
	return sp
}

// OperationNames lists the operations offered, in a stable order.
func (sp Space) OperationNames() []string {
	names := make([]string, 0, len(sp.Targets)+len(sp.Controls)+2)
	for op := range sp.Targets {
		names = append(names, op)
	}
	for op := range sp.Controls {
		names = append(names, op)
	}
	sort.Strings(names)
	return append(names, "DONE", "BLOCKED")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
