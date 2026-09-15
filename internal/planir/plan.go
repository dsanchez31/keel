// Package planir holds the Plan IR: the small, closed, coordinate-free
// document a planner emits, the JSON Schema that bounds it, the world it is
// resolved against and the four gates that validate it.
//
// It sits above the fence. Nothing here is in the decision path, but nothing
// here does I/O either: the world file and the doctrine packs are read at the
// edge and handed over as bytes and parsed packs.
package planir

import (
	"slices"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/domain"
)

// APIVersion is the only Plan IR version this package reads.
const APIVersion = "keel.plan/v1"

// DefaultOverlapPct is the overlap a parallel_lanes tactic gets when the plan
// omits overlap_pct (spec section 5.1).
const DefaultOverlapPct = 10

// MissionType is what the mission is for.
type MissionType string

const (
	MissionSystematicReconnaissance MissionType = "systematic_reconnaissance"
	MissionPerimeterPatrol          MissionType = "perimeter_patrol"
)

// Priority is the operator-facing urgency of the mission.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// Plan is one Plan IR document, spec section 5.1.
//
// No field can hold a coordinate. The area and the ground station are names
// the server resolves against its own world; the tactic is four scalars that
// coverage.Decompose turns into geometry. That is structural, not advisory.
type Plan struct {
	APIVersion string         `json:"apiVersion"`
	Intent     string         `json:"intent"`
	Doctrine   string         `json:"doctrine"`
	Mission    MissionSpec    `json:"mission"`
	Tactic     TacticSpec     `json:"tactic"`
	Assignment AssignmentSpec `json:"assignment"`
	Rationale  string         `json:"rationale"`
}

// MissionSpec names the mission and the AO it runs over.
type MissionSpec struct {
	Type     MissionType `json:"type"`
	Priority Priority    `json:"priority"`
	Area     string      `json:"area"`
}

// TacticSpec is the coverage tactic the planner chose.
//
// Orientation and Lanes are required for parallel_lanes and absent otherwise.
// OverlapPct is optional: nil means DefaultOverlapPct.
type TacticSpec struct {
	Pattern     domain.Pattern     `json:"pattern"`
	Orientation domain.Orientation `json:"orientation,omitempty"`
	Lanes       int                `json:"lanes,omitempty"`
	OverlapPct  *int               `json:"overlap_pct,omitempty"`
}

// Coverage is the tactic as coverage.Decompose takes it, with the overlap
// default applied. The default is applied here and nowhere else.
func (t TacticSpec) Coverage() coverage.Tactic {
	overlap := DefaultOverlapPct
	if t.OverlapPct != nil {
		overlap = *t.OverlapPct
	}
	return coverage.Tactic{
		Pattern:     t.Pattern,
		Orientation: t.Orientation,
		Lanes:       t.Lanes,
		OverlapPct:  overlap,
	}
}

// AssignmentSpec is how lanes go to vectors. Requires is a sorted set.
type AssignmentSpec struct {
	Policy   domain.AssignPolicy `json:"policy"`
	Requires []string            `json:"requires"`
	GCS      string              `json:"gcs,omitempty"`
}

// normalise puts every set into its canonical form. JSON Schema can demand
// that requires holds unique tags, not that it is sorted, so the order is
// fixed here, at construction, like every other set in the system.
func (p *Plan) normalise() {
	p.Assignment.Requires = slices.Clone(p.Assignment.Requires)
	slices.Sort(p.Assignment.Requires)
	p.Assignment.Requires = slices.Compact(p.Assignment.Requires)
}

// Gate is one of the four validation stages, in the order they run.
type Gate string

const (
	GateSchema      Gate = "schema"
	GateResolution  Gate = "resolution"
	GateFeasibility Gate = "feasibility"
	GateDoctrine    Gate = "doctrine"
)

// Diagnostic is one reason a Plan IR was refused.
//
// It is part of the Plan IR contract rather than an error string: the repair
// loop feeds it back to the planner verbatim, and the operator sees the same
// value. Every field is deterministic, so two validations of the same input
// produce byte-identical diagnostics.
type Diagnostic struct {
	Gate Gate `json:"gate"`
	// Code is a stable snake_case identifier of the failure.
	Code string `json:"code"`
	// Pointer is the RFC 6901 location in the Plan IR the failure concerns,
	// when one applies.
	Pointer string `json:"pointer,omitempty"`
	Message string `json:"message"`
	// Subject is the lane, vector or constraint the failure is about.
	Subject string `json:"subject,omitempty"`
	// Alternatives lists, sorted, the names that would have resolved.
	Alternatives []string `json:"alternatives,omitempty"`
	// Candidates lists every vector considered for a lane that no vector
	// could take, each with the reason it was rejected.
	Candidates []domain.Candidate `json:"candidates,omitempty"`
}

// compareDiagnostics orders the diagnostics of one gate by pointer, code,
// subject then message, so the list is a function of the input alone.
func compareDiagnostics(a, b Diagnostic) int {
	for _, c := range [][2]string{
		{a.Pointer, b.Pointer},
		{a.Code, b.Code},
		{a.Subject, b.Subject},
		{a.Message, b.Message},
	} {
		switch {
		case c[0] < c[1]:
			return -1
		case c[0] > c[1]:
			return 1
		}
	}
	return 0
}

// sortDiagnostics sorts in place and returns the slice.
func sortDiagnostics(ds []Diagnostic) []Diagnostic {
	slices.SortStableFunc(ds, compareDiagnostics)
	return ds
}
