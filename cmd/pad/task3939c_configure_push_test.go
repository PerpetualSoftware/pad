package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-3939 B2c: server startup starts the push dispatcher exactly when a
// gateway is configured with a usable secret, and refuses a gateway URL with
// no secret before anything starts.
//
// Kills: dropping StartPushDispatch (the URL+secret leg); starting the
// dispatcher before the secret check (the no-secret leg); starting it outside
// the gateway block (the no-URL leg).
func TestConfigurePush_StartsTheDispatcherOnlyWithAGateway(t *testing.T) {
	newSrv := func(t *testing.T) *server.Server {
		srv := server.New(storetest.NewSQLite(t))
		t.Cleanup(srv.Stop)
		return srv
	}

	t.Run("url and secret", func(t *testing.T) {
		srv := newSrv(t)
		cfg := &config.Config{PushGatewayURL: "https://push.example.test", PushGatewaySecret: "s3cret-for-the-gateway"}
		if err := configurePush(srv, cfg); err != nil {
			t.Fatalf("configurePush = %v, want nil", err)
		}
		if !srv.PushDispatchRunning() {
			t.Fatal("a configured gateway did not start the push dispatcher")
		}
	})

	t.Run("no url", func(t *testing.T) {
		srv := newSrv(t)
		if err := configurePush(srv, &config.Config{PushGatewaySecret: "s3cret-for-the-gateway"}); err != nil {
			t.Fatalf("configurePush = %v, want nil", err)
		}
		if srv.PushDispatchRunning() {
			t.Fatal("the push dispatcher started with no gateway configured")
		}
	})

	t.Run("url without a secret", func(t *testing.T) {
		srv := newSrv(t)
		err := configurePush(srv, &config.Config{PushGatewayURL: "https://push.example.test"})
		if err == nil || !strings.Contains(err.Error(), "PAD_PUSH_GATEWAY_URL is set") {
			t.Fatalf("configurePush = %v, want the missing-secret refusal", err)
		}
		if srv.PushDispatchRunning() {
			t.Fatal("the push dispatcher started before the missing-secret refusal")
		}
	})
}

// CONVE-19: the wiring is a claim. configurePush is tested above; this pins
// that server startup calls it and returns its error, and that nothing else in
// startup starts the dispatcher around it.
func TestServerStartCallsConfigurePush(t *testing.T) {
	src, err := os.ReadFile("cmd_server.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "if err := configurePush(srv, cfg); err != nil {\n\t\t\t\treturn err\n\t\t\t}") {
		t.Error("server start no longer calls configurePush(srv, cfg) and returns its error")
	}
	if n := strings.Count(body, "StartPushDispatch()"); n != 1 {
		t.Errorf("cmd_server.go calls StartPushDispatch() %d times, want exactly 1 (inside configurePush)", n)
	}
}

// TASK-3939 part C (ruling 77a8fae7): the PAD_PUSH_DISPATCH_* knobs reach the
// dispatcher through configurePush, and the interval is applied before Start.
//
// Kills: dropping any one knob's assignment (the four expectations differ);
// applying the config after StartPushDispatch (StartedInterval would be the
// 5s default, the ticker having been created before the knob landed).
func TestConfigurePush_AppliesEveryTimingKnobBeforeStart(t *testing.T) {
	srv := server.New(storetest.NewSQLite(t))
	t.Cleanup(srv.Stop)
	cfg := &config.Config{
		PushGatewayURL: "https://push.example.test", PushGatewaySecret: "s3cret-for-the-gateway",
		PushDispatchIntervalRaw: "2s", PushDispatchSettleRaw: "3s",
		PushDispatchLeaseRaw: "90s", PushDispatchLeaseMarginRaw: "20s",
	}
	if err := configurePush(srv, cfg); err != nil {
		t.Fatalf("configurePush = %v, want nil", err)
	}
	got := srv.PushDispatchApplied()
	want := server.PushDispatchApplied{
		Interval: 2 * time.Second, Settle: 3 * time.Second, Lease: 90 * time.Second,
		LeaseMargin: 20 * time.Second, StartedInterval: 2 * time.Second,
	}
	if got != want {
		t.Fatalf("dispatcher timings = %+v, want %+v", got, want)
	}
}

