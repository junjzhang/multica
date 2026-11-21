// Package issuecontract models delivery agreements independently of run delivery.
package issuecontract

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

type Actor struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	SessionID string `json:"session_id,omitempty"`
}

type Version struct {
	Number            int             `json:"number"`
	Goal              string          `json:"goal"`
	Scope             string          `json:"scope"`
	AuthorizationNote string          `json:"authorization_note"`
	Criteria          []string        `json:"criteria"`
	Executor          Actor           `json:"executor"`
	Acceptor          Actor           `json:"acceptor"`
	Basis             json.RawMessage `json:"basis"`
	Author            Actor           `json:"author"`
	CreatedAt         time.Time       `json:"created_at"`
}

type Delivery struct {
	ID       string   `json:"id"`
	Version  int      `json:"version"`
	Criteria []int    `json:"criteria"`
	Evidence string   `json:"evidence"`
	ChildIDs []string `json:"child_ids"`
	Author   Actor    `json:"author"`
	// TaskID is the agent run that submitted, when the submit came from one.
	TaskID    string    `json:"task_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Decision struct {
	DeliveryID string    `json:"delivery_id"`
	Version    int       `json:"version"`
	Outcome    string    `json:"outcome"`
	Criteria   []int     `json:"criteria"`
	Reason     string    `json:"reason"`
	ChildIDs   []string  `json:"child_ids"`
	Author     Actor     `json:"author"`
	CreatedAt  time.Time `json:"created_at"`
}

type RequestRecord struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

type State struct {
	Sequence   int             `json:"sequence"`
	Versions   []Version       `json:"versions"`
	Deliveries []Delivery      `json:"deliveries"`
	Decisions  []Decision      `json:"decisions"`
	Requests   []RequestRecord `json:"requests"`
}

func Empty() State {
	return State{Versions: []Version{}, Deliveries: []Delivery{}, Decisions: []Decision{}, Requests: []RequestRecord{}}
}

func (s State) Current() *Version {
	if len(s.Versions) == 0 {
		return nil
	}
	return &s.Versions[len(s.Versions)-1]
}

func (s State) Accepted() []int {
	out := []int{}
	v := s.Current()
	if v == nil {
		return out
	}
	for _, d := range s.Decisions {
		if d.Version != v.Number || d.Outcome != "accept" {
			continue
		}
		for _, i := range d.Criteria {
			if !slices.Contains(out, i) {
				out = append(out, i)
			}
		}
	}
	slices.Sort(out)
	return out
}

func (s State) Pending() *Delivery {
	v := s.Current()
	if v == nil {
		return nil
	}
	for i := len(s.Deliveries) - 1; i >= 0; i-- {
		d := &s.Deliveries[i]
		if d.Version != v.Number {
			continue
		}
		if !slices.ContainsFunc(s.Decisions, func(r Decision) bool { return r.DeliveryID == d.ID }) {
			return d
		}
	}
	return nil
}

func ValidateCriteria(indices []int, allowed []int) error {
	if len(indices) == 0 {
		return fmt.Errorf("select at least one criterion")
	}
	seen := map[int]bool{}
	for _, i := range indices {
		if !slices.Contains(allowed, i) || seen[i] {
			return fmt.Errorf("criteria must be distinct outstanding indices")
		}
		seen[i] = true
	}
	return nil
}

func (s State) Remaining() []int {
	out := []int{}
	v := s.Current()
	if v == nil {
		return out
	}
	accepted := s.Accepted()
	for i := range v.Criteria {
		if !slices.Contains(accepted, i) {
			out = append(out, i)
		}
	}
	return out
}
