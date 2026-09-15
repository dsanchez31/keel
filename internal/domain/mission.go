package domain

import "slices"

// TickIntervalMs is the fixed tick interval in milliseconds of mission time,
// 100 ms by spec.md section 2. It lives here rather than in engine because
// doctrine validates its duration windows against it and engine imports
// doctrine; engine re-exports it.
const TickIntervalMs int64 = 100

// MissionID identifies one mission run.
type MissionID string

// PlanHash is the lowercase hex SHA-256 of the canonically encoded plan.
// Two plans with the same hash are byte identical.
type PlanHash string

// LaneID identifies a lane within a plan.
type LaneID string

// MissionState is the mission lifecycle.
type MissionState string

const (
	MissionPlanning         MissionState = "planning"
	MissionAwaitingApproval MissionState = "awaiting_approval"
	MissionRunning          MissionState = "running"
	MissionComplete         MissionState = "complete"
	MissionFailed           MissionState = "failed"
)

// Terminal reports whether the mission has stopped for good. I8 requires every
// mission to reach one of these in bounded time.
func (m MissionState) Terminal() bool {
	return m == MissionComplete || m == MissionFailed
}

// DoctrineRef names a doctrine pack by name and semver, rendered as
// "recon-standard@2.1.0".
type DoctrineRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// String renders the canonical "<name>@<semver>" form.
func (d DoctrineRef) String() string { return d.Name + "@" + d.Version }

// AssignPolicy is how lanes are matched to vectors.
type AssignPolicy string

const (
	PolicyNearestCapable AssignPolicy = "nearest_capable"
	PolicyRoundRobin     AssignPolicy = "round_robin"
	PolicyLowestCost     AssignPolicy = "lowest_cost"
)

// Pattern is the coverage tactic a plan declares. Only PatternParallelLanes
// has an expansion (spec section 5.5); the other two are valid Plan IR values
// that expansion refuses.
type Pattern string

const (
	PatternParallelLanes Pattern = "parallel_lanes"
	PatternSpiral        Pattern = "spiral"
	PatternPerimeter     Pattern = "perimeter"
)

// Orientation is the direction lanes run in, for PatternParallelLanes.
type Orientation string

const (
	// OrientationNorthSouth runs lanes north to south, stacked west to east.
	OrientationNorthSouth Orientation = "north_south"
	// OrientationEastWest runs lanes east to west, stacked south to north.
	OrientationEastWest Orientation = "east_west"
	// OrientationLongAxis runs lanes along the longer side of the AO's
	// minimum-area bounding rectangle, which minimises the number of turns.
	OrientationLongAxis Orientation = "long_axis"
)

// Mission is the top-level unit of work.
type Mission struct {
	ID        MissionID    `json:"id"`
	Intent    string       `json:"intent"`
	Area      Area         `json:"area"`
	Doctrine  DoctrineRef  `json:"doctrine"`
	Plan      PlanHash     `json:"plan"`
	State     MissionState `json:"state"`
	StartedMs int64        `json:"started_ms"`
}

// Clone deep-copies the mutable coverage bitmaps carried by the area.
func (m Mission) Clone() Mission {
	out := m
	out.Area = m.Area.Clone()
	return out
}

// Lane is a contiguous strip of the AO assigned to exactly one vector.
//
// Cells is a sorted set. Waypoints are ordered: they are the boustrophedon
// path through the lane and their order is the path.
type Lane struct {
	ID         LaneID     `json:"id"`
	Index      int        `json:"index"`
	Cells      []CellID   `json:"cells"`
	Waypoints  []Position `json:"waypoints"`
	AssignedTo VectorID   `json:"assigned_to,omitempty"`
}

// SortCells puts Cells into ascending order and removes duplicates. Called at
// construction, never at encode time.
func (l *Lane) SortCells() {
	slices.Sort(l.Cells)
	l.Cells = slices.Compact(l.Cells)
}

// Assigned reports whether the lane has an owner.
func (l Lane) Assigned() bool { return l.AssignedTo != "" }

// LengthM is the total path length of the waypoint sequence in metres.
// Segments are summed in path order, which is fixed, so the sum is
// reproducible without a sort.
func (l Lane) LengthM() float64 {
	var total float64
	for i := 1; i < len(l.Waypoints); i++ {
		total += HaversineM(l.Waypoints[i-1], l.Waypoints[i])
	}
	return total
}

// Clone deep-copies the cell set and the waypoint path.
func (l Lane) Clone() Lane {
	out := l
	out.Cells = slices.Clone(l.Cells)
	out.Waypoints = slices.Clone(l.Waypoints)
	return out
}

// CloneLanes deep-copies a lane slice, preserving order.
func CloneLanes(in []Lane) []Lane {
	if in == nil {
		return nil
	}
	out := make([]Lane, len(in))
	for i, l := range in {
		out[i] = l.Clone()
	}
	return out
}

// ApprovedPlan is what crosses the fence. Everything above the fence produced
// it; everything below it treats it as given.
//
// It carries the expanded geometry rather than the Plan IR, because the engine
// never re-expands: expansion happened once, was validated, was rendered for a
// human, and was hashed.
type ApprovedPlan struct {
	Hash     PlanHash    `json:"hash"`
	Mission  MissionID   `json:"mission"`
	Intent   string      `json:"intent"`
	Area     Area        `json:"area"`
	Doctrine DoctrineRef `json:"doctrine"`
	// DoctrineHash is the content address of the pack gate 2 resolved
	// Doctrine to, covered by the hash. The engine refuses the approval when
	// its registry resolves the reference to a pack with another hash, so a
	// plan never runs under rules it was not validated against.
	DoctrineHash string       `json:"doctrine_hash"`
	Lanes        []Lane       `json:"lanes"`
	Requires     []string     `json:"requires"`
	Policy       AssignPolicy `json:"policy"`
	GCS          []Station    `json:"gcs,omitempty"`
	Rationale    string       `json:"rationale"`
	// SwathM and ScanAltM are the geometry expansion used, fixed at gate 3 and
	// covered by the hash. Redecomposition sweeps the pooled cells with them,
	// so the lanes cut mid mission have the geometry the human approved rather
	// than one derived from whatever the fleet has become.
	SwathM   float64 `json:"swath_m"`
	ScanAltM float64 `json:"scan_alt_m"`
}

// Clone deep-copies everything mutable in the plan.
func (p ApprovedPlan) Clone() ApprovedPlan {
	out := p
	out.Area = p.Area.Clone()
	out.Lanes = CloneLanes(p.Lanes)
	out.Requires = slices.Clone(p.Requires)
	out.GCS = slices.Clone(p.GCS)
	return out
}

// Station is a named ground control station, the destination of return_to_base.
type Station struct {
	Name     string   `json:"name"`
	Position Position `json:"position"`
}
