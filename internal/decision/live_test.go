package decision

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveProviderAllThreePrimitives calls the REAL provider.
//
// It is skipped unless DECISION_LIVE=1 and PAD_TYPESAFE_API_KEY are both set,
// so CI and `make test` never reach it — it costs money and needs network.
//
// It exists because the httptest fake cannot answer the question this does.
// The fake replays bodies I wrote and asserts against shapes I chose, so an
// encoder and a fake that share the same wrong field name agree with each
// other and both go green: the instrument's subject controls what the
// instrument reads. This is the second channel. Run it after any change to the
// wire types:
//
//	set -a && . ./.env && set +a && DECISION_LIVE=1 go test ./internal/decision/ -run Live -v
func TestLiveProviderAllThreePrimitives(t *testing.T) {
	if os.Getenv("DECISION_LIVE") != "1" {
		t.Skip("set DECISION_LIVE=1 to call the real provider")
	}
	key := os.Getenv("PAD_TYPESAFE_API_KEY")
	if key == "" {
		// Accept the vendor's own variable name too, since that is what the
		// eval scripts and .env use.
		key = os.Getenv("TYPESAFE_API_KEY")
	}
	if key == "" {
		t.Skip("no API key in the environment")
	}

	p, err := New(Config{Provider: ProviderTypesafe, APIKey: key})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	state := "A bug report: the mermaid diagram renderer uses a hardcoded dark theme " +
		"even when the application is in light mode. Cosmetic, affects every diagram, " +
		"no data is wrong and no flow is blocked. Nobody is waiting on a decision."

	answers, usage, err := p.Ask(ctx, state, map[string]Question{
		"component": Choice("Which part of the codebase does the fix live in?", map[string]string{
			"web":    "the SvelteKit web UI",
			"server": "the Go HTTP API handlers",
			"store":  "the database layer and migrations",
		}),
		"severity": Score("Rate how severe this bug is for users.", []string{
			"cosmetic only", "annoying but harmless", "a real defect in normal use",
			"breaks a core flow", "data loss or security exposure",
		}),
		"needs_human": Noul("The next step requires a judgment only a human can supply.",
			"a human must decide", "an agent can proceed"),
	})
	if err != nil {
		t.Fatalf("Ask against the live provider: %v", err)
	}
	t.Logf("usage: %+v", usage)

	// Choice: an option name we sent, with a confidence and name-keyed
	// probabilities. A wrong field name shows up here as a zero value.
	c := answers["component"]
	if c.Kind != KindChoice {
		t.Errorf("component Kind = %q, want choice", c.Kind)
	}
	if c.Choice != "web" && c.Choice != "server" && c.Choice != "store" {
		t.Errorf("component Choice = %q, want one of the options we sent", c.Choice)
	}
	if c.Confidence == nil {
		t.Error("live choice answer decoded no confidence — the field name is wrong")
	}
	if len(c.Probabilities) != 3 {
		t.Errorf("component Probabilities = %v, want one entry per option sent", c.Probabilities)
	}
	if _, ok := c.Probabilities[c.Choice]; !ok {
		t.Errorf("choice %q is not a key of its own probabilities %v — choice probabilities "+
			"must be keyed by option NAME", c.Choice, c.Probabilities)
	}
	t.Logf("choice: %q conf=%v probs=%v", c.Choice, *c.Confidence, c.Probabilities)

	// Score: index-keyed probabilities, a legend echoing our levels, and the
	// mean-not-index property re-verified against live numbers.
	s := answers["severity"]
	if s.Kind != KindScore {
		t.Errorf("severity Kind = %q, want score", s.Kind)
	}
	if len(s.Legend) != 5 {
		t.Errorf("severity Legend = %v, want 5 entries echoing the levels sent", s.Legend)
	}
	if s.Legend["0"] != "cosmetic only" {
		t.Errorf("Legend[0] = %q, want the first level we sent", s.Legend["0"])
	}
	if s.Confidence == nil {
		t.Error("live score answer decoded no confidence — the field name is wrong")
	}
	var mean float64
	for k, prob := range s.Probabilities {
		mean += float64(levelIndex(k)) * prob
	}
	if diff := mean - s.Score; diff > 0.02 || diff < -0.02 {
		t.Errorf("Score %v is not the probability-weighted mean of the level indices (%v). "+
			"The doc comment on Answer.Score asserts it is; if this fails, that comment is "+
			"now false and Answer.TopLevel's contract needs re-reading.", s.Score, mean)
	}
	idx, name, okTop := s.TopLevel()
	if !okTop {
		t.Error("TopLevel() reported no level for a live score answer")
	}
	t.Logf("score: %v conf=%v top=(%d,%q) probs=%v legend-size=%d",
		s.Score, *s.Confidence, idx, name, s.Probabilities, len(s.Legend))

	// Noul: a probability, and NO confidence. This is the assertion the whole
	// pointer design rests on, verified against the live provider rather than
	// against a recording of it.
	n := answers["needs_human"]
	if n.Kind != KindNoul {
		t.Errorf("needs_human Kind = %q, want noul", n.Kind)
	}
	if n.Noul < 0 || n.Noul > 1 {
		t.Errorf("needs_human Noul = %v, want a probability in [0,1]", n.Noul)
	}
	if n.Confidence != nil {
		t.Errorf("LIVE noul answer carried a confidence (%v). Answer.Confidence is a pointer "+
			"precisely because it does not; if the provider has started sending one, the "+
			"doc comments and the recorded fixtures need updating.", *n.Confidence)
	}
	if n.Probabilities != nil {
		t.Errorf("live noul answer carried probabilities %v, want none", n.Probabilities)
	}
	t.Logf("noul: %v (confidence absent as expected)", n.Noul)

	if usage.InputTokens <= 0 {
		t.Errorf("usage.InputTokens = %d, want a positive count — the field name may be wrong",
			usage.InputTokens)
	}
	if usage.StateTruncated {
		t.Error("a short live state was reported as truncated")
	}
}
