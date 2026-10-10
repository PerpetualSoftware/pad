package main

import "testing"

// PLAN-3535: --tracks-work is a string flag so false survives stdio MCP.
func TestPLAN3535_ParseTracksWorkFlag(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "false": false, "work": true, "reference": false, " FALSE ": false} {
		got, err := parseTracksWorkFlag(in)
		if err != nil || got != want {
			t.Errorf("%q: %v, %v", in, got, err)
		}
	}
	if _, err := parseTracksWorkFlag("maybe"); err == nil {
		t.Error("an unknown value was accepted")
	}
}
