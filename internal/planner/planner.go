// Package planner turns operator intent into Plan IR through a language model,
// and repairs what the model gets wrong.
//
// It is the only part of the system that is allowed to be non-deterministic,
// and it is placed above the fence for that reason: whatever a backend
// returns goes through planir.Validate, and nothing reaches the engine
// without a validated, content-addressed plan and a human approval.
package planner

import (
	"context"
	"encoding/json"
	"errors"
)

// Role is who wrote a message of the conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of the conversation with the model.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Conversation is everything a backend sends: the system prompt, the turns so
// far and the schema the reply is constrained by. It is built once, in
// prompt.go, so every backend sees byte-identical prompts.
type Conversation struct {
	System   string          `json:"system"`
	Messages []Message       `json:"messages"`
	Schema   json.RawMessage `json:"schema"`
}

// Planner is one language model backend.
//
// Propose returns the model's reply as raw text, not a decoded plan: gate 1
// validates the bytes the model produced, so an unknown field or a wrong type
// is diagnosed rather than silently dropped by a decoder on the way in.
//
// An error means the backend failed (network, timeout, refusal, truncated
// output), not that the plan is wrong. It wraps ErrBackend.
type Planner interface {
	Name() string
	Propose(ctx context.Context, c Conversation) (string, error)
}

// ErrBackend is what every backend failure wraps. It is not a plan defect, so
// the repair loop stops on it rather than spending an attempt.
var ErrBackend = errors.New("planner: backend failed")
