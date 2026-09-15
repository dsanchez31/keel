package world

import (
	"math/rand/v2"

	"github.com/dsanchez31/keel/internal/domain"
)

// Sensor model.
//
// What the simulator models is the position a vehicle reports, since that is
// what the engine paints coverage from: the true position plus GPS noise,
// drawn uniformly on each horizontal axis in [-GPSNoiseM, GPSNoiseM], plus the
// offset of a GPS drift fault when one is active. Uniform rather than
// Gaussian noise keeps the draw free of the transcendental functions whose
// last bit is not pinned across architectures (design section 10.2).
//
// The camera itself is the footprint the engine paints with SensorRadiusM:
// the world does not decide what a sensor saw.

type sensors struct {
	gpsNoiseM float64
}

// report returns the position a vehicle at truth reports, with a drift
// offset of driftE, driftN metres. It always draws twice, whatever the noise,
// so the stream advances the same way with noise or without.
func (s sensors) report(truth domain.Position, driftE, driftN float64, rng *rand.Rand) domain.Position {
	ne := float64((rng.Float64()*2 - 1) * s.gpsNoiseM)
	nn := float64((rng.Float64()*2 - 1) * s.gpsNoiseM)
	p := offset(truth, ne+driftE, nn+driftN)
	p.AltM = truth.AltM
	return p
}
