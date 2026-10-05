package main

import "github.com/PerpetualSoftware/pad/internal/server"

// e2eConfigureServer is set only by files built with the e2etest tag
// (e2e_app_ca.go, TASK-3415), which the e2e harness uses for its real-install
// leg. In a default build it stays nil and nothing reads any e2e setting.
var e2eConfigureServer func(*server.Server) error
