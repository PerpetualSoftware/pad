package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Instance-admin settings for the decision provider (TASK-3121).

const decisionTestKey = "tsk-live-0123456789-SECRETKEYBODY-abcdef"

// decisionKeyPrefix fronts randomDecisionKey's random hex body. The prefix
// is excluded from leak checks; the body's 4-byte windows are what they scan.
const decisionKeyPrefix = "tsk_"

func randomDecisionKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return decisionKeyPrefix + hex.EncodeToString(b)
}

func clearDecisionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PAD_DECISION_PROVIDER", "")
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	t.Setenv("PAD_DECISION_MODEL", "")
}

// decisionSettingsServer returns a server with an encryption key, the
// production question sets, a tick that never fires on its own (enabling
// typesafe must not reach the network from a test), and an admin session.
func decisionSettingsServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	srv.store.SetEncryptionKey(key)
	srv.SetDecisionTickChannel(make(chan time.Time))
	reg, err := decision.ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDecisionBase(decision.Config{}, reg)
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	return srv, token
}

func putDecisionSettings(t *testing.T, srv *Server, token string, body map[string]interface{}) (*decisionSettingsResponse, int, string) {
	t.Helper()
	rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/decision-provider", body, token)
	if rr.Code != http.StatusOK {
		return nil, rr.Code, rr.Body.String()
	}
	var resp decisionSettingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &resp, rr.Code, rr.Body.String()
}

func getDecisionSettings(t *testing.T, srv *Server, token string) (decisionSettingsResponse, string) {
	t.Helper()
	rr := doRequestWithCookie(srv, "GET", "/api/v1/admin/decision-provider", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp decisionSettingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, rr.Body.String()
}

// The lead's pin (04:35Z on TASK-3121): the key is stored through the
// PAD_ENCRYPTION_KEY path and never returned by ANY read — not this
// endpoint's, not the generic settings endpoint's, not as a mask.
func TestDecisionSettings_KeyNeverReturnedAndEncryptedAtRest(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	key := randomDecisionKey(t)

	resp, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{
		"enabled": true, "provider": "typesafe", "api_key": key,
	})
	if code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if !resp.Effective.APIKeySet || !resp.Stored.APIKeySet {
		t.Fatalf("api_key_set not reported after a key was saved: %+v", resp)
	}

	// Every read door, checked for any 4-byte window of the key's random
	// body: a mask such as the email key's abcd...wxyz carries exactly four
	// bytes from each end, so a wider window would pass it (it did — the
	// first version of this check used 8 and a masking mutant survived).
	_, getBody := getDecisionSettings(t, srv, token)
	settingsRR := doRequestWithCookie(srv, "GET", "/api/v1/admin/settings", nil, token)
	random := strings.TrimPrefix(key, decisionKeyPrefix)
	for name, b := range map[string]string{"PUT response": body, "GET decision-provider": getBody, "GET settings": settingsRR.Body.String()} {
		for i := 0; i+4 <= len(random); i++ {
			if strings.Contains(b, random[i:i+4]) {
				t.Errorf("%s carries key bytes %q: %s", name, random[i:i+4], b)
				break
			}
		}
	}
	if strings.Contains(settingsRR.Body.String(), settingDecisionAPIKey) {
		t.Errorf("GET /admin/settings names the decision key row: %s", settingsRR.Body.String())
	}

	raw, err := srv.store.GetPlatformSetting(settingDecisionAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "enc:") || strings.Contains(raw, random) {
		t.Errorf("stored key is not ciphertext: %q", raw)
	}
	if got, err := srv.store.GetSecretPlatformSetting(settingDecisionAPIKey); err != nil || got != key {
		t.Errorf("round trip = %q, %v; want the saved key", got, err)
	}
}

// The generic settings PATCH must not become a second, unencrypted door.
func TestDecisionSettings_GenericSettingsPatchCannotWriteTheKey(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/admin/settings", map[string]string{settingDecisionAPIKey: "plaintext-key"}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rr.Code, rr.Body.String())
	}
	if raw, _ := srv.store.GetPlatformSetting(settingDecisionAPIKey); raw != "" {
		t.Errorf("generic PATCH stored the decision key: %q", raw)
	}
}

