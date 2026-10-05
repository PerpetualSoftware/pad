//go:build e2etest

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/PerpetualSoftware/pad/internal/server"
)

// The e2e harness's door for a test app origin's CA (TASK-3415, lead
// ruling): a server built with -tags e2etest trusts the PEM CA bundle named
// by PAD_E2E_APP_CA for app manifest and artifact fetches and for webhook
// deliveries (both read Server.SetAppFetchTLS). Release and production
// builds do not contain this file, so no environment or config can widen
// what they trust; TestE2EAppCADoorIsNotInDefaultBuilds checks that.
func init() {
	e2eConfigureServer = func(srv *server.Server) error {
		path := os.Getenv("PAD_E2E_APP_CA")
		if path == "" {
			return nil
		}
		pem, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("PAD_E2E_APP_CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("PAD_E2E_APP_CA: %s holds no PEM certificate", path)
		}
		srv.SetAppFetchTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
		return nil
	}
}
