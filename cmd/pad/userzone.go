package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

const timezoneNotice = "times shown in UTC; set yours with pad auth timezone <IANA>"

// Process-local only: a later invocation must see changes made in web settings.
var viewerZone struct {
	load   sync.Once
	notice sync.Once
	loc    *time.Location
	note   string // the stderr notice; "" when the zone is set and loads
}

func installViewerZone() {
	viewerZone.load = sync.Once{}
	viewerZone.notice = sync.Once{}
	viewerZone.loc, viewerZone.note = time.UTC, ""
	cli.SetDisplayLocationResolver(func() *time.Location {
		viewerZone.load.Do(func() {
			// Never exit or prompt from a renderer: unconfigured means UTC, quietly.
			cfg := getConfig()
			applyPadTomlOverride(cfg)
			if !cfg.IsConfigured() {
				return
			}
			client := cli.NewClientFromURL(cfg.BaseURL())
			if user, err := client.GetCurrentUser(); err == nil {
				noteViewerZone(user.Timezone, user.Timezone == nil)
			}
		})
		if viewerZone.note != "" && formatFlag != "json" {
			viewerZone.notice.Do(func() { fmt.Fprintln(os.Stderr, viewerZone.note) })
		}
		return viewerZone.loc
	})
}

// Seed before any rendering, including commands with no instant columns.
// Older dashboards without zone metadata leave the lazy lookup available.
func seedDashboardZone(raw json.RawMessage) {
	var d struct {
		Timezone string `json:"timezone"`
		Note     string `json:"timezone_note"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Timezone == "" {
		return
	}
	viewerZone.load.Do(func() {
		noteViewerZone(&d.Timezone, d.Note != "")
	})
	_ = cli.DisplayLocation()
}

// A zone this machine's tzdata cannot load renders in UTC, and the notice names it (r4-C1).
func noteViewerZone(name *string, unset bool) {
	z := usertz.Resolve(name)
	viewerZone.loc, viewerZone.note = z.Loc, ""
	if unset {
		viewerZone.note = timezoneNotice
	} else if !z.Set {
		viewerZone.note = "times shown in UTC; your time zone " + *name + " is not available on this machine"
	}
}