// Takes effect without a restart, in BOTH directions, observed at the write
// doors — whether an item write owes a decision job — which is the half the
// swap exists for. Each leg's end state is the opposite of the other's, so
// neither can pass on a runner that never changed.
func TestDecisionSettings_ChangeTakesEffectWithoutRestart(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	if srv.decisionRunner() != nil {
		t.Fatal("precondition: no provider configured, but a runner is attached")
	}
	ws, coll := decisionSettingsWorkspace(t, srv)

	jobFor := func(title string) bool {
		t.Helper()
		item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		j, err := srv.store.GetDecisionJob(item.ID, decision.AttentionSetName)
		if err != nil {
			t.Fatal(err)
		}
		return j != nil
	}

	if jobFor("before") {
		t.Fatal("precondition: a job was owed with decisions off")
	}
	if _, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"enabled": true, "api_key": decisionTestKey}); code != http.StatusOK {
		t.Fatalf("enable: %d %s", code, body)
	}
	if srv.decisionRunner() == nil {
		t.Fatal("enabling did not attach a runner")
	}
	if !jobFor("while enabled") {
		t.Error("an item written after enabling owes no decision job: the write doors did not follow the change")
	}
	if _, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"enabled": false}); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	if srv.decisionRunner() != nil {
		t.Error("disabling left a runner attached")
	}
	if jobFor("after disabling") {
		t.Error("an item written after disabling still owes a decision job")
	}
}

func decisionSettingsWorkspace(t *testing.T, srv *Server) (*models.Workspace, *models.Collection) {
	t.Helper()
	owner, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || owner == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Decisions", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	coll, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	return ws, coll
}

// Env overrides the stored setting (day-73 ruling): GET says which fields
// the environment sets, the effective value is the environment's, and a
// write to such a field is refused rather than stored-but-ineffective.
func TestDecisionSettings_EnvOverridesAndRefusesTheWrite(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	if _, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"model": "jev-1.0.0"}); code != http.StatusOK {
		t.Fatalf("seed model: %d %s", code, body)
	}

	t.Setenv("PAD_DECISION_MODEL", "jev-9.9.9")
	resp, _ := getDecisionSettings(t, srv, token)
	if !resp.Env.Model || resp.Env.Provider || resp.Env.APIKey {
		t.Errorf("env fields = %+v, want model only", resp.Env)
	}
	if resp.Stored.Model != "jev-1.0.0" {
		t.Errorf("stored model = %q, want the saved jev-1.0.0 shown beneath the override", resp.Stored.Model)
	}

	rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/decision-provider", map[string]interface{}{"model": "jev-2.0.0"}, token)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "set_by_environment") {
		t.Fatalf("PUT of an env-set field = %d %s, want 409 set_by_environment", rr.Code, rr.Body.String())
	}
	if got, _ := srv.store.GetPlatformSetting(settingDecisionModel); got != "jev-1.0.0" {
		t.Errorf("refused write still changed the stored model to %q", got)
	}

	// An env-set provider takes the enable toggle with it: the toggle writes
	// Provider, which the environment decides.
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderNone)
	rr = doRequestWithCookie(srv, "PUT", "/api/v1/admin/decision-provider", map[string]interface{}{"enabled": true}, token)
	if rr.Code != http.StatusConflict {
		t.Errorf("toggle with env provider set = %d, want 409", rr.Code)
	}
}

// Effective enablement follows env over admin over file, per field.
func TestDecisionSettings_Precedence(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	reg, _ := decision.ProductionRegistry()
	srv.SetDecisionBase(decision.Config{Provider: decision.ProviderTypesafe, APIKey: "file-key", Model: "jev-file"}, reg)
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	if resp, _ := getDecisionSettings(t, srv, token); !resp.Effective.Enabled || resp.Effective.Model != "jev-file" {
		t.Fatalf("file source not in effect: %+v", resp.Effective)
	}

	// The admin toggle OFF disables what the file enabled.
	resp, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"enabled": false})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if resp.Effective.Enabled || srv.decisionRunner() != nil {
		t.Errorf("admin OFF did not override the file's provider: %+v", resp.Effective)
	}

	// The environment overrides the admin OFF.
	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	if resp, _ := getDecisionSettings(t, srv, token); !resp.Effective.Enabled || !resp.Env.Provider {
		t.Errorf("env provider did not override the admin OFF: %+v", resp)
	}
}

// Pad Cloud: env is the only source, the section is read-only, and a stored
// setting is ignored even when present.
func TestDecisionSettings_CloudIsEnvOnlyAndReadOnly(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	if _, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"enabled": true, "api_key": decisionTestKey}); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, body)
	}
	srv.cloudMode = true
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}

	resp, _ := getDecisionSettings(t, srv, token)
	if !resp.ReadOnly {
		t.Error("cloud GET is not read_only")
	}
	if resp.Effective.Enabled || srv.decisionRunner() != nil {
		t.Errorf("cloud honoured the stored admin setting: %+v", resp.Effective)
	}
	if resp.Stored.APIKeySet || resp.Stored.Enabled != nil {
		t.Errorf("cloud GET reports a stored setting: %+v", resp.Stored)
	}

	rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/decision-provider", map[string]interface{}{"enabled": false}, token)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "managed_by_operator") {
		t.Errorf("cloud PUT = %d %s, want 403 managed_by_operator", rr.Code, rr.Body.String())
	}

	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "env-key")
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	if resp, _ := getDecisionSettings(t, srv, token); !resp.Effective.Enabled {
		t.Errorf("cloud did not honour the environment: %+v", resp.Effective)
	}
}