// Unset knobs keep the defaults (5s / 10s / 60s / 15s) through configurePush.
func TestConfigurePush_UnsetKnobsKeepTheDefaults(t *testing.T) {
	srv := server.New(storetest.NewSQLite(t))
	t.Cleanup(srv.Stop)
	if err := configurePush(srv, &config.Config{PushGatewayURL: "https://push.example.test", PushGatewaySecret: "s3cret-for-the-gateway"}); err != nil {
		t.Fatal(err)
	}
	want := server.PushDispatchApplied{Interval: 5 * time.Second, Settle: 10 * time.Second, Lease: 60 * time.Second, LeaseMargin: 15 * time.Second, StartedInterval: 5 * time.Second}
	if got := srv.PushDispatchApplied(); got != want {
		t.Fatalf("dispatcher timings = %+v, want %+v", got, want)
	}
}

// A bad knob refuses startup, names its variable, and starts no dispatcher.
// It refuses with or without a gateway URL: a malformed config is an operator
// error either way.
//
// Kills: falling back to the default on a parse error; ignoring
// SetPushDispatchConfig's lease-too-short error.
func TestConfigurePush_BadKnobRefusesStartup(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantVar string
		// wantDetail are substrings that tell the two lease causes apart: the
		// name alone cannot, since PAD_PUSH_DISPATCH_LEASE is a prefix of
		// PAD_PUSH_DISPATCH_LEASE_MARGIN (TASK-3939 part C, ruling 37e3eafa).
		wantDetail []string
	}{
		{"malformed interval", func(c *config.Config) { c.PushDispatchIntervalRaw = "soon" }, "PAD_PUSH_DISPATCH_INTERVAL", nil},
		{"whitespace-only interval", func(c *config.Config) { c.PushDispatchIntervalRaw = " " }, "PAD_PUSH_DISPATCH_INTERVAL", nil},
		{"malformed settle", func(c *config.Config) { c.PushDispatchSettleRaw = "10" }, "PAD_PUSH_DISPATCH_SETTLE", nil},
		{"zero lease", func(c *config.Config) { c.PushDispatchLeaseRaw = "0s" }, "PAD_PUSH_DISPATCH_LEASE", nil},
		{"negative margin", func(c *config.Config) { c.PushDispatchLeaseMarginRaw = "-1s" }, "PAD_PUSH_DISPATCH_LEASE_MARGIN", nil},
		{"lease shorter than send timeout plus margin", func(c *config.Config) { c.PushDispatchLeaseRaw = "30s" }, "PAD_PUSH_DISPATCH_LEASE", []string{"lease 30s", "margin 15s"}},
		{"margin that outgrows the default lease", func(c *config.Config) { c.PushDispatchLeaseMarginRaw = "50s" }, "PAD_PUSH_DISPATCH_LEASE_MARGIN", []string{"lease 1m0s", "margin 50s"}},
		{"margin of 2562047h", func(c *config.Config) { c.PushDispatchLeaseMarginRaw = "2562047h" }, "PAD_PUSH_DISPATCH_LEASE_MARGIN", []string{"lease 1m0s"}},
		// 2562047h47m16s + the 25s send timeout exceeds MaxInt64 and wraps
		// negative; 2562047h alone does not.
		{"margin that overflows the sum", func(c *config.Config) { c.PushDispatchLeaseMarginRaw = "2562047h47m16s" }, "PAD_PUSH_DISPATCH_LEASE_MARGIN", []string{"lease 1m0s"}},
	}
	for _, withURL := range []bool{true, false} {
		for _, tc := range cases {
			name := tc.name + " no url"
			if withURL {
				name = tc.name + " with url"
			}
			t.Run(name, func(t *testing.T) {
				srv := server.New(storetest.NewSQLite(t))
				t.Cleanup(srv.Stop)
				cfg := &config.Config{PushGatewaySecret: "s3cret-for-the-gateway"}
				if withURL {
					cfg.PushGatewayURL = "https://push.example.test"
				}
				tc.mutate(cfg)
				err := configurePush(srv, cfg)
				if err == nil || !strings.Contains(err.Error(), tc.wantVar) {
					t.Fatalf("configurePush = %v, want a refusal naming %s", err, tc.wantVar)
				}
				for _, d := range tc.wantDetail {
					if !strings.Contains(err.Error(), d) {
						t.Errorf("refusal %q lacks %q", err, d)
					}
				}
				if srv.PushDispatchRunning() {
					t.Fatal("the push dispatcher started despite the refusal")
				}
			})
		}
	}
}

