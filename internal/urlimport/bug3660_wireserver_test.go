package urlimport

import (
	"context"
	"strings"
	"testing"
)

// BUG-3660: Azure WireServer (168.63.129.16) is a public address, so no
// reserved range covered it and URL import fetched it. The bug's scenario URL
// is refused as a literal and as a DNS answer.
func TestBUG3660_WireServerRefused(t *testing.T) {
	if err := ValidateURL("http://168.63.129.16/?comp=versions"); err == nil {
		t.Fatal("ValidateURL accepted http://168.63.129.16/?comp=versions")
	}
	stubLookup(t, map[string]string{"wire.test": "168.63.129.16"})
	_, err := NewFetcher().Fetch(context.Background(), "http://wire.test/?comp=versions")
	if err == nil || !strings.Contains(err.Error(), "private/reserved IP 168.63.129.16") {
		t.Fatalf("Fetch of a host resolving to 168.63.129.16: got %v, want the dial-time refusal", err)
	}
}