// Enabled with no key saves, reports why decisions are off, and runs none.
func TestDecisionSettings_EnabledWithoutKeyReportsTheError(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	resp, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"enabled": true})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if resp.Error == "" || srv.decisionRunner() != nil {
		t.Errorf("enabled-without-key: error=%q runner=%v, want an error and no runner", resp.Error, srv.decisionRunner() != nil)
	}
}

func TestDecisionSettings_AdminOnlyAndInputValidation(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)

	for _, tc := range []struct {
		name string
		body map[string]interface{}
	}{
		{"unknown provider", map[string]interface{}{"provider": "openai"}},
		{"empty key", map[string]interface{}{"api_key": "  "}},
		{"key and clear", map[string]interface{}{"api_key": "k", "clear_api_key": true}},
		{"long model", map[string]interface{}{"model": strings.Repeat("m", maxDecisionModelLen+1)}},
	} {
		rr := doRequestWithCookie(srv, "PUT", "/api/v1/admin/decision-provider", tc.body, token)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", tc.name, rr.Code, rr.Body.String())
		}
	}

	if rr := doRequest(srv, "GET", "/api/v1/admin/decision-provider", nil); rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Errorf("unauthenticated GET = %d", rr.Code)
	}
}

// The store boundary refuses a secret without a key instead of falling back
// to plaintext (the lead's ruling, amended: kept as a defence, one test).
// `pad server` cannot reach this — it will not boot without a key.
func TestSecretPlatformSetting_RefusesWithoutEncryptionKey(t *testing.T) {
	srv := testServer(t)
	if srv.store.HasEncryptionKey() {
		t.Fatal("precondition: test store has an encryption key")
	}
	err := srv.store.SetSecretPlatformSetting(settingDecisionAPIKey, decisionTestKey)
	if !errors.Is(err, store.ErrEncryptionUnavailable) {
		t.Fatalf("err = %v, want ErrEncryptionUnavailable", err)
	}
	if raw, _ := srv.store.GetPlatformSetting(settingDecisionAPIKey); raw != "" {
		t.Errorf("refused write stored %q", raw)
	}
}

// blockingProvider blocks in Ask until its context is cancelled, so a test
// can hold a pass in flight and observe what ends it.
type blockingProvider struct {
	entered chan struct{}
	exited  chan error
}

func (p *blockingProvider) Name() string  { return "blocking" }
func (p *blockingProvider) Model() string { return "blocking-1" }
func (p *blockingProvider) Ask(ctx context.Context, _ any, _ map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
	close(p.entered)
	<-ctx.Done()
	p.exited <- ctx.Err()
	return nil, decision.Usage{}, ctx.Err()
}

// Codex round 1 (P2): disabling the provider must end a pass already in
// flight, not let the old runner keep sending item data for the rest of it.
// The job must also stay owed with no attempt counted, for whatever runs next.
func TestDecisionSettings_DisableCancelsTheInFlightPass(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	ws, coll := decisionSettingsWorkspace(t, srv)

	reg, err := decision.ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	p := &blockingProvider{entered: make(chan struct{}), exited: make(chan error, 1)}
	srv.SetDecisionRunner(decision.NewRunner(srv.store, p, reg))
	// Start the loop (its tick channel never fires) so the pass below runs
	// with the production limit, lease and runner id, which Start sets.
	srv.StartDecisionTick()
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "held", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.runDecisionTick(context.Background())
	}()
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: the pass never reached the provider")
	}

	if _, code, body := putDecisionSettings(t, srv, token, map[string]interface{}{"enabled": false}); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	select {
	case err := <-p.exited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("provider call ended with %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("disabling did not cancel the provider call in flight")
	}
	<-done

	j, err := srv.store.GetDecisionJob(item.ID, decision.AttentionSetName)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil || j.Attempts != 0 || j.ClaimedBy != "" {
		t.Errorf("job after a cancelled pass = %+v, want still owed, unclaimed, 0 attempts", j)
	}
}

