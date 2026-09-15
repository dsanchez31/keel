// Package transport is the operator's surface of the daemon: the REST API and
// the WebSocket stream of spec section 8.
//
// It is an edge, outside the decision path. It holds no mission state of its
// own: the REST handlers ask a Service, which cmd/keeld implements, and the
// stream fans out what the daemon publishes into a Hub once per tick. The
// engine never learns that HTTP exists; a request reaches it as an event
// applied at the next tick, like anything else from outside.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/dsanchez31/keel/internal/domain"
	"github.com/dsanchez31/keel/internal/httpjson"
	"github.com/dsanchez31/keel/internal/pacer"
	"github.com/dsanchez31/keel/internal/planner"
)

// Service is what the REST handlers ask of the daemon.
//
// Every method reports a refusal by wrapping one of ErrNotFound, ErrConflict
// or ErrInvalid, and the failure of a planner backend or of another upstream
// service by wrapping planner.ErrBackend or ErrUpstream; the handlers turn
// those into 404, 409, 422 and 502.
// Anything else is a server error, logged and answered 500 without its
// message.
type Service interface {
	// Compile compiles an intent through the planner and the four gates
	// (planner.Compile). A produced plan is held pending approval under its
	// hash. A compilation every attempt of which was refused is an outcome,
	// not an error.
	Compile(ctx context.Context, req CompileRequest) (planner.Outcome, error)
	// Approve opens the human gate for a pending plan: the plan is handed to
	// the engine, applied at the next tick, and the id of the mission it
	// runs under is returned.
	Approve(ctx context.Context, hash domain.PlanHash) (domain.MissionID, error)
	// Discard drops a pending plan.
	Discard(ctx context.Context, hash domain.PlanHash) error
	// Mission is a mission's current view.
	Mission(ctx context.Context, id domain.MissionID) (MissionView, error)
	// SwapDoctrine requests a hot swap of a running mission's pack, applied
	// at the next tick boundary (spec section 6.6).
	SwapDoctrine(ctx context.Context, id domain.MissionID, ref domain.DoctrineRef) error
	// Doctrines lists the registered packs, sorted by name then version.
	Doctrines(ctx context.Context) ([]DoctrineInfo, error)
	// World is the world file's areas and stations, the names an intent may
	// use.
	World(ctx context.Context) (WorldView, error)
	// Fleet is every bound vector's health now, in id order, those that have
	// not joined the engine included.
	Fleet(ctx context.Context) ([]FleetVectorView, error)
	// InjectFault injects a fault on a vector, applied at the next tick.
	InjectFault(ctx context.Context, f domain.Fault) error
	// Replay opens a mission's recorded log, in the on-disk record format
	// (one canonical JSON record per line). The caller closes it.
	Replay(ctx context.Context, id domain.MissionID) (io.ReadCloser, error)
	// ReplayWindow replays a finished mission's log, verifying it, over a
	// window of mission time whose bounds the handler has checked
	// (BuildReplayWindow).
	ReplayWindow(ctx context.Context, id domain.MissionID, fromMs, toMs int64) (ReplayWindow, error)
	// Clock is the daemon's pace.
	Clock(ctx context.Context) (ClockView, error)
	// SetClock sets the pace of the daemon and of its fleet's simulators
	// from the next tick on (spec section 16.5), a speed the handler has
	// checked against pacer.CheckLiveSpeed.
	SetClock(ctx context.Context, speed int) (ClockView, error)
}

// The refusals a Service reports.
var (
	// ErrNotFound: the plan, mission or log named does not exist.
	ErrNotFound = errors.New("transport: not found")
	// ErrConflict: the request contradicts the current state, such as an
	// approval while a mission is running.
	ErrConflict = errors.New("transport: conflict")
	// ErrInvalid: the request is well formed but its content is refused,
	// such as an unknown backend or a fault on an unknown vector.
	ErrInvalid = errors.New("transport: invalid request")
	// ErrUpstream: a service the daemon relies on for the request failed or
	// could not be reached, such as the simulator a fault is forwarded to.
	ErrUpstream = errors.New("transport: upstream failure")
)

// CompileRequest is the body of POST /api/v1/plans. Backend is optional; the
// daemon's default applies when it is empty.
type CompileRequest struct {
	Intent  string `json:"intent"`
	Backend string `json:"backend,omitempty"`
}

