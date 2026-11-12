package logging

import (
	"strings"
	"testing"
)

func TestICSFeedRedaction(t *testing.T) {
	token := "padics_" + strings.Repeat("x", 43)
	path := "/api/v1/feeds/ics/" + token
	for _, got := range []string{RedactPath(path), RedactText("GET " + path), RedactText("failed " + token)} {
		if strings.Contains(got, token) || !strings.Contains(got, "REDACTED") {
			t.Fatal("ICS credential was not redacted")
		}
	}
}
