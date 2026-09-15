package planner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/planir"
)

// testArrival is the engine's default arrival budget.
var testArrival = coverage.Arrival{RadiusM: 5, ErrorM: 5}

// validReply is the Plan IR of spec section 14 for the reference world.
const validReply = `{
  "apiVersion": "keel.plan/v1",
  "intent": "Grid-search the unexplored area to lift the fog of war",
  "doctrine": "recon-standard@2.1.0",
  "mission": {"type": "systematic_reconnaissance", "priority": "normal", "area": "fog_of_war_east"},
  "tactic": {"pattern": "parallel_lanes", "orientation": "long_axis", "lanes": 4},
  "assignment": {"policy": "nearest_capable", "requires": ["aerial", "camera"], "gcs": "gcs-west"},
  "rationale": "Four camera drones, four lanes along the long axis."
}`

// A reply naming coordinates instead of an area: refused at gate 1.
const coordinateReply = `{"apiVersion": "keel.plan/v1", "area": [[45.02, 5.02], [45.04, 5.07]]}`

// triageMission is the triage reply finding a mission in the intent.
const triageMission = `{"reason":"A grid search of the unexplored area.","mission":true}`

// scripted replays fixed replies in order and records every conversation it
// was sent. The triage call is answered apart, with triage, or triageMission
// when empty, and is not recorded in seen.
type scripted struct {
	triage    string
	triageErr error
	replies   []string
	err       error // returned once the replies run out
	seen      []Conversation
	triaged   int
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Propose(_ context.Context, c Conversation) (string, error) {
	if IsTriage(c) {
		s.triaged++
		if s.triageErr != nil {
			return "", s.triageErr
		}
		if s.triage == "" {
			return triageMission, nil
		}
		return s.triage, nil
	}
	s.seen = append(s.seen, c)
	if len(s.seen) > len(s.replies) {
		if s.err != nil {
			return "", s.err
		}
		return s.replies[len(s.replies)-1], nil
	}
	return s.replies[len(s.seen)-1], nil
}

func compileInput(t *testing.T) Input {
	t.Helper()
	return Input{Intent: referenceIntent, World: loadWorld(t), Doctrines: loadPacks(t), Arrival: testArrival}
}

func TestCompileRepairsARefusedPlan(t *testing.T) {
	p := &scripted{replies: []string{coordinateReply, validReply}}
	out, err := Compile(context.Background(), p, compileInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK() || len(out.Attempts) != 2 {
		t.Fatalf("OK %v after %d attempts", out.OK(), len(out.Attempts))
	}
	if p.triaged != 1 || out.Triage == nil || !out.Triage.Mission || out.Declined() {
		t.Fatalf("triaged %d times, triage %+v", p.triaged, out.Triage)
	}
	if len(out.Attempts[0].Diagnostics) == 0 || out.Attempts[0].Diagnostics[0].Gate != planir.GateSchema {
		t.Fatalf("first attempt diagnostics: %+v", out.Attempts[0].Diagnostics)
	}
	if len(out.Attempts[1].Diagnostics) != 0 || len(out.Plan.Lanes) != 4 || out.Plan.Hash == "" {
		t.Fatalf("second attempt: %+v, plan %+v", out.Attempts[1].Diagnostics, out.Plan)
	}

	// The repair turn carries the refused reply and its diagnostics.
	second := p.seen[1].Messages
	if len(second) != 3 || second[1].Content != coordinateReply || !strings.Contains(second[2].Content, `"code":"additional_property"`) {
		t.Fatalf("repair conversation: %+v", second)
	}
}

// The phase 4 definition of done: a planner that never gets it right is
// stopped after exactly three attempts, with every diagnostic kept and no
// plan produced.
func TestCompileStopsAfterThreeAttempts(t *testing.T) {
	p := &scripted{replies: []string{coordinateReply}}
	out, err := Compile(context.Background(), p, compileInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if out.OK() || out.Plan != nil || out.IR != nil {
		t.Fatal("a plan was produced from refused replies")
	}
	if len(out.Attempts) != MaxAttempts || len(p.seen) != MaxAttempts {
		t.Fatalf("%d attempts recorded, %d proposals made, want %d", len(out.Attempts), len(p.seen), MaxAttempts)
	}
	for i, a := range out.Attempts {
		if a.N != i+1 || len(a.Diagnostics) == 0 {
			t.Fatalf("attempt %d: %+v", i, a)
		}
	}
	// Nothing was relaxed: the same reply was refused the same way each time.
	if !reflect.DeepEqual(out.Attempts[0].Diagnostics, out.Attempts[2].Diagnostics) {
		t.Fatal("the same reply was judged differently on a later attempt")
	}
	if n := len(p.seen[2].Messages); n != 5 {
		t.Fatalf("third conversation has %d messages, want intent plus two repair rounds", n)
	}
}

// An intent that asks for no mission is declined by the triage, with its
// reason, and no plan is ever asked for.
func TestCompileDeclinesAnIntentWithNoMission(t *testing.T) {
	p := &scripted{triage: `{"reason":"A request for the fleet's status.","mission":false}`, replies: []string{validReply}}
	in := compileInput(t)
	in.Intent = "check the fleet"
	out, err := Compile(context.Background(), p, in)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Declined() || out.OK() || out.Triage.Reason != "A request for the fleet's status." {
		t.Fatalf("outcome %+v, triage %+v", out, out.Triage)
	}
	if len(out.Attempts) != 0 || len(p.seen) != 0 {
		t.Fatalf("%d attempts, %d proposals after a declined intent", len(out.Attempts), len(p.seen))
	}
}

// A triage reply that cannot be read is no reason to decline: the intent
// goes the guarded way, through the gates and the human gate.
func TestCompileCompilesThroughAnUnreadableTriage(t *testing.T) {
	for name, reply := range map[string]string{
		"not json":           `mission: no`,
		"unknown field":      `{"reason":"r","mission":false,"area":"x"}`,
		"missing mission":    `{"reason":"r"}`,
		"decline, no reason": `{"reason":" ","mission":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := &scripted{triage: reply, replies: []string{validReply}}
			out, err := Compile(context.Background(), p, compileInput(t))
			if err != nil {
				t.Fatal(err)
			}
			if !out.OK() || out.Declined() || out.Triage.Unreadable == "" || out.Triage.Reply != reply {
				t.Fatalf("OK %v, triage %+v", out.OK(), out.Triage)
			}
		})
	}
}

func TestCompileAbortsOnTriageBackendError(t *testing.T) {
	p := &scripted{triageErr: errors.Join(ErrBackend, errors.New("connection refused")), replies: []string{validReply}}
	out, err := Compile(context.Background(), p, compileInput(t))
	if !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "triage") {
		t.Fatalf("got %v, want the backend error, named as the triage's", err)
	}
	if out.OK() || out.Triage != nil || len(p.seen) != 0 {
		t.Fatalf("OK %v, triage %+v, %d proposals", out.OK(), out.Triage, len(p.seen))
	}
}

func TestCompileAbortsOnBackendError(t *testing.T) {
	boom := errors.New("connection refused")
	p := &scripted{replies: []string{coordinateReply}, err: errors.Join(ErrBackend, boom)}
	// One refused reply, then the backend fails on the repair.
	out, err := Compile(context.Background(), p, compileInput(t))
	if !errors.Is(err, ErrBackend) || !errors.Is(err, boom) {
		t.Fatalf("got %v, want the backend error", err)
	}
	if out.OK() || len(out.Attempts) != 1 || len(p.seen) != 2 {
		t.Fatalf("OK %v, %d attempts, %d proposals", out.OK(), len(out.Attempts), len(p.seen))
	}
}
