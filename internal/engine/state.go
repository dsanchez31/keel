package engine

import (
	"maps"
	"slices"

	"github.com/dsanchez31/keel/internal/coverage"
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
)

// Config is the tuning the engine carries in state rather than reading from
// the environment.
//
// Every one of these is a number a decision can turn on, so every one of them
// is part of the state a replay has to reproduce. Reading any of them from a
// flag or an environment variable at decision time would make two runs of the
// same log diverge for a reason the log does not record.
type Config struct {
	// ArrivalRadiusM is how close a vector must be to a waypoint before the
	// lane cursor advances to the next one. The vector then turns toward the
	// next waypoint, cutting the corner by up to this distance, so it must
	// stay below the route clearance (a quarter cell, spec section 5.5 step
	// 7) less the error on a reported position, or the geofence trips on the
	// system's own route. A plan whose grid leaves no room for both is
	// refused at approval (coverage.Arrival).
	ArrivalRadiusM float64 `json:"arrival_radius_m"`
	// PositionErrorM is the horizontal error budgeted on a reported position.
	// It is checked against the route clearance with ArrivalRadiusM, and
	// read nowhere else.
	PositionErrorM float64 `json:"position_error_m"`
	// StaleTelemetryTicks is how many ticks without an accepted frame before
	// a link is downgraded, then declared lost.
	StaleTelemetryTicks int64 `json:"stale_telemetry_ticks"`
	// RedecomposeDeadlineTicks is the bound invariant I3 is checked against:
	// an uncovered cell must be assigned within this many ticks of becoming
	// unassigned, while an eligible vector exists.
	RedecomposeDeadlineTicks int64 `json:"redecompose_deadline_ticks"`
	// StallDeadlineTicks is how long the mission may run with no eligible
	// vector working a lane before it is declared failed. Without it, a
	// fleet that all goes dark, or one left with work none of it can reach,
	// keeps the mission running until MaxTicks.
	StallDeadlineTicks int64 `json:"stall_deadline_ticks"`
	// MaxTicks is the hard ceiling on a mission. Reaching it is a failure,
	// not a completion: it means the mission did not converge.
	MaxTicks int64 `json:"max_ticks"`
	// ResendTicks is how long a vector's current command stands before it
	// is sent again with the same Seq. Links lose commands; a vector that
	// missed its goto would otherwise hold at the previous waypoint forever,
	// and re-delivering an applied Seq is a no-op by spec section 7.2. Zero
	// disables re-sending.
	ResendTicks int64 `json:"resend_ticks"`
}

// Arrival is how far the engine lets a vector stray from its route, the
// budget a plan's route clearance must exceed.
func (c Config) Arrival() coverage.Arrival {
	return coverage.Arrival{RadiusM: c.ArrivalRadiusM, ErrorM: c.PositionErrorM}
}

// DefaultConfig is the tuning the reference scenario runs with.
//
// The battery reserve is not here: it is the active pack's
// params.battery_reserve_pct, read from State.Pack, so allocation, gate 3 and
// the doctrine constraint share one value.
func DefaultConfig() Config {
	return Config{
		ArrivalRadiusM: 5, // with the position error, 10 m under the 12.5 m clearance of 50 m cells
		// A 95 percent horizontal bound for consumer GNSS: 2.5 m CEP on
		// u-blox M8-class receivers, R95 about 2.08 times CEP.
		PositionErrorM:           5,
		StaleTelemetryTicks:      50,  // 5 s of mission time
		RedecomposeDeadlineTicks: 30,  // 3 s of mission time
		StallDeadlineTicks:       600, // 60 s of mission time
		MaxTicks:                 36000,
		ResendTicks:              20, // 2 s of mission time
	}
}

// IssuedCommand is the command a vector was last given: its target, so a goto
// is sent when the target changes rather than every tick, and the command
// itself with the tick it was last sent, so it can be sent again unchanged.
type IssuedCommand struct {
	Target   string  `json:"target"`
	Command  Command `json:"command"`
	SentTick int64   `json:"sent_tick"`
}

// Engagement is how far a lane's current assignee has got into it.
//
// A vector is scanning once it has reached a waypoint of its active lane
// from inside the AO, and in transit before that. Deriving the mode here
// rather than trusting telemetry is deliberate: an autopilot knows it is
// flying to a point, never that the point is lane work, and the geofence
// constraint must not trip on a vector crossing open ground from a GCS to
// the first waypoint of its lane.
type Engagement struct {
	// FromCursor is the lane's cursor when the current assignee took it. A
	// lane handed on carries its previous owner's progress, which says
	// nothing about where the new owner is.
	FromCursor int  `json:"from_cursor"`
	Engaged    bool `json:"engaged,omitempty"`
}