// Approval is the answer to POST /api/v1/plans/{hash}/approve: the mission
// the plan will run under. The engine applies the approval at its next tick,
// and a refusal there (spec section 4.5) is a decision on the stream.
type Approval struct {
	Mission domain.MissionID `json:"mission"`
	Plan    domain.PlanHash  `json:"plan"`
}

// Problem is httpjson.Problem with this API's one extension member: Outcome is
// set on a compilation the backend failed (502), carrying the attempts made
// before it did.
type Problem struct {
	Title   string           `json:"title"`
	Status  int              `json:"status"`
	Detail  string           `json:"detail,omitempty"`
	Outcome *planner.Outcome `json:"outcome,omitempty"`
}

// Options configures the handler.
type Options struct {
	// TrustedOrigins are the browser origins, "scheme://host[:port]", besides
	// the server's own, allowed to call the state-changing endpoints and to
	// open the stream: the frontend's dev server, a reverse proxy's public
	// origin.
	TrustedOrigins []string
	// Logger receives the server errors a 500 answer does not disclose. Nil
	// discards them.
	Logger *slog.Logger
}

// NewHandler routes the REST API (spec section 8.1) to svc and the stream
// (section 8.2) to hub.
//
// The API carries no authentication (spec section 1), so a page on another
// origin must not be able to drive it through the operator's browser: the
// approval endpoint is the human gate, and a cross-site form posting to it
// would open the gate with nobody looking. Every state-changing request
// passes net/http's CrossOriginProtection (Sec-Fetch-Site, then Origin
// against Host), request bodies must be application/json, which a
// cross-origin page cannot send without a CORS preflight this server never
// grants, and the stream's handshake checks the origin the same way.
func NewHandler(svc Service, hub *Hub, opts Options) (http.Handler, error) {
	cop := http.NewCrossOriginProtection()
	patterns := make([]string, 0, len(opts.TrustedOrigins))
	for _, o := range opts.TrustedOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("transport: trusted origin: %w", err)
		}
		u, err := url.Parse(o)
		if err != nil {
			return nil, fmt.Errorf("transport: trusted origin %q: %w", o, err)
		}
		// A pattern holding a scheme is matched against "scheme://host".
		patterns = append(patterns, u.Scheme+"://"+u.Host)
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpjson.WriteProblem(w, http.StatusForbidden, "cross-origin request refused: the origin is not trusted")
	}))

	a := &api{svc: svc, log: opts.Logger}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	accept := &websocket.AcceptOptions{OriginPatterns: patterns}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/plans", a.compile)
	mux.HandleFunc("POST /api/v1/plans/{hash}/approve", a.approve)
	mux.HandleFunc("DELETE /api/v1/plans/{hash}", a.discard)
	mux.HandleFunc("GET /api/v1/missions/{id}", a.mission)
	mux.HandleFunc("POST /api/v1/missions/{id}/doctrine", a.swap)
	mux.HandleFunc("GET /api/v1/doctrine", a.doctrines)
	mux.HandleFunc("GET /api/v1/world", a.world)
	mux.HandleFunc("GET /api/v1/fleet", a.fleet)
	mux.HandleFunc("POST /api/v1/faults", a.fault)
	mux.HandleFunc("GET /api/v1/replays/{id}", a.replay)
	mux.HandleFunc("GET /api/v1/replays/{id}/frames", a.replayWindow)
	mux.HandleFunc("GET /api/v1/clock", a.clock)
	mux.HandleFunc("PUT /api/v1/clock", a.setClock)
	mux.HandleFunc("GET "+StreamPath, func(w http.ResponseWriter, r *http.Request) {
		hub.serveStream(w, r, accept)
	})
	return cop.Handler(mux), nil
}

type api struct {
	svc Service
	log *slog.Logger
}

func (a *api) compile(w http.ResponseWriter, r *http.Request) {
	req, ok := httpjson.ReadBody[CompileRequest](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Intent) == "" {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, "intent is empty")
		return
	}
	out, err := a.svc.Compile(r.Context(), req)
	if err != nil {
		if errors.Is(err, planner.ErrBackend) {
			// The attempts the backend answered before failing are still
			// what the operator needs to see.
			httpjson.WriteJSON(w, http.StatusBadGateway, Problem{
				Title:   http.StatusText(http.StatusBadGateway),
				Status:  http.StatusBadGateway,
				Detail:  err.Error(),
				Outcome: &out,
			})
			return
		}
		a.fail(w, r, err)
		return
	}
	// A plan refused by every attempt is a valid outcome (spec section 8.1):
	// 200 either way, the diagnostics say why.
	httpjson.WriteJSON(w, http.StatusOK, out)
}

