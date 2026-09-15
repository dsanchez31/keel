// Package vector is the public integration surface of KEEL: the contract a
// vehicle adapter implements (spec section 7) and, in agent.go, the toolkit
// that implements the mechanics of that contract once for every adapter.
//
// The types are aliases of internal/domain, not copies. vector.VectorState and
// the engine's VectorState are one type, so a frame an adapter publishes is
// the frame the engine consumes, with no translation layer to drift.
package vector

import (
	"errors"
	"io"

	"github.com/dsanchez31/keel/internal/domain"
)

// The vector vocabulary, re-exported from internal/domain.
type (
	VectorID     = domain.VectorID
	Domain       = domain.Domain
	Capabilities = domain.Capabilities
	VectorState  = domain.VectorState
	LinkState    = domain.LinkState
	VectorMode   = domain.VectorMode
	Position     = domain.Position
	LaneID       = domain.LaneID
	CommandType  = domain.CommandType
	Command      = domain.Command
	HealthKind   = domain.HealthKind
	HealthStatus = domain.HealthStatus
)

// Domains.
const (
	DomainAerial = domain.DomainAerial
	DomainGround = domain.DomainGround
)

// Link states.
const (
	LinkOK       = domain.LinkOK
	LinkDegraded = domain.LinkDegraded
	LinkLost     = domain.LinkLost
)

// Vector modes. Telemetry reports the physical ones only: ModeIdle,
// ModeTransit and ModeRTB. The engine derives ModeScanning and sets ModeDown
// (spec section 4.2).
const (
	ModeIdle     = domain.ModeIdle
	ModeTransit  = domain.ModeTransit
	ModeScanning = domain.ModeScanning
	ModeRTB      = domain.ModeRTB
	ModeDown     = domain.ModeDown
)

// Command types.
const (
	CommandGoto  = domain.CommandGoto
	CommandHold  = domain.CommandHold
	CommandRTB   = domain.CommandRTB
	CommandAbort = domain.CommandAbort
)

// Health kinds.
const (
	HealthOK       = domain.HealthOK
	HealthDegraded = domain.HealthDegraded
	HealthLost     = domain.HealthLost
)

// Vector is what the orchestrator drives: one vehicle behind one adapter.
//
// The contract is spec section 7:
//
//   - Describe is pure, cheap and stable for the adapter's lifetime.
//   - Telemetry returns a receive-only channel the adapter owns. It lives from
//     construction to Close, and only Close closes it: a disconnect is
//     reported by Health, and frames resume on the same channel once the
//     adapter has reconnected.
//   - Execute never blocks. It returns once the command is accepted for
//     transmission; completion is observed through telemetry. It is
//     idempotent by Seq and tolerates gaps.
//   - Health performs no I/O and reports the adapter's cached view.
//   - Close closes the telemetry channel, stops every goroutine the adapter
//     started, and is safe to call more than once.
type Vector interface {
	Describe() Capabilities
	Telemetry() <-chan VectorState
	Execute(Command) error
	Health() HealthStatus
	io.Closer
}

// Errors an adapter returns. Callers test them with errors.Is.
var (
	// ErrClosed is returned by Execute, and by an Agent's Publish, after
	// Close.
	ErrClosed = errors.New("vector: closed")
	// ErrUnsupported refuses a command whose type the adapter does not
	// declare, or that does not exist. Refusing is the point of conformance
	// case C8: a command accepted and never performed leaves the orchestrator
	// assigning work that silently never happens.
	ErrUnsupported = errors.New("vector: command type not supported")
	// ErrInvalidCommand refuses a malformed command: addressed to another
	// vector, Seq 0, a goto without a waypoint, a waypoint that is not finite.
	ErrInvalidCommand = errors.New("vector: invalid command")
	// ErrInvalidFrame refuses a telemetry frame that breaks conformance case
	// C2: another vector's id, a value that is not finite, a battery outside
	// [0, 100], a mode that is not physical, mission time going backwards.
	ErrInvalidFrame = errors.New("vector: invalid telemetry frame")
)

// CommandTypes returns every command type the contract defines, sorted. It is
// the default set an Agent supports.
func CommandTypes() []CommandType {
	return []CommandType{CommandAbort, CommandGoto, CommandHold, CommandRTB}
}

// PhysicalMode reports whether telemetry may carry m. Orchestration modes are
// the engine's to derive, never the vehicle's to report.
func PhysicalMode(m VectorMode) bool {
	return m == ModeIdle || m == ModeTransit || m == ModeRTB
}
