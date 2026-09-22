package decision

import (
	"fmt"
	"os"
	"strings"
)

// Provider names.
const (
	// ProviderTypesafe selects the typesafe.ai Jev backend.
	ProviderTypesafe = "typesafe"

	// ProviderNone explicitly disables decisions. It exists so an
	// environment variable can DISABLE a provider an instance-admin setting
	// enabled: [Resolve] lets non-empty values win, and an empty string
	// therefore means "this source has no opinion" rather than "off". See
	// [Resolve] for why that distinction is load-bearing.
	ProviderNone = "none"
)

// DefaultModel is the pinned Jev model.
//
// It is a pinned VERSION rather than "jev-latest" deliberately: the provider
// documents that jev-latest moves without notice, and a decision model that
// changes underneath stored answers makes those answers uncomparable across
// time. Callers who want the moving target must set it explicitly.
const DefaultModel = "jev-1.13.0"

// Config is the resolved decision-provider configuration.
//
// It is the SINGLE reader of provider settings. Three sources feed it, each
// a Config passed to [Resolve] / [ResolveNamed]: pad's config file, the
// instance-admin setting (TASK-3121), and the environment — in that order,
// later winning. See TASK-3116.
type Config struct {
	// Provider is the backend name: "typesafe", "none", or "" when this
	// source has no opinion.
	Provider string

	// APIKey authenticates to the backend. Never logged, never printed, and
	// never included in an error message.
	APIKey string

	// Model pins the backend model. Empty falls back to [DefaultModel].
	Model string
}

// Enabled reports whether this config selects a real provider.
func (c Config) Enabled() bool {
	p := strings.TrimSpace(strings.ToLower(c.Provider))
	return p != "" && p != ProviderNone
}

// EnvConfig reads the decision-provider settings from the environment.
//
// The variables follow the PAD_MAILEROO_API_KEY pattern in
// internal/config: PAD_DECISION_PROVIDER, PAD_TYPESAFE_API_KEY,
// PAD_DECISION_MODEL.
func EnvConfig() Config {
	return Config{
		Provider: strings.TrimSpace(os.Getenv("PAD_DECISION_PROVIDER")),
		APIKey:   strings.TrimSpace(os.Getenv("PAD_TYPESAFE_API_KEY")),
		Model:    strings.TrimSpace(os.Getenv("PAD_DECISION_MODEL")),
	}
}

// Resolve merges configuration sources, LATER sources winning per field.
// Only non-empty fields override, so a source that says nothing about a
// field leaves the earlier value standing.
//
// Callers put the environment LAST, because the environment overrides the
// instance-admin setting (ruled day 73 on PLAN-3114). That precedence is why
// [ProviderNone] exists: since an empty string means "no opinion", an
// environment that wants to TURN OFF a provider an admin enabled has to say
// so with a value, and "none" is that value.
func Resolve(sources ...Config) Config {
	named := make([]NamedConfig, len(sources))
	for i, c := range sources {
		named[i] = NamedConfig{Config: c}
	}
	out, _ := ResolveNamed(named...)
	return out
}

// Source names for [NamedConfig], as reported by [Origins].
const (
	SourceFile  = "file"
	SourceAdmin = "admin"
	SourceEnv   = "env"
)

// NamedConfig is a [Config] source labelled for [ResolveNamed].
type NamedConfig struct {
	Name string
	Config
}

// Origins reports, per field, the Name of the source whose value won, or ""
// when no source set that field.
type Origins struct {
	Provider string
	APIKey   string
	Model    string
}

// ResolveNamed is [Resolve] with attribution: it applies the same
// later-wins, non-empty-overrides rule and also reports which source each
// field came from. [Resolve] is implemented on it, so the two cannot drift —
// the admin page's "set by environment" is read from the same pass that
// builds the provider (TASK-3121).
func ResolveNamed(sources ...NamedConfig) (Config, Origins) {
	var out Config
	var o Origins
	for _, s := range sources {
		if v := strings.TrimSpace(s.Provider); v != "" {
			out.Provider, o.Provider = v, s.Name
		}
		if v := strings.TrimSpace(s.APIKey); v != "" {
			out.APIKey, o.APIKey = v, s.Name
		}
		if v := strings.TrimSpace(s.Model); v != "" {
			out.Model, o.Model = v, s.Name
		}
	}
	return out, o
}

// New builds the configured provider.
//
// A config that selects no provider returns (nil, nil) — the unconfigured
// case is not an error, and callers branch on a nil Provider. See the package
// doc.
//
// A config that names a provider but cannot satisfy it — an unknown name, or
// a missing API key — IS an error, because it describes an instance whose
// operator asked for decisions and will not get them. Failing quietly there
// would present a misconfiguration as the supported off state.
func New(cfg Config) (Provider, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	name := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch name {
	case ProviderTypesafe:
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("decision: provider %q is configured but PAD_TYPESAFE_API_KEY is empty", name)
		}
		model := cfg.Model
		if model == "" {
			model = DefaultModel
		}
		return newTypesafe(cfg.APIKey, model), nil
	default:
		return nil, fmt.Errorf("decision: unknown provider %q (known: %s, %s)", cfg.Provider, ProviderTypesafe, ProviderNone)
	}
}

// Describe reports the configured provider for operator-facing output such as
// `pad server info`. It returns ("none", "") when nothing is configured, and
// it NEVER returns the API key.
func Describe(cfg Config) (name, model string) {
	if !cfg.Enabled() {
		return ProviderNone, ""
	}
	name = strings.ToLower(strings.TrimSpace(cfg.Provider))
	model = cfg.Model
	if model == "" && name == ProviderTypesafe {
		model = DefaultModel
	}
	return name, model
}
