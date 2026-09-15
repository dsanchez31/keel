package planner

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/dsanchez31/keel/internal/strictjson"
)

// triageSystem is the system prompt of the triage call. It carries no world:
// whether a text asks for a mission does not depend on which areas exist, and
// an unknown area is gate 2's to name.
const triageSystem = `You triage the operator's text for KEEL, which plans reconnaissance missions of areas for a fleet of autonomous aerial and ground vectors.

Decide whether the text asks for such a mission: an area to be reconnoitred, searched, swept, surveyed or patrolled, whether or not it names the area. A question, a greeting, a status request or an order to a single vehicle is not a mission.

Reply with exactly one JSON document: "reason", one sentence saying what the operator asks for, then "mission", true or false.`

// triageSchema constrains the triage reply. It is written out rather than
// encoded from a map, which would sort mission first: Ollama generates the
// properties in the schema's order, and the model states what is asked before
// it decides.
const triageSchema = `{"type":"object","additionalProperties":false,"required":["reason","mission"],"properties":{` +
	`"reason":{"type":"string","description":"One sentence: what the operator asks for."},` +
	`"mission":{"type":"boolean","description":"true when the text asks for an area to be reconnoitred, searched, swept, surveyed or patrolled."}}}`

// Triage is the planner's answer, before the first attempt, to whether the
// intent asks for a mission at all (spec section 5.4).
//
// Every text is otherwise compiled into a plan: a question such as "check the
// fleet" spent three attempts to be refused, and once the model schema
// requires a full tactic it can even yield a plan that passes every gate.
// Declining is fail closed, nothing moves, and the operator reads why.
type Triage struct {
	// Reply is the model's reply, as raw text.
	Reply string `json:"reply"`
	// Mission is false when the intent asks for no mission: the compilation
	// ends there, with no attempt.
	Mission bool `json:"mission"`
	// Reason is the model's one-sentence reading of the intent.
	Reason string `json:"reason,omitempty"`
	// Unreadable says why the reply could not be read. The intent is then
	// compiled anyway: in doubt, the guarded path.
	Unreadable string `json:"unreadable,omitempty"`
}

// TriageConversation is the one-turn conversation the triage call sends.
func TriageConversation(intent string) Conversation {
	return Conversation{
		System:   triageSystem,
		Messages: []Message{{Role: RoleUser, Content: intent}},
		Schema:   json.RawMessage(triageSchema),
	}
}

// IsTriage reports whether a conversation is the triage call rather than a
// plan attempt.
func IsTriage(c Conversation) bool { return string(c.Schema) == triageSchema }

// triage asks the planner whether the intent asks for a mission. Only a
// backend failure is an error.
func triage(ctx context.Context, p Planner, intent string) (Triage, error) {
	reply, err := p.Propose(ctx, TriageConversation(intent))
	if err != nil {
		return Triage{}, err
	}
	return readTriage(reply), nil
}

// readTriage reads a triage reply. A reply that is not the closed document
// the schema describes, or that declines without a reason, is unreadable and
// counts as a mission.
func readTriage(reply string) Triage {
	t := Triage{Reply: reply, Mission: true}
	v, err := strictjson.Decode[struct {
		Reason  string `json:"reason"`
		Mission bool   `json:"mission"`
	}]([]byte(reply), "triage")
	switch {
	case err != nil:
		t.Unreadable = err.Error()
	case !v.Mission && strings.TrimSpace(v.Reason) == "":
		t.Unreadable = "triage.reason: empty on a declined intent"
	default:
		t.Mission, t.Reason = v.Mission, strings.TrimSpace(v.Reason)
	}
	return t
}
