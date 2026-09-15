// Package pacer decides when a tick runs, never what it sees.
//
// This is the whole difference between a headless-fast run and a real-time
// one. keelsim's loop, keelsim --serve and keeld go through their loops the
// same way whatever paces them; only the wait between ticks differs. The wall
// clock lives here and nowhere below, so a closed loop run fast and in real
// time produces the same log to the bit, which is the phase 6 definition of
// done and the reason a DST seed found fast can be watched in real time.
package pacer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dsanchez31/keel/internal/domain"
)

// MaxLiveSpeed bounds the pace an operator may set on a live loop (keeld and
// keelsim --serve): the whole closed loop, simulators included, must keep up,
// and the stream thins its frames by the speed (spec section 8.2). A headless
// keelsim run paced in real time has no such bound.
const MaxLiveSpeed = 20

// CheckLiveSpeed reports whether speed is a pace a live loop accepts: a whole
// number in [1, MaxLiveSpeed].
func CheckLiveSpeed(speed int) error {
	if speed < 1 || speed > MaxLiveSpeed {
		return fmt.Errorf("speed %d is outside [1, %d]", speed, MaxLiveSpeed)
	}
	return nil
}

// Setting is the body of a clock endpoint: keelsim's PUT /v1/clock (spec
// section 15.4), keeld's PUT /api/v1/clock (spec section 8.1).
type Setting struct {
	Speed int `json:"speed"`
}

// Scalable is a pacer whose speed may change while it runs.
type Scalable interface {
	Pacer
	Speed() float64
	SetSpeed(speed float64) error
}

var _ Scalable = (*RealTime)(nil)

// Pacer decides when each tick runs.
type Pacer interface {
	// Wait returns when tick may run, or with the context's error.
	Wait(ctx context.Context, tick int64) error
}

// Fast runs every tick as soon as the previous one is done.
type Fast struct{}

// Wait returns at once, unless the context is done.
func (Fast) Wait(ctx context.Context, _ int64) error { return ctx.Err() }

// RealTime runs tick n no earlier than n-1 tick intervals of wall clock,
// divided by the speed, after the first Wait. A slow tick is not made up by
// skipping: later ticks run late, back to back, until the loop catches up.
// A time.Ticker would drop them instead, and mission time would fall behind
// the wall clock for good.
//
// The speed may change while the loop runs (SetSpeed). Its methods are safe
// for concurrent use.
type RealTime struct {
	mu    sync.Mutex
	speed float64
	// The tick anchor: pos tick intervals had elapsed at wall, counted from
	// the first Wait. Zero wall means no Wait yet.
	wall time.Time
	pos  float64
	// The scaled clock's anchor: Now read scaled at wall time scaledWall.
	scaled     time.Time
	scaledWall time.Time
	// changed is closed and replaced on every change of speed, waking a
	// sleeping Wait so it recomputes its deadline.
	changed chan struct{}
}

// NewRealTime returns a pacer running speed times faster than real time.
func NewRealTime(speed float64) (*RealTime, error) {
	if !(speed > 0) {
		return nil, fmt.Errorf("speed %v is not positive", speed)
	}
	now := time.Now()
	return &RealTime{speed: speed, scaled: now, scaledWall: now, changed: make(chan struct{})}, nil
}

// Speed is how many times faster than real time the pacer runs.
func (r *RealTime) Speed() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.speed
}

// SetSpeed changes the pace from now on. The ticks already due stay due and
// no tick is skipped: the position reached at the old speed becomes the
// origin the new speed counts from.
func (r *RealTime) SetSpeed(speed float64) error {
	if !(speed > 0) {
		return fmt.Errorf("speed %v is not positive", speed)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.wall.IsZero() {
		r.pos += r.elapsed(now)
		r.wall = now
	}
	r.scaled, r.scaledWall = r.scaledAt(now), now
	r.speed = speed
	close(r.changed)
	r.changed = make(chan struct{})
	return nil
}

// Now is a clock running at the pacer's speed: wall time since the pacer was
// made, multiplied by the speed in force at each moment. A threshold read
// against it (an adapter's link health) means the same stretch of mission
// time at any speed.
func (r *RealTime) Now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scaledAt(time.Now())
}

func (r *RealTime) scaledAt(now time.Time) time.Time {
	return r.scaled.Add(time.Duration(float64(now.Sub(r.scaledWall)) * r.speed))
}

// elapsed is the tick intervals run at the current speed since the anchor.
func (r *RealTime) elapsed(now time.Time) float64 {
	return float64(now.Sub(r.wall)) * r.speed / float64(domain.TickIntervalMs*int64(time.Millisecond))
}

// Wait sleeps until the tick's wall-clock deadline, recomputed whenever the
// speed changes.
func (r *RealTime) Wait(ctx context.Context, tick int64) error {
	for {
		r.mu.Lock()
		now := time.Now()
		if r.wall.IsZero() {
			r.wall = now
		}
		ahead := float64(tick-1) - r.pos
		d := time.Duration(ahead*float64(domain.TickIntervalMs*int64(time.Millisecond))/r.speed) - now.Sub(r.wall)
		changed := r.changed
		r.mu.Unlock()
		if d <= 0 {
			return ctx.Err()
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			return nil
		case <-changed:
			t.Stop()
		}
	}
}
