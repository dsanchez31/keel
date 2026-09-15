package pacer

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The real-time pacer waits for each tick's wall-clock deadline and gives
// up when the context does.
func TestRealTimePaces(t *testing.T) {
	p, err := NewRealTime(10)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for tick := int64(1); tick <= 6; tick++ {
		if err := p.Wait(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
	}
	// Tick 6 is five intervals after tick 1: 500 ms at ten times real time.
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("six ticks at ten times real time took %v, want at least 50 ms", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait on a cancelled context: %v", err)
	}
	if _, err := NewRealTime(0); err == nil {
		t.Fatal("a speed of zero was accepted")
	}
}

// A late tick runs at once rather than being skipped.
func TestRealTimeCatchesUp(t *testing.T) {
	p, err := NewRealTime(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	start := time.Now()
	for tick := int64(2); tick <= 4; tick++ {
		if err := p.Wait(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("three overdue ticks waited %v, want them back to back", elapsed)
	}
}

// A change of speed counts from the position already reached: ticks due at
// the old speed stay due, the next ones come at the new one.
func TestSetSpeedRebases(t *testing.T) {
	p, err := NewRealTime(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSpeed(20); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for tick := int64(2); tick <= 11; tick++ {
		if err := p.Wait(context.Background(), tick); err != nil {
			t.Fatal(err)
		}
	}
	// Ten intervals at twenty times real time: 50 ms, not the second at
	// real time.
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("ten ticks at twenty times real time took %v, want about 50 ms", elapsed)
	}
	if got := p.Speed(); got != 20 {
		t.Fatalf("speed %v, want 20", got)
	}
	if err := p.SetSpeed(-1); err == nil {
		t.Fatal("a negative speed was accepted")
	}
}

// A Wait sleeping at the old speed wakes up to the new deadline.
func TestSetSpeedWakesAWait(t *testing.T) {
	p, err := NewRealTime(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan time.Duration)
	start := time.Now()
	go func() {
		// Tick 21 is 2 s away at real time.
		_ = p.Wait(context.Background(), 21)
		done <- time.Since(start)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := p.SetSpeed(20); err != nil {
		t.Fatal(err)
	}
	select {
	case elapsed := <-done:
		if elapsed > time.Second {
			t.Fatalf("the wait returned after %v, want about 120 ms", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wait kept the old deadline")
	}
}

// The scaled clock runs at the speed in force.
func TestNowScales(t *testing.T) {
	p, err := NewRealTime(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetSpeed(10); err != nil {
		t.Fatal(err)
	}
	before, wall := p.Now(), time.Now()
	time.Sleep(50 * time.Millisecond)
	scaled, real := p.Now().Sub(before), time.Since(wall)
	if scaled < 9*real || scaled > 11*real+10*time.Millisecond {
		t.Fatalf("the scaled clock advanced %v over %v of wall clock, want ten times", scaled, real)
	}
}

func TestCheckLiveSpeed(t *testing.T) {
	for _, s := range []int{1, 2, 5, 10, MaxLiveSpeed} {
		if err := CheckLiveSpeed(s); err != nil {
			t.Errorf("speed %d refused: %v", s, err)
		}
	}
	for _, s := range []int{0, -1, MaxLiveSpeed + 1} {
		if err := CheckLiveSpeed(s); err == nil {
			t.Errorf("speed %d accepted", s)
		}
	}
}

func TestFastNeverWaits(t *testing.T) {
	if err := (Fast{}).Wait(context.Background(), 1<<40); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Fast{}).Wait(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("fast wait on a cancelled context: %v", err)
	}
}
