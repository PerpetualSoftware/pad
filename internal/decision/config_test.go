package decision

import (
	"strings"
	"testing"
)

func TestNewReturnsNilWhenUnconfigured(t *testing.T) {
	// The unconfigured case is the DEFAULT and is not an error: callers
	// branch on a nil Provider and behave exactly as they did before this
	// package existed.
	for _, cfg := range []Config{
		{},
		{Provider: ""},
		{Provider: "   "},
		{Provider: ProviderNone},
		{Provider: "NONE"},
		// A key with no provider selected is still off — a stray
		// PAD_TYPESAFE_API_KEY in the environment must not switch decisions
		// on behind the operator's back.
		{APIKey: "secret-key"},
		{Model: "jev-1.13.0"},
	} {
		p, err := New(cfg)
		if err != nil {
			t.Errorf("New(%+v) returned an error for an unconfigured provider: %v", cfg, err)
		}
		if p != nil {
			t.Errorf("New(%+v) returned a provider %T, want nil", cfg, p)
		}
	}
}

func TestNewErrorsOnConfiguredButUnsatisfiable(t *testing.T) {
	// The opposite of the above: an operator who ASKED for decisions and
	// cannot get them must not be handed the silent off state.
	t.Run("missing key", func(t *testing.T) {
		p, err := New(Config{Provider: ProviderTypesafe})
		if err == nil {
			t.Fatal("New succeeded with a provider selected and no API key")
		}
		if p != nil {
			t.Error("New returned a provider alongside an error")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Error("error message may not carry key material")
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		_, err := New(Config{Provider: "acme", APIKey: "k"})
		if err == nil {
			t.Fatal("New succeeded with an unknown provider name")
		}
		if !strings.Contains(err.Error(), "acme") {
			t.Errorf("error does not name the unknown provider: %v", err)
		}
	})
}

func TestNewPinsTheModel(t *testing.T) {
	p, err := New(Config{Provider: ProviderTypesafe, APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != ProviderTypesafe {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderTypesafe)
	}
	// An unset model must resolve to the PINNED default, never to the
	// provider's moving "latest" alias — a decision model that changes
	// underneath stored answers makes them uncomparable over time.
	if p.Model() != DefaultModel {
		t.Errorf("Model() = %q, want the pinned %q", p.Model(), DefaultModel)
	}
	if strings.Contains(DefaultModel, "latest") {
		t.Errorf("DefaultModel = %q, which is a moving alias", DefaultModel)
	}

	p2, err := New(Config{Provider: ProviderTypesafe, APIKey: "k", Model: "jev-9.9.9"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p2.Model() != "jev-9.9.9" {
		t.Errorf("Model() = %q, want the explicitly configured jev-9.9.9", p2.Model())
	}
}

func TestResolvePrecedence(t *testing.T) {
	file := Config{Provider: ProviderTypesafe, APIKey: "file-key", Model: "jev-1.0.0"}

	t.Run("later source wins per field", func(t *testing.T) {
		got := Resolve(file, Config{APIKey: "env-key"})
		if got.APIKey != "env-key" {
			t.Errorf("APIKey = %q, want env-key", got.APIKey)
		}
		// A source silent about a field must not blank it.
		if got.Provider != ProviderTypesafe {
			t.Errorf("Provider = %q, want the earlier source's value to survive", got.Provider)
		}
		if got.Model != "jev-1.0.0" {
			t.Errorf("Model = %q, want the earlier source's value to survive", got.Model)
		}
	})

	t.Run("empty does not clobber", func(t *testing.T) {
		got := Resolve(file, Config{}, Config{Provider: "   "})
		if got != file {
			t.Errorf("Resolve = %+v, want the earlier config %+v unchanged", got, file)
		}
	})

	t.Run("none disables what an earlier source enabled", func(t *testing.T) {
		// This is why ProviderNone exists. With empty meaning "no opinion",
		// an environment that wants to turn OFF a provider an admin enabled
		// has to say so with a value.
		got := Resolve(file, Config{Provider: ProviderNone})
		if got.Enabled() {
			t.Error("Resolve kept the provider enabled after a later source said none")
		}
		p, err := New(got)
		if err != nil || p != nil {
			t.Errorf("New after an explicit none = (%v, %v), want (nil, nil)", p, err)
		}
	})

	t.Run("no sources", func(t *testing.T) {
		if got := Resolve(); got.Enabled() {
			t.Error("Resolve() with no sources reported enabled")
		}
	})
}

func TestEnvConfigReadsAndTrims(t *testing.T) {
	t.Setenv("PAD_DECISION_PROVIDER", "  typesafe  ")
	t.Setenv("PAD_TYPESAFE_API_KEY", " env-key ")
	t.Setenv("PAD_DECISION_MODEL", " jev-2.0.0 ")

	got := EnvConfig()
	want := Config{Provider: ProviderTypesafe, APIKey: "env-key", Model: "jev-2.0.0"}
	if got != want {
		t.Errorf("EnvConfig() = %+v, want %+v", got, want)
	}
}

func TestEnvConfigEmptyWhenUnset(t *testing.T) {
	t.Setenv("PAD_DECISION_PROVIDER", "")
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	t.Setenv("PAD_DECISION_MODEL", "")
	if got := EnvConfig(); got != (Config{}) {
		t.Errorf("EnvConfig() = %+v, want the zero Config so it cannot clobber another source", got)
	}
}

func TestDescribeNeverLeaksTheKey(t *testing.T) {
	const key = "sk-super-secret-value"

	name, model := Describe(Config{Provider: ProviderTypesafe, APIKey: key})
	if name != ProviderTypesafe {
		t.Errorf("name = %q, want %q", name, ProviderTypesafe)
	}
	if model != DefaultModel {
		t.Errorf("model = %q, want the pinned default %q", model, DefaultModel)
	}
	if strings.Contains(name+model, key) || strings.Contains(name+model, "secret") {
		t.Errorf("Describe leaked key material: name=%q model=%q", name, model)
	}

	// Unconfigured reports the sentinel and no model, which is what
	// `pad server info` prints.
	name, model = Describe(Config{})
	if name != ProviderNone {
		t.Errorf("name = %q, want %q", name, ProviderNone)
	}
	if model != "" {
		t.Errorf("model = %q, want empty when nothing is configured", model)
	}

	// An unknown provider is still described rather than reported as none:
	// saying "none" for a misconfiguration would hide it from the operator
	// looking at exactly this output.
	name, _ = Describe(Config{Provider: "acme", APIKey: key})
	if name != "acme" {
		t.Errorf("name = %q, want the configured-but-unknown name reported as-is", name)
	}
}

func TestEnabled(t *testing.T) {
	for _, tt := range []struct {
		cfg  Config
		want bool
	}{
		{Config{}, false},
		{Config{Provider: ""}, false},
		{Config{Provider: "  "}, false},
		{Config{Provider: ProviderNone}, false},
		{Config{Provider: "None"}, false},
		{Config{Provider: ProviderTypesafe}, true},
		{Config{Provider: "TypeSafe"}, true},
		{Config{Provider: "acme"}, true}, // configured, even if unsatisfiable
	} {
		if got := tt.cfg.Enabled(); got != tt.want {
			t.Errorf("Config{Provider:%q}.Enabled() = %v, want %v", tt.cfg.Provider, got, tt.want)
		}
	}
}
