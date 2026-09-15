package domain

// CommandType is what the orchestrator asks a vector to do.
type CommandType string

const (
	CommandGoto  CommandType = "goto"
	CommandHold  CommandType = "hold"
	CommandRTB   CommandType = "rtb"
	CommandAbort CommandType = "abort"
)

// Command is one instruction to one vector.
//
// Seq is monotonic per vector and starts at 1. Commands are idempotent by
// Seq: re-delivering a Seq already applied is a no-op, not an error, and
// adapters tolerate gaps caused by loss (spec section 7.2).
type Command struct {
	Vector   VectorID    `json:"vector"`
	Seq      uint64      `json:"seq"`
	Type     CommandType `json:"type"`
	Waypoint *Position   `json:"waypoint,omitempty"`
	Lane     LaneID      `json:"lane,omitempty"`
}

// RequiresWaypoint reports whether the command type carries a destination.
func (c Command) RequiresWaypoint() bool { return c.Type == CommandGoto }

// SortCommands orders a tick's outgoing commands by vector then sequence, so
// the dispatch order is a function of the decision rather than of the order
// the engine happened to build them in.
func SortCommands(in []Command) []Command {
	out := make([]Command, len(in))
	copy(out, in)
	sortSlice(out, func(a, b Command) int {
		if a.Vector != b.Vector {
			return compareString(string(a.Vector), string(b.Vector))
		}
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		default:
			return 0
		}
	})
	return out
}