// TASK-4051: a signed-in device registration observes the gateway URL wired
// by startup through the real HTTP handler. Kills: M1a.
func TestConfigurePush_EnablesDeviceRegistration(t *testing.T) {
	_, srv, _, tok := task4051PushServer(t)
	if err := configurePush(srv, &config.Config{PushGatewayURL: "http://gateway.test/send", PushGatewaySecret: "test-secret"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/push-devices/conn-4051", strings.NewReader(`{"platform":"android","token":"test-device","app_version":"1"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("device registration = %d %s, want 200 (push available)", rr.Code, rr.Body.String())
	}
}

func task4051PushServer(t *testing.T) (*store.Store, *server.Server, *models.User, string) {
	t.Helper()
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	u, err := s.CreateUser(models.UserCreate{Email: "push-4051@example.com", Name: "Push", Username: "push-4051", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateSession(u.ID, "android", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s, srv, u, tok
}

// TASK-4051: one pending notification reaches exactly the configured HTTP
// gateway, using notify.Gateway's messages/results contract. Kills: M1b.
func TestConfigurePush_SendsToConfiguredGateway(t *testing.T) {
	s, srv, u, tok := task4051PushServer(t)
	var sends atomic.Int64
	arrived := make(chan []notify.Message, 4)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		var body struct {
			Messages []notify.Message `json:"messages"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/internal/push/send" || r.Header.Get("X-Cloud-Secret") != "test-secret" {
			t.Errorf("unexpected gateway request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		results := make([]notify.Result, len(body.Messages))
		for i, m := range body.Messages {
			results[i] = notify.Result{ID: m.ID, Status: notify.StatusSent}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		arrived <- body.Messages
	}))
	defer gateway.Close()
	session, err := s.ValidateSession(tok)
	if err != nil || session == nil {
		t.Fatalf("ValidateSession = %v, %v", session, err)
	}
	dev, err := s.RegisterPushDevice(u.ID, session.ID, "conn-4051", "android", "test-device", "1")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE push_devices SET created_at = ? WHERE id = ?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), dev.ID)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Push 4051", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Assigned task", Fields: `{"status":"open"}`, AssignedUserID: &u.ID})
	if err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM user_notifications`)
	exec(`INSERT INTO user_notifications (id, user_id, workspace_id, item_id, kind, dedupe_key, created_at) VALUES (?, ?, ?, ?, 'assigned', ?, ?)`, "n-4051", u.ID, ws.ID, item.ID, "k-4051", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	cfg := &config.Config{PushGatewayURL: gateway.URL + "/internal/push/send", PushGatewaySecret: "test-secret", PushDispatchIntervalRaw: "10ms", PushDispatchSettleRaw: "1ms"}
	if err := configurePush(srv, cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case msgs := <-arrived:
		if len(msgs) != 1 || msgs[0].Token != "test-device" || msgs[0].Data.NID != "n-4051" {
			t.Errorf("gateway messages = %+v, want the seeded notification on its device", msgs)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no send request arrived at the configured gateway before the deadline")
	}
	srv.Stop()
	if got := sends.Load(); got != 1 {
		t.Fatalf("send requests arriving at the configured gateway = %d, want exactly 1", got)
	}
}

// TASK-4051: stopping the whole server stops its configured dispatcher too.
// Kills: M2.
func TestConfigurePush_ServerStopStopsDispatcher(t *testing.T) {
	// No Stop cleanup: if shutdown hangs, the deadline must fail cleanly.
	// A leaked goroutine is acceptable only in that failing run.
	srv := server.New(storetest.NewSQLite(t))
	if err := configurePush(srv, &config.Config{PushGatewayURL: "http://gateway.test/send", PushGatewaySecret: "test-secret"}); err != nil {
		t.Fatal(err)
	}
	if !srv.PushDispatchRunning() {
		t.Fatal("configured dispatcher is not running")
	}
	done := make(chan struct{})
	go func() { srv.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Server.Stop did not drain the dispatcher before the deadline")
	}
	if srv.PushDispatchRunning() {
		t.Error("dispatcher is still running after Server.Stop")
	}
}