// Codex round 1 (P1): a settings write racing Stop() must not start a tick
// after Stop() drained the background goroutines.
func TestDecisionSettings_NoTickStartsAfterStop(t *testing.T) {
	clearDecisionEnv(t)
	srv, _ := decisionSettingsServer(t)
	srv.stopDecisionTick()

	t.Setenv("PAD_DECISION_PROVIDER", decision.ProviderTypesafe)
	t.Setenv("PAD_TYPESAFE_API_KEY", "env-key")
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	if srv.decisionRunner() == nil {
		t.Fatal("precondition: the configure did not attach a runner, so a start was never attempted")
	}
	srv.decisionTick.mu.Lock()
	running := srv.decisionTick.running
	srv.decisionTick.mu.Unlock()
	if running {
		t.Error("a reconfigure after stopDecisionTick started the tick loop")
	}
}

// Codex round 2: a decision_api_key row that is not ciphertext — from a
// restore, a migration, or a direct write — is refused, not used as a live key.
func TestDecisionSettings_PlaintextKeyRowIsNotUsed(t *testing.T) {
	clearDecisionEnv(t)
	srv, token := decisionSettingsServer(t)
	if err := srv.store.SetPlatformSetting(settingDecisionAPIKey, "plaintext-live-key"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetPlatformSetting(settingDecisionEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.GetSecretPlatformSetting(settingDecisionAPIKey); !errors.Is(err, store.ErrSecretNotEncrypted) {
		t.Fatalf("GetSecretPlatformSetting on a plaintext row = %v, want ErrSecretNotEncrypted", err)
	}
	_ = srv.ConfigureDecisions()
	if srv.decisionRunner() != nil {
		t.Error("a plaintext key row configured a live provider")
	}
	resp, body := getDecisionSettings(t, srv, token)
	if resp.Effective.APIKeySet || resp.Error == "" || resp.StoredKeyError == "" {
		t.Errorf("plaintext row: effective=%+v error=%q stored_key_error=%q, want no key in effect, a build error, and a stored-key error", resp.Effective, resp.Error, resp.StoredKeyError)
	}

	// Codex round 4: an environment key overrides the unreadable row, the
	// provider runs, and the response must not claim it is not running.
	t.Setenv("PAD_TYPESAFE_API_KEY", "env-key")
	if err := srv.ConfigureDecisions(); err != nil {
		t.Fatal(err)
	}
	resp, _ = getDecisionSettings(t, srv, token)
	if !resp.Effective.Enabled || resp.Error != "" || resp.StoredKeyError == "" {
		t.Errorf("env key over an unreadable row: effective=%+v error=%q stored_key_error=%q, want running, no build error, the stored-key error still named", resp.Effective, resp.Error, resp.StoredKeyError)
	}
	t.Setenv("PAD_TYPESAFE_API_KEY", "")
	if resp.Stored.APIKeySet {
		t.Error("plaintext row reported as a saved key (stored.api_key_set)")
	}
	if strings.Contains(body, "plaintext-live-key") {
		t.Errorf("GET carries the plaintext row: %s", body)
	}
}

// Codex round 4 claimed a disabled runner makes the next tick panic and kill
// the loop. It does not — RunOnce is nil-safe — and this pins that: the loop
// ticks through a nil runner and still runs the provider after a re-enable.
func TestDecisionSettings_TickLoopSurvivesDisableAndReenable(t *testing.T) {
	clearDecisionEnv(t)
	srv, _ := decisionSettingsServer(t)
	ws, coll := decisionSettingsWorkspace(t, srv)
	reg, err := decision.ProductionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	tick := make(chan time.Time)
	srv.SetDecisionTickChannel(tick)
	first := &attentionStub{}
	srv.SetDecisionRunner(decision.NewRunner(srv.store, first, reg))
	srv.StartDecisionTick()

	send := func(what string) {
		t.Helper()
		select {
		case tick <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatalf("the tick loop is gone: %s was never received", what)
		}
	}
	srv.SetDecisionRunner(nil)
	send("a pass with a nil runner")
	send("the pass after it") // received only if the loop survived the first

	counting := &countingProvider{called: make(chan struct{}, 1)}
	srv.SetDecisionRunner(decision.NewRunner(srv.store, counting, reg))
	if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "after re-enable", Fields: `{"status":"open"}`}); err != nil {
		t.Fatal(err)
	}
	send("the pass after re-enabling")
	select {
	case <-counting.called:
	case <-time.After(10 * time.Second):
		t.Fatal("after disable and re-enable the loop never ran the new provider")
	}
}

type countingProvider struct{ called chan struct{} }

func (p *countingProvider) Name() string  { return "counting" }
func (p *countingProvider) Model() string { return "counting-1" }
func (p *countingProvider) Ask(_ context.Context, _ any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
	select {
	case p.called <- struct{}{}:
	default:
	}
	out := map[string]decision.Answer{}
	for k := range qs {
		out[k] = decision.Answer{Kind: decision.KindNoul, Noul: 0.1}
	}
	return out, decision.Usage{}, nil
}
