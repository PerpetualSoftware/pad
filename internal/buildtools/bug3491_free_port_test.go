package buildtools

import "testing"

// BUG-3491: freePort handed out ephemeral ports, the pool every parallel test
// binary binds :0 from, so another package's listener could take one between
// freePort and the script. Every port it returns must sit outside the
// kernel's ephemeral range, where a :0 bind never lands.
func TestFreePortIsOutsideTheEphemeralRange(t *testing.T) {
	lo, hi := ephemeralRange()
	for i := 0; i < 50; i++ {
		if p := freePort(t); p >= lo && p <= hi {
			t.Fatalf("freePort returned %d, inside the ephemeral range %d-%d", p, lo, hi)
		}
	}
}
