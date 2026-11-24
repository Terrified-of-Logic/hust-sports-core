package hustrun

import "testing"

// The nonce is a pure function of the epoch-millisecond clock. These vectors are
// native timing samples from the reference analysis; they contain no
// credentials.
func TestNonceNativeVectors(t *testing.T) {
	vectors := []struct {
		ms   int64
		want uint64
	}{
		{1792980000000, 9700844518142346010},
		{1792980234567, 10510588983020170643},
		{1792980358546, 15914751984504218043},
		{1792979000000, 7829393877569493284},
	}
	for _, vector := range vectors {
		if got := Nonce(vector.ms); got != vector.want {
			t.Errorf("Nonce(%d) = %d, want %d", vector.ms, got, vector.want)
		}
	}
}

func TestNonceIsDeterministic(t *testing.T) {
	const ms = 1792980000000
	if Nonce(ms) != Nonce(ms) {
		t.Fatal("nonce is not deterministic for a fixed clock")
	}
	if Nonce(ms) == Nonce(ms+1) {
		t.Fatal("nonce collides for adjacent milliseconds")
	}
}
