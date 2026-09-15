package domain

import "slices"

// VectorID is stable and unique within a mission.
type VectorID string

// Domain is the medium a vector operates in.
type Domain string

const (
	DomainAerial Domain = "aerial"
	DomainGround Domain = "ground"
)

// ValidDomain reports whether d is one of the two supported media. Anything
// else is out of scope per spec section 1.
func ValidDomain(d Domain) bool { return d == DomainAerial || d == DomainGround }

// LinkState is the orchestrator's view of the comms link to a vector.
type LinkState string

const (
	LinkOK       LinkState = "ok"
	LinkDegraded LinkState = "degraded"
	LinkLost     LinkState = "lost"
)

// VectorMode is what a vector is currently doing.
type VectorMode string

const (
	ModeIdle     VectorMode = "idle"
	ModeTransit  VectorMode = "transit"
	ModeScanning VectorMode = "scanning"
	ModeRTB      VectorMode = "rtb"
	ModeDown     VectorMode = "down"
)

// Capabilities is what a vector declares about itself. It is stable for the
// lifetime of the connection: capabilities changing mid mission is not
// supported (spec section 7.1).
type Capabilities struct {
	ID            VectorID `json:"id"`
	Domain        Domain   `json:"domain"`
	Tags          []string `json:"tags"`
	CruiseSpeed   float64  `json:"cruise_speed"`
	MaxRangeM     float64  `json:"max_range_m"`
	SensorRadiusM float64  `json:"sensor_radius_m"`
}

// SortTags puts Tags into lexicographic order and removes duplicates.
//
// Tags is a set. Two capability sets with the same members in a different
// order are the same set, and the canonical encoding depends on that being
// true of the representation too, so sorting happens at construction.
func (c *Capabilities) SortTags() {
	slices.Sort(c.Tags)
	c.Tags = slices.Compact(c.Tags)
}

// HasTag reports membership. Tags is sorted, so this is a binary search.
func (c Capabilities) HasTag(tag string) bool {
	_, ok := slices.BinarySearch(c.Tags, tag)
	return ok
}

// Satisfies reports whether every required tag is declared. required must be
// sorted; the Plan IR validator guarantees it.
func (c Capabilities) Satisfies(required []string) bool {
	for _, r := range required {
		if !c.HasTag(r) {
			return false
		}
	}
	return true
}

// Clone deep-copies the tag set.
func (c Capabilities) Clone() Capabilities {
	out := c
	out.Tags = slices.Clone(c.Tags)
	return out
}

// VectorState is the last accepted view of a vector.
type VectorState struct {
	ID         VectorID   `json:"id"`
	Position   Position   `json:"position"`
	Heading    float64    `json:"heading"`
	Speed      float64    `json:"speed"`
	BatteryPct int        `json:"battery_pct"`
	Link       LinkState  `json:"link"`
	Mode       VectorMode `json:"mode"`
	LastSeenMs int64      `json:"last_seen_ms"`
	// AckSeq is the last command Seq the vehicle applied, 0 before any. It is
	// the vehicle's word, not a maximum: a vehicle that restarts reports 0
	// again, and the commands standing for it are re-sent (spec section 7.2).
	AckSeq uint64 `json:"ack_seq"`
}

// Available reports whether the vector can be given work: it is answering, it
// has not been written off, and it has not been sent home. A vector returning
// to base left its lanes for a reason (battery, geofence), and handing it new
// work in the same tick would undo the reaction that sent it.
func (v VectorState) Available() bool {
	return v.Link != LinkLost && v.Mode != ModeDown && v.Mode != ModeRTB
}

// HealthKind is the adapter's cached view of a link, reported by Health()
// without performing I/O (spec section 7.1).
type HealthKind string

const (
	HealthOK       HealthKind = "ok"
	HealthDegraded HealthKind = "degraded"
	HealthLost     HealthKind = "lost"
)

// HealthStatus is what a Vector implementation reports about itself.
type HealthStatus struct {
	Kind        HealthKind `json:"kind"`
	Detail      string     `json:"detail,omitempty"`
	LastFrameMs int64      `json:"last_frame_ms"`
}
