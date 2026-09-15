package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// EventKind discriminates the Event union.
type EventKind string

const (
	// EventVectorJoined announces a vector with its capabilities and its
	// initial state, both required. It is the only event that carries
	// Capabilities: they are stable thereafter. The initial state is required
	// because a vector the engine knows nothing about would otherwise sit in
	// state at zero battery on the point (0, 0), a real place, and doctrine
	// would react to that fiction.
	EventVectorJoined EventKind = "vector_joined"
	// EventVectorLeft withdraws a vector from the fleet.
	EventVectorLeft EventKind = "vector_left"
	// EventTelemetry is one accepted telemetry frame.
	EventTelemetry EventKind = "telemetry"
	// EventLinkChanged is the transport's verdict on a link, independent of
	// telemetry content.
	EventLinkChanged EventKind = "link_changed"
	// EventPlanApproved is the human gate opening. It carries the expanded,
	// validated, content-addressed plan.
	EventPlanApproved EventKind = "plan_approved"
	// EventDoctrineSwap requests a hot swap, applied at the tick boundary. It
	// carries the pack by reference and by expected hash, both required.
	EventDoctrineSwap EventKind = "doctrine_swap"
	// EventFaultInjected records an operator-triggered fault so that replay
	// reproduces the fault, not merely its consequences.
	EventFaultInjected EventKind = "fault_injected"
	// EventOperatorAbort ends the mission from the operator's side.
	EventOperatorAbort EventKind = "operator_abort"
)

// FaultKind enumerates the faults the simulator can inject.
type FaultKind string

const (
	FaultKill           FaultKind = "kill"
	FaultLinkLoss       FaultKind = "link_loss"
	FaultBatteryDrain   FaultKind = "battery_drain"
	FaultGPSDrift       FaultKind = "gps_drift"
	FaultStaleTelemetry FaultKind = "stale_telemetry"
)

// Fault is an injected failure. Parameters are explicit fields rather than a
// map, because a map in the decision path is an iteration order waiting to
// leak into a decision.
type Fault struct {
	Kind       FaultKind `json:"kind"`
	Vector     VectorID  `json:"vector,omitempty"`
	Magnitude  float64   `json:"magnitude,omitempty"`
	DurationMs int64     `json:"duration_ms,omitempty"`
}

// FaultKinds lists every fault kind, sorted.
func FaultKinds() []FaultKind {
	return []FaultKind{FaultBatteryDrain, FaultGPSDrift, FaultKill, FaultLinkLoss, FaultStaleTelemetry}
}

// Check reports whether f is a fault the simulator can apply (spec section
// 15.1): a vector, a known kind, a finite magnitude that is not negative and
// is positive for battery_drain and gps_drift, and a duration of zero (the
// rest of the run) or a whole number of ticks.
//
// It is the one rule every source of a fault holds: the scenario loader, the
// world, keeld's and keelsim's endpoints. Two copies of it would drift, and a
// fault one source refuses would reach the world through another.
func (f Fault) Check() error {
	if strings.TrimSpace(string(f.Vector)) == "" {
		return errors.New("vector is required")
	}
	switch f.Kind {
	case FaultKill, FaultLinkLoss, FaultStaleTelemetry:
	case FaultBatteryDrain, FaultGPSDrift:
		if !(f.Magnitude > 0) || math.IsInf(f.Magnitude, 0) {
			return fmt.Errorf("%s needs a finite positive magnitude", f.Kind)
		}
	default:
		names := make([]string, 0, len(FaultKinds()))
		for _, k := range FaultKinds() {
			names = append(names, string(k))
		}
		return fmt.Errorf("unknown kind %q, valid: %s", f.Kind, strings.Join(names, ", "))
	}
	if math.IsNaN(f.Magnitude) || math.IsInf(f.Magnitude, 0) || f.Magnitude < 0 {
		return fmt.Errorf("magnitude %v is negative or not finite", f.Magnitude)
	}
	if f.DurationMs < 0 || f.DurationMs%TickIntervalMs != 0 {
		return fmt.Errorf("duration_ms %d is not a non-negative whole number of ticks", f.DurationMs)
	}
	return nil
}

// Event is everything that can reach the engine from outside.
//
// It is a tagged union expressed as a struct with optional fields rather than
// an interface, so that one canonical encoder handles it without a type
// registry and so that a replayed log decodes without dynamic dispatch.
type Event struct {
	Kind      EventKind     `json:"kind"`
	Vector    VectorID      `json:"vector,omitempty"`
	Caps      *Capabilities `json:"caps,omitempty"`
	Telemetry *VectorState  `json:"telemetry,omitempty"`
	Link      LinkState     `json:"link,omitempty"`
	Plan      *ApprovedPlan `json:"plan,omitempty"`
	Doctrine  *DoctrineRef  `json:"doctrine,omitempty"`
	// DoctrineHash is the content address a doctrine_swap expects Doctrine to
	// resolve to. A registry pack with another hash, or no hash at all, is
	// refused: replayed against a registry whose pack was edited under the
	// same version, the swap names the pack instead of silently diverging.
	DoctrineHash string `json:"doctrine_hash,omitempty"`
	Fault        *Fault `json:"fault,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// eventPrecedence is the order a tick's events are applied in, by kind.
//
// What vehicles report comes first, then the transport's verdict on a link,
// then what the operator injected, then the mission lifecycle. A frame that
// was already in flight when a link was cut therefore cannot overwrite the
// loss, nor a frame sent before a battery collapse the drain, when both land
// in the same tick. A kind not listed sorts after every listed one.
var eventPrecedence = []EventKind{
	EventVectorJoined,
	EventTelemetry,
	EventLinkChanged,
	EventFaultInjected,
	EventVectorLeft,
	EventPlanApproved,
	EventDoctrineSwap,
	EventOperatorAbort,
}

func precedenceOf(k EventKind) int {
	for i, p := range eventPrecedence {
		if p == k {
			return i
		}
	}
	return len(eventPrecedence)
}

// SortEvents orders a tick's events so that the engine sees them in the same
// sequence whatever order the inbox drained them in.
//
// Ordering is by kind precedence (eventPrecedence), then by kind name for
// kinds outside it, then by vector id. The sort is stable, so two events of
// one kind for one vector keep their arrival order. Reordering the wire is
// exactly the fault the DST harness injects, and this is why it is
// survivable.
func SortEvents(in []Event) []Event {
	out := make([]Event, len(in))
	copy(out, in)
	sortSlice(out, func(a, b Event) int {
		if pa, pb := precedenceOf(a.Kind), precedenceOf(b.Kind); pa != pb {
			return pa - pb
		}
		if a.Kind != b.Kind {
			return compareString(string(a.Kind), string(b.Kind))
		}
		return compareString(string(a.Vector), string(b.Vector))
	})
	return out
}
