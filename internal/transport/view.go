package transport

import (
	"github.com/dsanchez31/keel/internal/doctrine"
	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/engine"
	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/planir"
)

// The wire views: engine state projected for the operator.
//
// A view is built from a state Step returned. Step clones the state it is
// given before changing anything, so a state it returned is never mutated
// again, and a view shares its slices rather than copying them: building one
// every tick costs slice headers, not a deep copy of the coverage bitmaps.

// DoctrineInfo is one doctrine pack as the operator sees it: its reference,
// its content address and its normalised document, the rules the doctrine
// panel shows and a diff compares.
type DoctrineInfo struct {
	Ref      string            `json:"ref"`
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Hash     string            `json:"hash"`
	Document doctrine.Document `json:"document"`
}

// WorldView is the answer to GET /api/v1/world: the names an intent may use,
// with what the operator needs to choose among them. The Plan IR names an
// area and never draws one (spec section 5.1); the operator, who approves
// geometry they have seen, gets the polygon.
type WorldView struct {
	Name     string           `json:"name"`
	Areas    []AreaView       `json:"areas"`
	Stations []domain.Station `json:"stations"`
}

// AreaView is one named area of operations: its outline, the cell size and
// scan altitude expansion takes from the world file, and the surface its
// raster covers. The raster itself is left out: explored cells are a
// mission's, and a mission view carries them.
type AreaView struct {
	Name     string         `json:"name"`
	Polygon  domain.Polygon `json:"polygon"`
	CellM    float64        `json:"cell_m"`
	ScanAltM float64        `json:"scan_alt_m"`
	AreaKm2  float64        `json:"area_km2"`
}

// NewWorldView projects the world file keeld loaded, areas and stations in
// name order as the world holds them.
func NewWorldView(w *planir.World) WorldView {
	v := WorldView{Name: w.Name, Areas: make([]AreaView, len(w.Areas)), Stations: w.Stations}
	for i, a := range w.Areas {
		g := a.Area.Grid
		_, cells := g.Coverage()
		v.Areas[i] = AreaView{
			Name:     a.Area.Name,
			Polygon:  a.Area.Polygon,
			CellM:    g.CellM,
			ScanAltM: a.ScanAltM,
			AreaKm2:  float64(cells) * float64(g.CellM*g.CellM) / 1e6,
		}
	}
	return v
}

// FleetVectorView is one bound vector as GET /api/v1/fleet answers it: its
// adapter's verdict on the link now, and why. It is the adapter's view, not
// the engine's, and is never recorded: a vector bound and not joined yet (no
// frame, or frames withheld) is listed with the reason, where the mission
// view does not know it exists.
type FleetVectorView struct {
	ID     domain.VectorID   `json:"id"`
	Health domain.HealthKind `json:"health"`
	Detail string            `json:"detail,omitempty"`
}

// NewDoctrineInfo describes a pack.
func NewDoctrineInfo(p *doctrine.Pack) DoctrineInfo {
	return DoctrineInfo{
		Ref:      p.Ref.String(),
		Name:     p.Ref.Name,
		Version:  p.Ref.Version,
		Hash:     p.Hash,
		Document: p.Doc,
	}
}

// VectorView is one vector: what it declared when it joined and the engine's
// latest view of it, orchestration modes (scanning, down) included.
type VectorView struct {
	Caps  domain.Capabilities `json:"caps"`
	State domain.VectorState  `json:"state"`
}

// LaneView is a lane with its progress: the index of the next waypoint and
// whether its assignee has engaged it (spec section 4.2).
type LaneView struct {
	domain.Lane
	Cursor  int  `json:"cursor"`
	Engaged bool `json:"engaged,omitempty"`
}

