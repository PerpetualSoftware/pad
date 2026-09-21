package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/decision"
)

// These exercise the BINDING, not the component: internal/decision has its own
// tests for Resolve and Describe, and they have no opinion about whether
// `pad server info` ever calls them or with which precedence. Per CONVE-19,
// each case here asserts something only the wiring can get wrong.

func TestDescribeDecisionProviderReportsNoneWhenUnconfigured(t *testing.T) {
	t.Setenv("PAD_DECISION_PROVIDER", "")
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	t.Setenv("PAD_DECISION_MODEL", "")

	got := describeDecisionProvider(&config.Config{})
	if got.Name != decision.ProviderNone {
		t.Errorf("Name = %q, want %q", got.Name, decision.ProviderNone)
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want empty", got.Model)
	}
}

func TestDescribeDecisionProviderReadsTheConfigFile(t *testing.T) {
	t.Setenv("PAD_DECISION_PROVIDER", "")
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	t.Setenv("PAD_DECISION_MODEL", "")

	got := describeDecisionProvider(&config.Config{
		DecisionProvider: decision.ProviderTypesafe,
		TypesafeAPIKey:   "file-key",
		DecisionModel:    "jev-1.0.0",
	})
	if got.Name != decision.ProviderTypesafe {
		t.Errorf("Name = %q, want %q", got.Name, decision.ProviderTypesafe)
	}
	if got.Model != "jev-1.0.0" {
		t.Errorf("Model = %q, want the config file's jev-1.0.0", got.Model)
	}
}

func TestDescribeDecisionProviderEnvOverridesTheFile(t *testing.T) {
	// The precedence ruled on PLAN-3114 (day 73): the environment overrides
	// the instance-admin setting, and the config file stands in for that
	// source until TASK-3121 adds it. Only the wiring decides the ORDER the
	// sources are passed in, so only a test here can catch it reversed.
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "env-key")
	t.Setenv("PAD_DECISION_MODEL", "jev-9.9.9")

	got := describeDecisionProvider(&config.Config{
		DecisionProvider: decision.ProviderTypesafe,
		TypesafeAPIKey:   "file-key",
		DecisionModel:    "jev-1.0.0",
	})
	if got.Model != "jev-9.9.9" {
		t.Errorf("Model = %q, want the environment's jev-9.9.9 to win over the file's jev-1.0.0", got.Model)
	}
}

func TestDescribeDecisionProviderEnvCanDisableTheFile(t *testing.T) {
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderNone)
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	t.Setenv("PAD_DECISION_MODEL", "")

	got := describeDecisionProvider(&config.Config{
		DecisionProvider: decision.ProviderTypesafe,
		TypesafeAPIKey:   "file-key",
	})
	if got.Name != decision.ProviderNone {
		t.Errorf("Name = %q, want %q: an explicit env 'none' must be able to disable what the file enabled", got.Name, decision.ProviderNone)
	}
}

func TestCollectedReportCarriesDecisionProviderAndNoKey(t *testing.T) {
	// The report is what `--format json` serialises, so this is the shape a
	// consumer sees. It also pins the absence of the API key, which is the
	// one thing that must never reach this output.
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "sk-super-secret-value")
	t.Setenv("PAD_DECISION_MODEL", "")

	report := &serverInfoReport{Config: serverInfoConfig{
		DecisionProvider: describeDecisionProvider(&config.Config{}),
	}}

	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	s := string(blob)

	if !strings.Contains(s, `"decision_provider":{"name":"typesafe","model":"`+decision.DefaultModel+`"}`) {
		t.Errorf("report JSON does not carry decision_provider with the pinned default model: %s", s)
	}
	if strings.Contains(s, "sk-super-secret-value") || strings.Contains(s, "secret") {
		t.Errorf("report JSON leaked key material: %s", s)
	}

	// Unconfigured still emits the object with a "none" name rather than
	// omitting the field or emitting a bare string — a consumer must not
	// have to type-switch.
	t.Setenv("PAD_DECISION_PROVIDER", "")
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	blob, err = json.Marshal(&serverInfoReport{Config: serverInfoConfig{
		DecisionProvider: describeDecisionProvider(&config.Config{}),
	}})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if !strings.Contains(string(blob), `"decision_provider":{"name":"none"}`) {
		t.Errorf("unconfigured report JSON = %s, want decision_provider {\"name\":\"none\"} with model omitted", blob)
	}
}

// Review round 2: the tests above call describeDecisionProvider directly, so
// they have no opinion about whether collectServerInfo calls it, or whether
// the human-readable output prints it. These go through both doors.
func TestServerInfoCarriesTheDecisionProviderThroughBothDoors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "sk-super-secret-value")
	t.Setenv("PAD_DECISION_MODEL", "jev-9.9.9")

	cfg := config.DefaultConfig()
	cfg.Mode = config.ModeRemote
	// Nothing listens here; the connection probe fails fast and is recorded,
	// which this test does not care about.
	cfg.URL = "http://127.0.0.1:1"

	report, err := collectServerInfo(cfg)
	if err != nil {
		t.Fatalf("collectServerInfo: %v", err)
	}
	if got := report.Config.DecisionProvider; got.Name != decision.ProviderTypesafe || got.Model != "jev-9.9.9" {
		t.Errorf("collectServerInfo reported decision provider %+v, want typesafe / jev-9.9.9", got)
	}

	out := captureStdout(t, func() { printServerInfo(report) })
	if !regexp.MustCompile(`Decision provider:\s+typesafe \(jev-9\.9\.9\)`).MatchString(out) {
		t.Errorf("human-readable output has no decision provider line:\n%s", out)
	}
	if strings.Contains(out, "secret") {
		t.Errorf("human-readable output leaked key material:\n%s", out)
	}

	// The printer must print the REPORT, not re-resolve the environment: a
	// report carrying values the environment does not hold tells the two
	// apart (review round 3).
	report.Config.DecisionProvider = serverInfoDecision{Name: "report-only-name", Model: "report-only-model"}
	out = captureStdout(t, func() { printServerInfo(report) })
	if !strings.Contains(out, "report-only-name (report-only-model)") {
		t.Errorf("printServerInfo did not print the report's decision provider:\n%s", out)
	}

	// Unconfigured: the line is still printed, and says none.
	t.Setenv("PAD_DECISION_PROVIDER", "")
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	t.Setenv("PAD_DECISION_MODEL", "")
	report, err = collectServerInfo(cfg)
	if err != nil {
		t.Fatalf("collectServerInfo: %v", err)
	}
	out = captureStdout(t, func() { printServerInfo(report) })
	if !regexp.MustCompile(`Decision provider:\s+none\n`).MatchString(out) {
		t.Errorf("unconfigured output does not say the decision provider is none:\n%s", out)
	}
}
