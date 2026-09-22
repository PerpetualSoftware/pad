package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Instance-admin settings for the decision provider (TASK-3121, unit 6 of
// PLAN-3114).
//
// The stored setting is ONE of three sources handed to decision.ResolveNamed:
// config file, then this, then the environment — later winning per field,
// because the environment overrides the instance-admin setting (ruled day 73
// on PLAN-3114). On Pad Cloud the environment is the ONLY source and the
// setting is read-only.
//
// A write takes effect without a restart by rebuilding the provider and
// swapping the runner (reconfigureDecisions). The tick reads the runner once
// per pass and SetDecisionRunner re-installs the write doors' enqueue
// resolver, so both halves follow the swap.

// platform_settings keys. None is in adminManagedSettings, so the generic
// /admin/settings endpoint can neither read nor write them — in particular it
// can never return the API key.
const (
	settingDecisionProvider = "decision_provider" // "typesafe" / "none" / unset
	settingDecisionEnabled  = "decision_enabled"  // "true" / "false" / unset
	settingDecisionModel    = "decision_model"
	settingDecisionAPIKey   = "decision_api_key" // encrypted; see store.SetSecretPlatformSetting
)

// maxDecisionModelLen bounds the model pin. Real pins are ~20 bytes
// ("jev-1.13.0"); the bound only stops a pasted blob being stored.
const maxDecisionModelLen = 128

type decisionSettingsState struct {
	// mu serialises reconfiguration, so two concurrent writes cannot
	// interleave "resolve" and "swap" and leave the older config running.
	mu   sync.Mutex
	file decision.Config
	sets *decision.Registry
}

// SetDecisionBase records the config-file source and the question-set
// registry, both fixed for the process lifetime. Call reconfigureDecisions
// (via ConfigureDecisions) afterwards to build the provider.
func (s *Server) SetDecisionBase(file decision.Config, sets *decision.Registry) {
	s.decisionSettings.mu.Lock()
	defer s.decisionSettings.mu.Unlock()
	s.decisionSettings.file = file
	s.decisionSettings.sets = sets
}

// ConfigureDecisions resolves the provider from every source, swaps the
// runner, and starts the tick if a provider is now configured. The returned
// error describes a provider that is NAMED but cannot be built; decisions are
// then off, which the caller reports.
func (s *Server) ConfigureDecisions() error {
	s.decisionSettings.mu.Lock()
	defer s.decisionSettings.mu.Unlock()
	return s.reconfigureDecisionsLocked()
}

func (s *Server) reconfigureDecisionsLocked() error {
	cfg, _, _ := s.resolveDecisionConfigLocked()
	provider, err := decision.New(cfg)
	if err != nil {
		provider = nil
	}
	sets := s.decisionSettings.sets
	if sets == nil {
		provider = nil
	}
	runner := decision.NewRunner(s.store, provider, sets)
	s.SetDecisionRunner(runner)
	if runner != nil {
		s.StartDecisionTick()
	}
	return err
}

// resolveDecisionConfigLocked returns the resolved config, the origin of each
// field, and any error reading the stored secret. A stored key that cannot be
// read — it does not decrypt, or the row is not ciphertext at all — is treated
// as absent rather than failing the resolution: the
// other sources still decide, and the error is reported to the admin.
func (s *Server) resolveDecisionConfigLocked() (decision.Config, decision.Origins, error) {
	env := decision.NamedConfig{Name: decision.SourceEnv, Config: decision.EnvConfig()}
	if s.cloudMode {
		cfg, o := decision.ResolveNamed(env)
		return cfg, o, nil
	}
	stored, serr := s.storedDecisionConfig()
	cfg, o := decision.ResolveNamed(
		decision.NamedConfig{Name: decision.SourceFile, Config: s.decisionSettings.file},
		decision.NamedConfig{Name: decision.SourceAdmin, Config: stored},
		env,
	)
	return cfg, o, serr
}

// storedDecisionConfig reads the admin setting as a decision.Config source.
// The enable toggle maps onto Provider: OFF is decision.ProviderNone, which
// (unlike "") overrides a provider the config file names; ON with no stored
// provider means typesafe, the only one; UNSET says nothing.
func (s *Server) storedDecisionConfig() (decision.Config, error) {
	vals, err := s.store.GetPlatformSettings()
	if err != nil {
		return decision.Config{}, err
	}
	cfg := decision.Config{Model: vals[settingDecisionModel]}
	switch vals[settingDecisionEnabled] {
	case "false":
		cfg.Provider = decision.ProviderNone
	case "true":
		cfg.Provider = vals[settingDecisionProvider]
		if cfg.Provider == "" {
			cfg.Provider = decision.ProviderTypesafe
		}
	default:
		cfg.Provider = vals[settingDecisionProvider]
	}
	key, kerr := s.store.GetSecretPlatformSetting(settingDecisionAPIKey)
	if kerr != nil {
		return cfg, kerr
	}
	cfg.APIKey = key
	return cfg, nil
}

// decisionSettingsResponse is the GET/PUT body. It carries whether a key is
// set and where it came from — NEVER the key, masked or otherwise: unlike the
// email key's abcd...wxyz mask, nothing here echoes any of its bytes.
type decisionSettingsResponse struct {
	// Effective is what the running server uses.
	Effective decisionEffective `json:"effective"`
	// Stored is the instance-admin setting as saved, whether or not a
	// higher-precedence source overrides it.
	Stored decisionStored `json:"stored"`
	// Env names the fields the environment sets. Each one overrides the
	// stored value, and the page disables that input.
	Env decisionEnvFields `json:"env"`
	// ReadOnly is true on Pad Cloud, where the environment is the only
	// source ("configured by the operator").
	ReadOnly bool `json:"read_only"`
	// Error describes a provider that is configured but cannot be built:
	// decisions are off because of it. Empty when healthy.
	Error string `json:"error,omitempty"`
	// StoredKeyError says the SAVED key cannot be read (not ciphertext, or
	// does not decrypt). It is separate from Error because it need not stop
	// decisions: a key from the environment or config file may be in force
	// instead (codex round 4).
	StoredKeyError string `json:"stored_key_error,omitempty"`
}

type decisionEffective struct {
	Enabled   bool   `json:"enabled"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	APIKeySet bool   `json:"api_key_set"`
}

type decisionStored struct {
	Provider  string `json:"provider"`
	Enabled   *bool  `json:"enabled"`
	Model     string `json:"model"`
	APIKeySet bool   `json:"api_key_set"`
}

type decisionEnvFields struct {
	Provider bool `json:"provider"`
	APIKey   bool `json:"api_key"`
	Model    bool `json:"model"`
}

func (s *Server) buildDecisionSettingsResponse() (decisionSettingsResponse, error) {
	s.decisionSettings.mu.Lock()
	defer s.decisionSettings.mu.Unlock()
	return s.buildDecisionSettingsResponseLocked()
}

func (s *Server) buildDecisionSettingsResponseLocked() (decisionSettingsResponse, error) {
	cfg, origins, serr := s.resolveDecisionConfigLocked()
	var resp decisionSettingsResponse
	name, model := decision.Describe(cfg)
	resp.Effective = decisionEffective{
		Enabled:   cfg.Enabled(),
		Provider:  name,
		Model:     model,
		APIKeySet: cfg.APIKey != "",
	}
	resp.Env = decisionEnvFields{
		Provider: origins.Provider == decision.SourceEnv,
		APIKey:   origins.APIKey == decision.SourceEnv,
		Model:    origins.Model == decision.SourceEnv,
	}
	resp.ReadOnly = s.cloudMode
	if !s.cloudMode {
		vals, err := s.store.GetPlatformSettings()
		if err != nil {
			return resp, err
		}
		// A key counts as saved only if it can be READ: a row the store
		// refuses (not ciphertext, or does not decrypt) is reported through
		// Error, never as a usable saved key (codex round 3).
		storedKey, kerr := s.store.GetSecretPlatformSetting(settingDecisionAPIKey)
		resp.Stored = decisionStored{
			Provider:  vals[settingDecisionProvider],
			Model:     vals[settingDecisionModel],
			APIKeySet: kerr == nil && storedKey != "",
		}
		switch vals[settingDecisionEnabled] {
		case "true":
			t := true
			resp.Stored.Enabled = &t
		case "false":
			f := false
			resp.Stored.Enabled = &f
		}
	}
	if serr != nil {
		// The store error names the failure (not ciphertext, or does not
		// decrypt), never the key, but it is not the admin's vocabulary.
		slog.Error("decision settings: stored API key unreadable", "error", serr)
		resp.StoredKeyError = "The saved API key could not be read. Enter it again to replace it."
	}
	if _, err := decision.New(cfg); err != nil {
		resp.Error = err.Error()
	}
	return resp, nil
}

// handleGetDecisionSettings: GET /api/v1/admin/decision-provider. Admin-only.
func (s *Server) handleGetDecisionSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}
	resp, err := s.buildDecisionSettingsResponse()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load decision settings")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decisionSettingsInput is the PUT body. Every field is optional; an absent
// field is left as stored. api_key is write-only: a non-empty value replaces
// the stored key, and clear_api_key removes it.
type decisionSettingsInput struct {
	Provider    *string `json:"provider"`
	Enabled     *bool   `json:"enabled"`
	Model       *string `json:"model"`
	APIKey      *string `json:"api_key"`
	ClearAPIKey bool    `json:"clear_api_key"`
}

// handleUpdateDecisionSettings: PUT /api/v1/admin/decision-provider.
// Admin-only; refused on Pad Cloud. A field the environment sets is REFUSED
// rather than stored: the write could not take effect, and a 200 would tell
// the admin it had.
func (s *Server) handleUpdateDecisionSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}
	if s.cloudMode {
		writeError(w, http.StatusForbidden, "managed_by_operator", "The decision provider is configured by the operator on this instance.")
		return
	}
	var in decisionSettingsInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	if in.APIKey != nil && in.ClearAPIKey {
		writeError(w, http.StatusBadRequest, "bad_request", "api_key and clear_api_key cannot both be set")
		return
	}

	var provider, model, key string
	if in.Provider != nil {
		provider = strings.ToLower(strings.TrimSpace(*in.Provider))
		if provider != decision.ProviderTypesafe && provider != decision.ProviderNone {
			writeError(w, http.StatusBadRequest, "bad_request", "provider must be \"typesafe\" or \"none\"")
			return
		}
	}
	if in.Model != nil {
		model = strings.TrimSpace(*in.Model)
		if len(model) > maxDecisionModelLen {
			writeError(w, http.StatusBadRequest, "bad_request", "model is too long")
			return
		}
	}
	if in.APIKey != nil {
		key = strings.TrimSpace(*in.APIKey)
		if key == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "api_key is empty; use clear_api_key to remove the stored key")
			return
		}
	}

	env := decision.EnvConfig()
	var envSet []string
	if (in.Provider != nil || in.Enabled != nil) && env.Provider != "" {
		envSet = append(envSet, "provider")
	}
	if (in.APIKey != nil || in.ClearAPIKey) && env.APIKey != "" {
		envSet = append(envSet, "api_key")
	}
	if in.Model != nil && env.Model != "" {
		envSet = append(envSet, "model")
	}
	if len(envSet) > 0 {
		writeError2(w, http.StatusConflict, "set_by_environment",
			"Set by the server environment, which overrides this setting: "+strings.Join(envSet, ", "),
			map[string]interface{}{"fields": envSet})
		return
	}

	s.decisionSettings.mu.Lock()
	defer s.decisionSettings.mu.Unlock()

	var changed []string
	// The secret first: it is the only write that can be refused for a
	// reason other than a store failure, and refusing it after the others
	// landed would leave a half-applied save.
	if in.APIKey != nil {
		if err := s.store.SetSecretPlatformSetting(settingDecisionAPIKey, key); err != nil {
			if errors.Is(err, store.ErrEncryptionUnavailable) {
				writeError(w, http.StatusConflict, "encryption_unavailable", "This server has no encryption key (PAD_ENCRYPTION_KEY), so it cannot store an API key. Set PAD_TYPESAFE_API_KEY in the environment instead.")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save the API key")
			return
		}
		changed = append(changed, settingDecisionAPIKey)
	} else if in.ClearAPIKey {
		if err := s.store.DeletePlatformSetting(settingDecisionAPIKey); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to clear the API key")
			return
		}
		changed = append(changed, settingDecisionAPIKey)
	}
	if in.Provider != nil {
		if err := s.store.SetPlatformSetting(settingDecisionProvider, provider); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save the provider")
			return
		}
		changed = append(changed, settingDecisionProvider)
	}
	if in.Enabled != nil {
		v := "false"
		if *in.Enabled {
			v = "true"
		}
		if err := s.store.SetPlatformSetting(settingDecisionEnabled, v); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save the enable toggle")
			return
		}
		changed = append(changed, settingDecisionEnabled)
	}
	if in.Model != nil {
		var err error
		if model == "" {
			err = s.store.DeletePlatformSetting(settingDecisionModel)
		} else {
			err = s.store.SetPlatformSetting(settingDecisionModel, model)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to save the model")
			return
		}
		changed = append(changed, settingDecisionModel)
	}

	if err := s.reconfigureDecisionsLocked(); err != nil {
		// Saved, but the result names a provider that cannot be built (for
		// example enabled with no key). Decisions are off; the response's
		// error field says why, which is where the page reads it.
		slog.Warn("decision provider misconfigured after settings change; decisions are OFF", "error", err)
	}
	if len(changed) > 0 {
		s.logAuditEvent(models.ActionSettingsChanged, r, settingsChangedMeta(changed))
	}

	resp, err := s.buildDecisionSettingsResponseLocked()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load decision settings")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