// State is everything the engine can see.
//
// It is a value. Step takes one and returns another, and the returned state
// shares nothing mutable with the one it was derived from, so a caller may
// keep a snapshot of any tick and know it will not change underneath them.
// That is what lets the DST harness assert invariants against tick N while
// tick N+1 is being computed.
//
// The maps here are iterated only through the helpers in order.go.
type State struct {
	Clock  Clock  `json:"clock"`
	Rand   Rand   `json:"-"`
	Config Config `json:"config"`

	// Doctrines is the registry plan approvals and hot swaps resolve packs
	// against. It is immutable and shared by every copy of the state. Replay
	// rebuilds it from the same pack files; every approval and swap records
	// the pack hash, so a pack edited on disk surfaces as a divergent head
	// hash (I7) rather than as a silently different replay.
	Doctrines *doctrine.Registry `json:"-"`
	// Pack is the active doctrine pack, nil until a plan is approved.
	// Immutable and shared, like the registry it came from.
	Pack *doctrine.Pack `json:"-"`
	// Windows is the doctrine duration-window state, sorted by rule id then
	// agent.
	Windows []Window `json:"windows,omitempty"`

	Mission Mission      `json:"mission"`
	Plan    ApprovedPlan `json:"plan"`

	// Lanes is ordered by Index and stays that way. Lane order is the sweep
	// order, so it is meaning rather than presentation.
	Lanes []Lane `json:"lanes"`
	// NextLaneIndex is the first lane index never used in this mission. It
	// only grows: a retired lane's id is never handed to another lane, so a
	// lane id in the log names one lane for the whole mission.
	NextLaneIndex int `json:"next_lane_index"`

	// Caps is fixed once a vector joins: capabilities do not change mid
	// mission (spec section 7.1).
	Caps map[VectorID]Capabilities `json:"caps"`
	// Vectors is the last accepted view of each vector.
	Vectors map[VectorID]VectorState `json:"vectors"`
	// CmdSeq is the monotonic per-vector command counter. It starts at 1 on
	// the first command, and it lives in state because a replay has to
	// reissue the same sequence numbers to produce the same log.
	CmdSeq map[VectorID]uint64 `json:"cmd_seq"`
	// Cursor is the index of the next waypoint each lane is working toward.
	Cursor map[LaneID]int `json:"cursor"`
	// Issued records the command each vector was last given.
	Issued map[VectorID]IssuedCommand `json:"issued"`
	// Unassigned records the tick at which each lane became unassigned, so
	// I3's deadline is measurable rather than merely asserted.
	Unassigned map[LaneID]int64 `json:"unassigned"`
	// Engagement tracks each assigned lane's progress under its current
	// assignee, from which the scanning mode is derived.
	Engagement map[LaneID]Engagement `json:"engagement"`
	// Reported records the unassigned lanes whose failed allocation has been
	// recorded, so a lane no vector can take is reported once per stretch
	// rather than on every tick it stays unassigned.
	Reported map[LaneID]bool `json:"reported"`

	// Stations are the ground control stations return_to_base routes to.
	Stations []Station `json:"stations"`

	// StalledTicks counts consecutive ticks in which no eligible vector
	// works a lane (Working).
	StalledTicks int64 `json:"stalled_ticks"`
}

// NewState returns a state at mission time zero with a seeded source.
//
// packs is the registry the approved plan's doctrine and every hot swap
// resolve against. A nil registry is a state that can run no mission: plan
// approval is refused with a decision saying why.
func NewState(seed uint64, cfg Config, packs *doctrine.Registry) State {
	return State{
		Clock:      NewClock(),
		Rand:       NewRand(seed),
		Config:     cfg,
		Doctrines:  packs,
		Mission:    Mission{State: MissionPlanning},
		Caps:       map[VectorID]Capabilities{},
		Vectors:    map[VectorID]VectorState{},
		CmdSeq:     map[VectorID]uint64{},
		Cursor:     map[LaneID]int{},
		Issued:     map[VectorID]IssuedCommand{},
		Unassigned: map[LaneID]int64{},
		Engagement: map[LaneID]Engagement{},
		Reported:   map[LaneID]bool{},
	}
}

// Clone deep-copies every mutable part of the state.
//
// Shallow copying would alias the maps and the coverage bitmap, so mutating
// tick N+1 would retroactively change tick N. That is not a performance
// question: an aliased explored bitmap makes I2 unfalsifiable, because the
// "before" snapshot changes to match the "after" one.
func (s State) Clone() State {
	out := s
	out.Mission = s.Mission.Clone()
	out.Plan = s.Plan.Clone()
	out.Lanes = domain.CloneLanes(s.Lanes)
	out.Caps = maps.Clone(s.Caps)
	out.Vectors = maps.Clone(s.Vectors)
	out.CmdSeq = maps.Clone(s.CmdSeq)
	out.Cursor = maps.Clone(s.Cursor)
	out.Issued = maps.Clone(s.Issued)
	out.Unassigned = maps.Clone(s.Unassigned)
	out.Engagement = maps.Clone(s.Engagement)
	out.Reported = maps.Clone(s.Reported)
	out.Windows = slices.Clone(s.Windows)
	out.Stations = slices.Clone(s.Stations)
	if out.Caps == nil {
		out.Caps = map[VectorID]Capabilities{}
	}
	if out.Vectors == nil {
		out.Vectors = map[VectorID]VectorState{}
	}
	if out.CmdSeq == nil {
		out.CmdSeq = map[VectorID]uint64{}
	}
	if out.Cursor == nil {
		out.Cursor = map[LaneID]int{}
	}
	if out.Issued == nil {
		out.Issued = map[VectorID]IssuedCommand{}
	}
	if out.Unassigned == nil {
		out.Unassigned = map[LaneID]int64{}
	}
	if out.Engagement == nil {
		out.Engagement = map[LaneID]Engagement{}
	}
	if out.Reported == nil {
		out.Reported = map[LaneID]bool{}
	}
	return out
}

