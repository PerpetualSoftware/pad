package server

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TestDeriveTOTPStep verifies the single-use step derivation the verify
// handler relies on (BUG-2054): a code resolves to the exact step it was
// generated for, and — crucially — even when the library would still accept
// it one step later/earlier via skew, deriveTOTPStep pins it to the ORIGINAL
// step so the replay watermark advances correctly.
func TestDeriveTOTPStep(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP" // canonical base32 test secret
	now := time.Unix(1_700_000_000, 0).UTC()
	wantStep := now.Unix() / totpPeriod

	code, err := totp.GenerateCodeCustom(secret, now, totp.ValidateOpts{
		Period:    totpPeriod,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}

	// Derived at the generating instant → exact step.
	step, ok := deriveTOTPStep(code, secret, now)
	if !ok {
		t.Fatal("expected the freshly-generated code to derive a step")
	}
	if step != wantStep {
		t.Fatalf("got step %d, want %d", step, wantStep)
	}

	// The code is still accepted ±1 step (the skew totp.Validate allows), but
	// deriveTOTPStep must report the step it was ISSUED for, not the current
	// one — otherwise a replay a step later would record a too-low watermark.
	for _, delta := range []int64{-1, 1} {
		later := now.Add(time.Duration(delta*totpPeriod) * time.Second)
		got, ok := deriveTOTPStep(code, secret, later)
		if !ok {
			t.Fatalf("delta %d: expected code derivable within skew window", delta)
		}
		if got != wantStep {
			t.Errorf("delta %d: got step %d, want original %d", delta, got, wantStep)
		}
	}

	// Two steps away is outside the ±1 skew window → no match (mirrors
	// totp.Validate refusing it).
	tooFar := now.Add(2 * totpPeriod * time.Second)
	if _, ok := deriveTOTPStep(code, secret, tooFar); ok {
		t.Error("expected no step outside the ±1 skew window")
	}

	// A non-matching passcode derives nothing.
	if _, ok := deriveTOTPStep("not-a-code", secret, now); ok {
		t.Error("expected no step for a non-matching passcode")
	}
}
