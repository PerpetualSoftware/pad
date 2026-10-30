package server

import (
	"strings"
	"testing"
)

func TestTASK4137_PKCEHelpers(t *testing.T) {
	if !pkceVerifierMatches(t4137Verifier, t4137Chall) { // RFC 7636 appendix B
		t.Error("RFC 7636 appendix B pair does not match")
	}
	if pkceVerifierMatches(t4137Verifier, strings.Repeat("A", 43)) || pkceVerifierMatches("", t4137Chall) {
		t.Error("a wrong pair matched")
	}
	if !isS256Challenge(t4137Chall) || isS256Challenge(t4137Chall+"A") || isS256Challenge(t4137Chall[:42]+".") {
		t.Error("isS256Challenge")
	}
	if !isPKCEVerifier(strings.Repeat("a", 42)+"~") || isPKCEVerifier(strings.Repeat("a", 129)) || isPKCEVerifier(strings.Repeat("a", 42)+"+") {
		t.Error("isPKCEVerifier")
	}
}
