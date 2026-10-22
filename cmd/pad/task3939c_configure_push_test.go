package main

import (
	"os"
	"strings"
	"testing"

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