// MissionView is the orchestrator's state as of one tick: the body of
// GET /api/v1/missions/{id} and of the mission frame a stream connection
// starts with.
//
// The mission carries its area, whose grid carries the explored bitmap, so
// the fog of war is drawn from a view alone. Head is the hash chain's head
// after the tick, the value the UI renders live and a replay recomputes.
type MissionView struct {
	Tick         int64            `json:"tick"`
	TickMs       int64            `json:"tick_ms"`
	Head         string           `json:"head"`
	Mission      domain.Mission   `json:"mission"`
	DoctrineHash string           `json:"doctrine_hash,omitempty"`
	Lanes        []LaneView       `json:"lanes"`
	Vectors      []VectorView     `json:"vectors"`
	Stations     []domain.Station `json:"stations"`
	Explored     int              `json:"explored"`
	Total        int              `json:"total"`
	// Speed is keeld's pace when the view went out on the live stream, how
	// many times faster than real time the fleet moves. A replay and the REST
	// view omit it: the log does not record the pace (spec section 8.2).
	Speed int `json:"speed,omitempty"`
}

// NewMissionView projects a state and the log head recorded after it.
func NewMissionView(s engine.State, head eventlog.Digest) MissionView {
	v := MissionView{
		Tick:     s.Clock.Tick,
		TickMs:   s.Clock.TickMs,
		Head:     head.String(),
		Mission:  s.Mission,
		Lanes:    make([]LaneView, len(s.Lanes)),
		Stations: s.Stations,
	}
	if s.Pack != nil {
		v.DoctrineHash = s.Pack.Hash
	}
	for i, l := range s.Lanes {
		v.Lanes[i] = LaneView{Lane: l, Cursor: s.Cursor[l.ID], Engaged: s.Engagement[l.ID].Engaged}
	}
	ids := s.VectorIDs()
	v.Vectors = make([]VectorView, len(ids))
	for i, id := range ids {
		v.Vectors[i] = VectorView{Caps: s.Caps[id], State: s.Vectors[id]}
	}
	v.Explored, v.Total = s.Grid().Coverage()
	return v
}

// CoverageDelta is the data of a coverage frame: the cells a tick explored,
// ascending. Coverage is monotonic (I2), so a client adds them to the bitmap
// of the last mission frame and holds the whole fog of war.
type CoverageDelta struct {
	Cells []domain.CellID `json:"cells"`
}

// LaneCursor is one lane's cursor.
type LaneCursor struct {
	Lane   domain.LaneID `json:"lane"`
	Cursor int           `json:"cursor"`
}

// TickSummary is the data of a tick frame, the last frame of every tick: the
// log head after it, the mission state, coverage, and every lane's cursor in
// lane order. Cursors move on most ticks a vector reaches a waypoint, so they
// travel here rather than in a mission frame.
type TickSummary struct {
	Tick     int64               `json:"tick"`
	Head     string              `json:"head"`
	State    domain.MissionState `json:"state"`
	Explored int                 `json:"explored"`
	Total    int                 `json:"total"`
	Cursors  []LaneCursor        `json:"cursors"`
	// Speed is keeld's pace on the live stream, omitted in a replay, as in
	// MissionView.
	Speed int `json:"speed,omitempty"`
}

// ClockView is keeld's pace: the answer to GET and PUT /api/v1/clock (spec
// section 8.1).
type ClockView struct {
	// Speed is how many times faster than real time the fleet moves.
	Speed int `json:"speed"`
	// MaxSpeed is the fastest pace keeld accepts.
	MaxSpeed int `json:"max_speed"`
	// Fixed, when set, is why the pace stays real time: a vector bound that
	// no simulator runs, or a loop not paced in real time.
	Fixed string `json:"fixed,omitempty"`
}

// Message is one frame's type and data, before the hub numbers it for a
// connection.
type Message struct {
	Type FrameType
	Data any
}

