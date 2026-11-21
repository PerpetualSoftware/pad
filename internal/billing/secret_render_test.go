package billing

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The credential this holder carries never renders: not through fmt (every
// verb, value and pointer) and not through either slog handler.
func TestHolderNeverRendersCredential(t *testing.T) {
	const fixture = "fixture-SECRET-billing"
	holder := NewCloudClient("http://sidecar.example.test", fixture)
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		outs = append(outs, fmt.Sprintf(verb, holder), fmt.Sprintf(verb, *holder))
	}
	var jb, tb bytes.Buffer
	slog.New(slog.NewJSONHandler(&jb, nil)).Info("holder", "h", holder, "v", *holder)
	slog.New(slog.NewTextHandler(&tb, nil)).Info("holder", "h", holder, "v", *holder)
	outs = append(outs, jb.String(), tb.String())
	for _, out := range outs {
		if strings.Contains(out, fixture) {
			t.Errorf("credential rendered: %s", out)
		}
	}
}
