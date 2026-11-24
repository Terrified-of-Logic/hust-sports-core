package hustrun

import (
	"math"
	"sync/atomic"
	"time"
)

// deterministicRNG is a seeded splitmix64 generator. The reference tool uses
// CPython's Mersenne Twister, so identical seeds do not reproduce identical
// payloads across the two implementations; what carries over is the statistical
// shape of the data plus deterministic output for a fixed seed within Go.
type deterministicRNG struct {
	state uint64
}

// rngSequence guarantees distinct states even when the host clock is coarse.
var rngSequence atomic.Uint64

func newDeterministicRNG(seed *int64) *deterministicRNG {
	if seed != nil {
		return &deterministicRNG{state: uint64(*seed)}
	}
	// A nil seed means "no fixed seed": derive from the clock plus a monotonic
	// counter so two runs without --seed never produce the same trajectory.
	state := uint64(time.Now().UnixNano()) ^ (rngSequence.Add(1) * 0x9e3779b97f4a7c15)
	return &deterministicRNG{state: state}
}

func (r *deterministicRNG) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Float64 returns a value in [0, 1).
func (r *deterministicRNG) Float64() float64 {
	return float64(r.next()>>11) / (1 << 53)
}

// Uniform returns a value in [low, high).
func (r *deterministicRNG) Uniform(low, high float64) float64 {
	return low + (high-low)*r.Float64()
}

// Gauss returns a normally distributed value (Box-Muller transform).
func (r *deterministicRNG) Gauss(mean, sigma float64) float64 {
	u1 := r.Float64()
	if u1 < 1e-12 {
		u1 = 1e-12
	}
	u2 := r.Float64()
	radius := math.Sqrt(-2 * math.Log(u1))
	return mean + sigma*radius*math.Cos(2*math.Pi*u2)
}

// IntN returns a value in [0, n).
func (r *deterministicRNG) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// Shuffle permutes n elements in place using the supplied swap function.
func (r *deterministicRNG) Shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		swap(i, j)
	}
}
