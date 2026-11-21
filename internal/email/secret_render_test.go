package email

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The credential this holder carries never renders: not through fmt (every
// verb, through the pointer, since the holder holds a lock) and not through
// either slog handler.
func TestHolderNeverRendersCredential(t *testing.T) {
	const fixture = "fixture-SECRET-email"
	holder := NewSender(fixture, "from@example.test", "Pad", "https://pad.example.test")
	holder.Configure(fixture, "from@example.test", "Pad", "https://pad.example.test")
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		outs = append(outs, fmt.Sprintf(verb, holder))
	}
	var jb, tb bytes.Buffer
	slog.New(slog.NewJSONHandler(&jb, nil)).Info("holder", "h", holder)
	slog.New(slog.NewTextHandler(&tb, nil)).Info("holder", "h", holder)
	outs = append(outs, jb.String(), tb.String())
	for _, out := range outs {
		if strings.Contains(out, fixture) {
			t.Errorf("credential rendered: %s", out)
		}
	}
}
