package planner

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/planir"
)

// systemRules is the fixed part of the system prompt. The world and the
// doctrine packs follow it.
const systemRules = `You are the planner of KEEL, an orchestrator for fleets of autonomous aerial and ground vectors performing systematic reconnaissance.

Your job is to compile the operator's intent into one Plan IR document. You choose the tactic. The server computes every lane, waypoint and coordinate from your choices, validates the plan, and a human approves it before anything moves.

Rules:
- Reply with exactly one JSON document that matches the schema. No prose, no Markdown, no code fence.
- Never write a coordinate. Areas and ground stations are referred to by the names listed below, and only those names resolve.
- Copy the operator's intent into "intent" verbatim, character for character.
- Choose a doctrine pack from the list below and write it as <name>@<version>.
- "requires" lists the capability tags every vector flying a lane must declare. With parallel_lanes, use at most as many lanes as there are available vectors declaring every one of those tags, and each lane goes to a different vector.
- Explain your choices in "rationale", in two or three sentences, for the operator who approves the plan.
- If the server refuses a plan, it replies with structured diagnostics. Fix the plan. Every constraint stands: change the plan, never the constraint.`

// Opening starts the conversation for one intent.
func Opening(intent string, w *planir.World, reg *doctrine.Registry) Conversation {
	return Conversation{
		System:   SystemPrompt(w, reg),
		Messages: []Message{{Role: RoleUser, Content: intent}},
		Schema:   planir.ModelSchema(),
	}
}

// SystemPrompt describes the rules, the doctrine packs and the world.
//
// It carries names, capability tags, availability and sizes, and no
// coordinate: the model has no field that could hold one and no decision that
// needs one. Every list is emitted in sorted order, so the same world yields
// the same prompt.
func SystemPrompt(w *planir.World, reg *doctrine.Registry) string {
	var b strings.Builder
	b.WriteString(systemRules)

	b.WriteString("\n\nDoctrine packs:\n")
	for _, ref := range reg.Refs() {
		p, err := reg.Resolve(ref)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "- %s: battery reserve %d%%", ref, p.Params.BatteryReservePct)
		if len(p.Roles) > 0 {
			roles := make([]string, len(p.Roles))
			for i, r := range p.Roles {
				roles[i] = fmt.Sprintf("%s [%s]", r.ID, strings.Join(r.Requires, ", "))
			}
			fmt.Fprintf(&b, "; roles %s", strings.Join(roles, ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("\nAreas of operations:\n")
	for _, a := range w.Areas {
		g := a.Area.Grid
		_, cells := g.Coverage()
		km2 := float64(cells) * g.CellM * g.CellM / 1e6
		fmt.Fprintf(&b, "- %s: %.1f km², about %.1f km east-west by %.1f km north-south\n",
			a.Area.Name, km2, float64(g.Cols)*g.CellM/1000, float64(g.Rows)*g.CellM/1000)
	}

	b.WriteString("\nGround control stations:\n")
	for _, name := range w.StationNames() {
		fmt.Fprintf(&b, "- %s\n", name)
	}

	b.WriteString("\nFleet:\n")
	for _, v := range w.Fleet {
		state := "available"
		if !v.State.Available() {
			state = fmt.Sprintf("unavailable (link %s, mode %s)", v.State.Link, v.State.Mode)
		}
		fmt.Fprintf(&b, "- %s: %s, tags [%s], battery %d%%, %s\n",
			v.Caps.ID, v.Caps.Domain, strings.Join(v.Caps.Tags, ", "), v.State.BatteryPct, state)
	}
	return b.String()
}

// Repair continues a conversation after a refused reply: the reply goes back
// as the model's turn, the diagnostics as the next user turn. The conversation
// given is not modified.
//
// Diagnostics are sent in their canonical encoding, the same bytes the
// operator and the log see, so what the model is told and what is recorded
// cannot differ.
func Repair(c Conversation, reply string, diags []planir.Diagnostic) (Conversation, error) {
	enc, err := eventlog.CanonicalString(diags)
	if err != nil {
		return Conversation{}, fmt.Errorf("planner: encoding diagnostics: %w", err)
	}
	gate := planir.GateSchema
	if len(diags) > 0 {
		gate = diags[0].Gate
	}
	out := c
	out.Messages = slices.Clone(c.Messages)
	out.Messages = append(out.Messages,
		Message{Role: RoleAssistant, Content: reply},
		Message{Role: RoleUser, Content: fmt.Sprintf(
			"The server refused this plan at the %s gate. Diagnostics:\n%s\nEvery constraint stands: change the plan, never the constraint. Reply with the corrected plan as one JSON document.",
			gate, enc)},
	)
	return out, nil
}
