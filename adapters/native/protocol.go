// Package native is KEEL's own vehicle protocol, JSON over WebSocket, and the
// two ends of it: the adapter the orchestrator drives (adapter.go) and the
// vehicle side a simulator or a third-party vehicle serves (vehicle.go).
//
// It is the simple case of the integration layer and the reference for third
// parties (design section 7). The protocol is spec section 7.3:
//
//   - The adapter dials, one connection per vector, negotiating the
//     subprotocol keel.native.v1.
//   - Every message is one JSON text message, an envelope {"type", "data"}
//     whose data is the domain type under its own JSON tags.
//   - The vehicle's first message is hello: its capabilities and the command
//     types it executes. Then telemetry flows vehicle to adapter, commands
//     adapter to vehicle.
//   - Decoding is strict. An unknown message type, an unknown field or a
//     missing required one is a protocol violation, and the receiver closes
//     the connection naming it.
package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"unicode/utf8"

	"github.com/dsanchez31/keel/internal/strictjson"
	"github.com/dsanchez31/keel/vector"
)

// Subprotocol names the protocol and its version in the WebSocket handshake.
// A peer that does not negotiate it is refused: an incompatible revision
// fails at the handshake rather than at the first message it misreads.
const Subprotocol = "keel.native.v1"

// MaxMessageBytes bounds one message on either side. A hello or a frame is a
// few hundred bytes; the bound is what a peer gone wrong can make the other
// buffer.
const MaxMessageBytes = 32 << 10

// VectorPath is the path pattern of a vector's endpoint on the vehicle side,
// in net/http.ServeMux syntax.
const VectorPath = "/v1/vectors/{id}"

// MessageType is the envelope's type.
type MessageType string

const (
	// TypeHello is the vehicle's first message, and only its first.
	TypeHello MessageType = "hello"
	// TypeTelemetry carries one frame, vehicle to adapter.
	TypeTelemetry MessageType = "telemetry"
	// TypeCommand carries one command, adapter to vehicle.
	TypeCommand MessageType = "command"
)

// Hello is what a vehicle declares about itself when a connection opens.
//
// Supports is the set of command types the vehicle executes, the declaration
// conformance case C8 holds it to: a type outside it is refused by the
// adapter with vector.ErrUnsupported rather than sent to be ignored. It is
// required and never empty; there is no implicit "everything".
//
// Tags and Supports are sets: order and duplicates carry no meaning. A
// reconnect must declare the same hello as the first connection, since
// Describe is stable for the adapter's lifetime.
type Hello struct {
	Capabilities vector.Capabilities  `json:"capabilities"`
	Supports     []vector.CommandType `json:"supports"`
}

// normalised sorts and deduplicates both sets, and turns an absent tag set
// into an empty one so it encodes as [] rather than null.
func (h Hello) normalised() Hello {
	h.Capabilities = h.Capabilities.Clone()
	h.Capabilities.SortTags()
	if h.Capabilities.Tags == nil {
		h.Capabilities.Tags = []string{}
	}
	h.Supports = slices.Clone(h.Supports)
	slices.Sort(h.Supports)
	h.Supports = slices.Compact(h.Supports)
	return h
}

// equal compares two normalised hellos.
func (h Hello) equal(o Hello) bool {
	a, b := h.Capabilities, o.Capabilities
	return a.ID == b.ID && a.Domain == b.Domain &&
		a.CruiseSpeed == b.CruiseSpeed && a.MaxRangeM == b.MaxRangeM && a.SensorRadiusM == b.SensorRadiusM &&
		slices.Equal(a.Tags, b.Tags) && slices.Equal(h.Supports, o.Supports)
}

// ErrProtocol is wrapped by every protocol violation: a message that is not
// JSON text, an unknown type or field, a missing required field, a message
// out of place.
var ErrProtocol = errors.New("native: protocol violation")

// envelope is one message on the wire.
type envelope struct {
	Type MessageType     `json:"type"`
	Data json.RawMessage `json:"data"`
}

// encode renders one message.
func encode(t MessageType, v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("native: encoding %s: %w", t, err)
	}
	return json.Marshal(envelope{Type: t, Data: data})
}

// decode parses an envelope strictly. The type is returned as found; the
// caller refuses the ones out of place.
func decode(b []byte) (envelope, error) {
	var e envelope
	if err := checkFields(b, reflect.TypeFor[envelope](), "message"); err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("%w: message: %v", ErrProtocol, err)
	}
	return e, nil
}

// decodeData parses an envelope's data strictly as T.
func decodeData[T any](e envelope) (T, error) {
	var v T
	if err := checkFields(e.Data, reflect.TypeFor[T](), string(e.Type)); err != nil {
		return v, err
	}
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return v, fmt.Errorf("%w: %s: %v", ErrProtocol, e.Type, err)
	}
	return v, nil
}

// checkFields holds raw to the closed schema of the Go type t (package
// strictjson), a violation being a protocol violation.
func checkFields(raw json.RawMessage, t reflect.Type, path string) error {
	if err := strictjson.Check(raw, t, path); err != nil {
		return fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	return nil
}

// decodeHello parses a hello, refusing an empty command set.
func decodeHello(e envelope) (Hello, error) {
	h, err := decodeData[Hello](e)
	if err != nil {
		return h, err
	}
	h = h.normalised()
	if len(h.Supports) == 0 {
		return h, fmt.Errorf("%w: hello.supports: empty, a vehicle executes at least one command type", ErrProtocol)
	}
	return h, nil
}

// unexpected refuses a well-formed message out of place.
func unexpected(e envelope, want MessageType) error {
	switch e.Type {
	case TypeHello, TypeTelemetry, TypeCommand:
		return fmt.Errorf("%w: %s message, want %s", ErrProtocol, e.Type, want)
	default:
		return fmt.Errorf("%w: unknown message type %q", ErrProtocol, e.Type)
	}
}

// maxCloseReason is the longest reason a close frame carries (RFC 6455
// section 5.5: a control frame payload is at most 125 bytes, 2 of them the
// status code).
const maxCloseReason = 123

// closeReason cuts an error to fit a close frame, on a rune boundary.
func closeReason(err error) string {
	s := err.Error()
	if len(s) <= maxCloseReason {
		return s
	}
	s = s[:maxCloseReason]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