// TickMessages projects one tick onto the stream: the tick's events and
// decisions, then what they changed between prev and next, in the order a
// client applies them.
//
//  1. event, for every event of the batch but telemetry, in the order Step
//     applied them
//  2. decision, for every decision, in the order Step made them
//  3. doctrine, when the active pack changed
//  4. mission, the whole view, when the mission, its plan, its pack or the
//     set and assignment of its lanes changed
//  5. telemetry, every vector's state, in id order
//  6. coverage, the cells the tick explored, unless a mission frame carried
//     a new raster
//  7. tick, which closes the tick
//
// Telemetry events are left out: the telemetry frame carries the engine's
// view of every vector, which is what the frames said once the engine
// accepted, aged and derived them.
func TickMessages(prev, next engine.State, events []domain.Event, decisions []domain.Decision, head eventlog.Digest) []Message {
	var out []Message
	for _, ev := range domain.SortEvents(events) {
		if ev.Kind == domain.EventTelemetry {
			continue
		}
		out = append(out, Message{Type: FrameEvent, Data: ev})
	}
	for _, d := range decisions {
		out = append(out, Message{Type: FrameDecision, Data: d})
	}
	if next.Pack != nil && (prev.Pack == nil || prev.Pack.Ref != next.Pack.Ref || prev.Pack.Hash != next.Pack.Hash) {
		out = append(out, Message{Type: FrameDoctrine, Data: NewDoctrineInfo(next.Pack)})
	}
	reshaped := missionReshaped(prev, next)
	if reshaped {
		out = append(out, Message{Type: FrameMission, Data: NewMissionView(next, head)})
	}
	if ids := next.VectorIDs(); len(ids) > 0 {
		states := make([]domain.VectorState, len(ids))
		for i, id := range ids {
			states[i] = next.Vectors[id]
		}
		out = append(out, Message{Type: FrameTelemetry, Data: states})
	}
	if cells := newlyExplored(prev.Grid(), next.Grid()); !reshaped && len(cells) > 0 {
		out = append(out, Message{Type: FrameCoverage, Data: CoverageDelta{Cells: cells}})
	}
	explored, total := next.Grid().Coverage()
	cursors := make([]LaneCursor, len(next.Lanes))
	for i, l := range next.Lanes {
		cursors[i] = LaneCursor{Lane: l.ID, Cursor: next.Cursor[l.ID]}
	}
	return append(out, Message{Type: FrameTick, Data: TickSummary{
		Tick:     next.Clock.Tick,
		Head:     head.String(),
		State:    next.Mission.State,
		Explored: explored,
		Total:    total,
		Cursors:  cursors,
	}})
}

// StampSpeed writes the live stream's pace into a tick's mission and tick
// frames.
func StampSpeed(msgs []Message, speed int) {
	for i, m := range msgs {
		switch d := m.Data.(type) {
		case MissionView:
			d.Speed = speed
			msgs[i].Data = d
		case TickSummary:
			d.Speed = speed
			msgs[i].Data = d
		}
	}
}

// Sampled reports whether a tick is one whose telemetry and tick frames are
// sent, when one tick in every is.
func Sampled(tick, every int64) bool { return every <= 1 || tick%every == 0 }

// ThinTick splits a tick's frames for a stream thinned in time: sampled, it
// keeps them all; otherwise it keeps every frame but the telemetry and tick
// frames, returned apart. Those two only replace what the next ones replace
// again, while an event, a decision, a doctrine, a mission or a coverage frame
// dropped would be a state that never was. A replay window sends its last
// tick's thinned frames anyway; the live stream drops them.
func ThinTick(msgs []Message, sampled bool) (kept, thinned []Message) {
	if sampled {
		return msgs, nil
	}
	kept = make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Type == FrameTelemetry || m.Type == FrameTick {
			thinned = append(thinned, m)
			continue
		}
		kept = append(kept, m)
	}
	return kept, thinned
}

// missionReshaped reports whether a tick changed what a mission frame carries
// beyond what the other frames of the tick update: the mission itself, its
// active pack, or which lanes exist and who holds them.
func missionReshaped(prev, next engine.State) bool {
	a, b := prev.Mission, next.Mission
	if a.ID != b.ID || a.State != b.State || a.Plan != b.Plan || a.Doctrine != b.Doctrine || a.StartedMs != b.StartedMs {
		return true
	}
	if len(prev.Lanes) != len(next.Lanes) {
		return true
	}
	for i := range next.Lanes {
		if prev.Lanes[i].ID != next.Lanes[i].ID || prev.Lanes[i].AssignedTo != next.Lanes[i].AssignedTo {
			return true
		}
	}
	return false
}

// newlyExplored lists the cells explored in next and not in prev, ascending.
// Two grids of a different size are two rasters, and nothing is a delta
// between them.
func newlyExplored(prev, next domain.Grid) []domain.CellID {
	if len(prev.Explored) != len(next.Explored) {
		return nil
	}
	var out []domain.CellID
	for i, e := range next.Explored {
		if e && !prev.Explored[i] {
			out = append(out, domain.CellID(i))
		}
	}
	return out
}