func (a *api) approve(w http.ResponseWriter, r *http.Request) {
	hash := domain.PlanHash(r.PathValue("hash"))
	id, err := a.svc.Approve(r.Context(), hash)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/missions/"+url.PathEscape(string(id)))
	httpjson.WriteJSON(w, http.StatusAccepted, Approval{Mission: id, Plan: hash})
}

func (a *api) discard(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.Discard(r.Context(), domain.PlanHash(r.PathValue("hash"))); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) mission(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Mission(r.Context(), domain.MissionID(r.PathValue("id")))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, v)
}

func (a *api) swap(w http.ResponseWriter, r *http.Request) {
	ref, ok := httpjson.ReadBody[domain.DoctrineRef](w, r)
	if !ok {
		return
	}
	if err := a.svc.SwapDoctrine(r.Context(), domain.MissionID(r.PathValue("id")), ref); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *api) doctrines(w http.ResponseWriter, r *http.Request) {
	packs, err := a.svc.Doctrines(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, packs)
}

func (a *api) world(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.World(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, v)
}

func (a *api) fleet(w http.ResponseWriter, r *http.Request) {
	v, err := a.svc.Fleet(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, v)
}

func (a *api) fault(w http.ResponseWriter, r *http.Request) {
	f, ok := httpjson.ReadBody[domain.Fault](w, r)
	if !ok {
		return
	}
	if err := f.Check(); err != nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := a.svc.InjectFault(r.Context(), f); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *api) clock(w http.ResponseWriter, r *http.Request) {
	c, err := a.svc.Clock(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, c)
}

// setClock sets the pace: 200 with the pace now in force, 422 for a speed
// outside [1, pacer.MaxLiveSpeed], 409 for a fleet or a loop that cannot
// change pace, 502 for a simulator that did not take it. PUT, since putting
// the pace in force already changes nothing.
func (a *api) setClock(w http.ResponseWriter, r *http.Request) {
	s, ok := httpjson.ReadBody[pacer.Setting](w, r)
	if !ok {
		return
	}
	if err := pacer.CheckLiveSpeed(s.Speed); err != nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c, err := a.svc.SetClock(r.Context(), s.Speed)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, c)
}

// replay streams a recorded log as newline-delimited JSON, the records as
// they are on disk, so a client can verify the chain from the bytes it
// received.
func (a *api) replay(w http.ResponseWriter, r *http.Request) {
	rc, err := a.svc.Replay(r.Context(), domain.MissionID(r.PathValue("id")))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// The status is sent: all that is left is to cut the response, so
		// the client sees a truncated stream rather than a complete-looking
		// short one.
		a.log.Error("streaming a replay", "mission", r.PathValue("id"), "error", err)
		panic(http.ErrAbortHandler)
	}
}

// replayWindow answers a window of a replayed mission. The bounds are whole
// milliseconds of mission time: absent or unreadable is 400, out of range
// 422.
func (a *api) replayWindow(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, errFrom := strconv.ParseInt(q.Get("from"), 10, 64)
	to, errTo := strconv.ParseInt(q.Get("to"), 10, 64)
	if errFrom != nil || errTo != nil {
		httpjson.WriteProblem(w, http.StatusBadRequest, "from and to are required, whole milliseconds of mission time")
		return
	}
	if err := CheckReplayWindow(from, to); err != nil {
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	win, err := a.svc.ReplayWindow(r.Context(), domain.MissionID(r.PathValue("id")), from, to)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.WriteJSON(w, http.StatusOK, win)
}

// fail answers a Service error: a refusal with its status and message, any
// other error as a 500 whose cause is logged, not disclosed.
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpjson.WriteProblem(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict):
		httpjson.WriteProblem(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalid):
		httpjson.WriteProblem(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, planner.ErrBackend), errors.Is(err, ErrUpstream):
		httpjson.WriteProblem(w, http.StatusBadGateway, err.Error())
	case r.Context().Err() != nil:
		// The client is gone: nobody reads the answer.
	default:
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		httpjson.WriteProblem(w, http.StatusInternalServerError, "internal error")
	}
}
