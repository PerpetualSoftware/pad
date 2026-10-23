package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/server"
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