// Grid is the AO raster, which lives on the mission's area.
func (s State) Grid() Grid { return s.Mission.Area.Grid }

// VectorIDs lists every known vector in ascending id order.
func (s State) VectorIDs() []VectorID { return SortedKeys(s.Vectors) }

// LaneByID finds a lane by id. Lanes is a small ordered slice, so a linear
// scan is both fast enough and free of the map ordering question.
func (s State) LaneByID(id LaneID) (Lane, bool) {
	for _, l := range s.Lanes {
		if l.ID == id {
			return l, true
		}
	}
	return Lane{}, false
}

// LaneOf returns the lowest-Index lane assigned to a vector, if any.
func (s State) LaneOf(v VectorID) (Lane, bool) {
	for _, l := range s.Lanes {
		if l.AssignedTo == v {
			return l, true
		}
	}
	return Lane{}, false
}

// ActiveLane returns the index in Lanes of the lane a vector is working: the
// lowest-Index lane it holds with a waypoint left. The lanes queued behind it
// wait (spec section 4.4). It returns -1 when every lane it holds is walked.
func (s State) ActiveLane(v VectorID) int {
	for i, l := range s.Lanes {
		if l.AssignedTo == v && s.Cursor[l.ID] < len(l.Waypoints) {
			return i
		}
	}
	return -1
}

// laneIndex returns the position of a lane in Lanes, -1 when absent.
func (s State) laneIndex(id LaneID) int {
	for i, l := range s.Lanes {
		if l.ID == id {
			return i
		}
	}
	return -1
}

// InAO reports whether a position lies in an in-AO raster cell, the
// resolution the geofence is checked at (spec section 6.2, within).
func (s State) InAO(p Position) bool {
	g := s.Grid()
	id, ok := g.CellOf(p)
	return ok && g.IsInAO(id)
}

// Eligible reports whether a vector can be given work: it is answering, it is
// not written off or sent home, and it declares every capability the plan
// requires.
func (s State) Eligible(id VectorID) bool {
	v, ok := s.Vectors[id]
	if !ok || !v.Available() {
		return false
	}
	caps, ok := s.Caps[id]
	if !ok {
		return false
	}
	return caps.Satisfies(s.Plan.Requires)
}

// EligibleVectors lists every eligible vector in ascending id order.
func (s State) EligibleVectors() []VectorID {
	var out []VectorID
	for _, id := range s.VectorIDs() {
		if s.Eligible(id) {
			out = append(out, id)
		}
	}
	return out
}

// PendingLanes lists the lanes an allocation round offers, in lane order:
// unassigned, with a waypoint left and a cell uncovered. A lane whose cells
// are all explored, or whose waypoints are all walked, is not work to hand
// out: the residue sweep deals with the second.
func (s State) PendingLanes() []Lane {
	var out []Lane
	for _, l := range s.Lanes {
		if l.Assigned() || len(l.Waypoints) == 0 || s.Cursor[l.ID] >= len(l.Waypoints) {
			continue
		}
		if explored, total := coverage.LaneCoverage(s.Grid(), l); explored == total {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Working reports whether an eligible vector holds a lane with a waypoint
// left: the mission is being worked. A tick in which it is not is a stall
// tick, whatever the reason: no vector eligible, or work that none of the
// eligible vectors can take.
func (s State) Working() bool {
	for _, l := range s.Lanes {
		if l.Assigned() && s.Cursor[l.ID] < len(l.Waypoints) && s.Eligible(l.AssignedTo) {
			return true
		}
	}
	return false
}

// UncoveredCells lists the AO cells that are not yet explored, ascending.
func (s State) UncoveredCells() []CellID { return coverage.Uncovered(s.Grid()) }

// NearestStation returns the closest ground station to a position.
// Ties break on station name, so two equidistant stations resolve the same
// way on every run.
func (s State) NearestStation(p Position) (Station, bool) {
	if len(s.Stations) == 0 {
		return Station{}, false
	}
	best := MinByCost(s.Stations,
		func(st Station) float64 { return domain.HaversineM(p, st.Position) },
		func(st Station) string { return st.Name },
	)
	return s.Stations[best], true
}
