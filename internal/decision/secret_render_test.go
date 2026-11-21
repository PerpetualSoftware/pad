package decision

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/secret"
)

// The credential this holder carries never renders: not through fmt (every
// verb, value and pointer) and not through either slog handler.
func TestHolderNeverRendersCredential(t *testing.T) {
	const fixture = "fixture-SECRET-decision"
	holder := newTypesafe(fixture, "jev-1.0.0")
	cfg := Resolve(Config{Provider: ProviderTypesafe, APIKey: secret.New(fixture)}, EnvConfig())
	if s := fmt.Sprintf("%+v %#v", cfg, cfg); strings.Contains(s, fixture) {
		t.Errorf("Config rendered: %s", s)
	}
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
