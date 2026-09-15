// Package engine is the decision path: a pure function over in-memory state.
//
// No I/O, no clock, no concurrency, no map iteration. Everything impure lives
// at the edges, in the daemon that owns the tick loop. Replay reuses Step
// unchanged, substituting a recorded event stream for the live inbox, which is
// the only way a replay guarantee survives maintenance.
//
// The rules in spec.md section 12 are normative here. Any violation is a
// defect, and .golangci.yml enforces the mechanical half of them.
package engine

import "github.com/dsanchez31/keel/internal/domain"

// The domain vocabulary, re-exported.
//
// The definitions live in internal/domain so that coverage, assign and
// doctrine can speak the same types without an import cycle back through this
// package. These are aliases, not conversions: engine.VectorState and
// domain.VectorState are one type, and a value of one is a value of the other.
type (
	VectorID     = domain.VectorID
	Domain       = domain.Domain
	Capabilities = domain.Capabilities
	VectorState  = domain.VectorState
	LinkState    = domain.LinkState
	VectorMode   = domain.VectorMode
	HealthKind   = domain.HealthKind
	HealthStatus = domain.HealthStatus

	Position = domain.Position
	Polygon  = domain.Polygon
	CellID   = domain.CellID
	Grid     = domain.Grid
	Area     = domain.Area

	MissionID    = domain.MissionID
	MissionState = domain.MissionState
	Mission      = domain.Mission
	PlanHash     = domain.PlanHash
	DoctrineRef  = domain.DoctrineRef
	LaneID       = domain.LaneID
	Lane         = domain.Lane
	ApprovedPlan = domain.ApprovedPlan
	Station      = domain.Station
	AssignPolicy = domain.AssignPolicy
	Pattern      = domain.Pattern
	Orientation  = domain.Orientation

	EventKind = domain.EventKind
	Event     = domain.Event
	Fault     = domain.Fault
	FaultKind = domain.FaultKind

	CommandType = domain.CommandType
	Command     = domain.Command

	DecisionKind = domain.DecisionKind
	Decision     = domain.Decision
	Candidate    = domain.Candidate
	ShadowedRule = domain.ShadowedRule

	Window       = domain.Window
	DoctrineSwap = domain.DoctrineSwap
)

// Domain constants.
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

// Vector modes.
const (
	ModeIdle     = domain.ModeIdle
	ModeTransit  = domain.ModeTransit
	ModeScanning = domain.ModeScanning
	ModeRTB      = domain.ModeRTB
	ModeDown     = domain.ModeDown
)

// Mission states.
const (
	MissionPlanning         = domain.MissionPlanning
	MissionAwaitingApproval = domain.MissionAwaitingApproval
	MissionRunning          = domain.MissionRunning
	MissionComplete         = domain.MissionComplete
	MissionFailed           = domain.MissionFailed
)

// Event kinds.
const (
	EventVectorJoined  = domain.EventVectorJoined
	EventVectorLeft    = domain.EventVectorLeft
	EventTelemetry     = domain.EventTelemetry
	EventLinkChanged   = domain.EventLinkChanged
	EventPlanApproved  = domain.EventPlanApproved
	EventDoctrineSwap  = domain.EventDoctrineSwap
	EventFaultInjected = domain.EventFaultInjected
	EventOperatorAbort = domain.EventOperatorAbort
)

// Command types.
const (
	CommandGoto  = domain.CommandGoto
	CommandHold  = domain.CommandHold
	CommandRTB   = domain.CommandRTB
	CommandAbort = domain.CommandAbort
)

// Decision kinds.
const (
	DecisionAssignment     = domain.DecisionAssignment
	DecisionReassignment   = domain.DecisionReassignment
	DecisionRedecompose    = domain.DecisionRedecompose
	DecisionDoctrineRule   = domain.DecisionDoctrineRule
	DecisionDoctrineSwap   = domain.DecisionDoctrineSwap
	DecisionMissionState   = domain.DecisionMissionState
	DecisionOperatorNotice = domain.DecisionOperatorNotice
)

// Assignment policies.
const (
	PolicyNearestCapable = domain.PolicyNearestCapable
	PolicyRoundRobin     = domain.PolicyRoundRobin
	PolicyLowestCost     = domain.PolicyLowestCost
)

// Coverage patterns.
const (
	PatternParallelLanes = domain.PatternParallelLanes
	PatternSpiral        = domain.PatternSpiral
	PatternPerimeter     = domain.PatternPerimeter
)

// Lane orientations.
const (
	OrientationNorthSouth = domain.OrientationNorthSouth
	OrientationEastWest   = domain.OrientationEastWest
	OrientationLongAxis   = domain.OrientationLongAxis
)

// Fault kinds.
const (
	FaultKill           = domain.FaultKill
	FaultLinkLoss       = domain.FaultLinkLoss
	FaultBatteryDrain   = domain.FaultBatteryDrain
	FaultGPSDrift       = domain.FaultGPSDrift
	FaultStaleTelemetry = domain.FaultStaleTelemetry
)

// CostEpsilon is the tolerance below which two costs are the same cost.
const CostEpsilon = domain.CostEpsilon
