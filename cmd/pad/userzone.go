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
	unset  bool
}

func installViewerZone() {
	viewerZone.load = sync.Once{}
	viewerZone.notice = sync.Once{}
	viewerZone.loc, viewerZone.unset = time.UTC, false
	cli.SetDisplayLocationResolver(func() *time.Location {
		viewerZone.load.Do(func() {
			client := cli.NewClientFromURL(getConfiguredConfig().BaseURL())
			if user, err := client.GetCurrentUser(); err == nil {
				viewerZone.loc = usertz.Resolve(user.Timezone).Loc
				viewerZone.unset = user.Timezone == nil
			}
		})
		if viewerZone.unset && formatFlag != "json" {
			viewerZone.notice.Do(func() { fmt.Fprintln(os.Stderr, timezoneNotice) })
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
		viewerZone.loc = usertz.Resolve(&d.Timezone).Loc
		viewerZone.unset = d.Note != ""
	})
	_ = cli.DisplayLocation()
}
