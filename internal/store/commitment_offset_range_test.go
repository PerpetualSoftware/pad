package store

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestLinkOffsetDaysRange(t *testing.T) {
	for _, mode := range []string{"elapsed", "civil"} {
		for _, days := range []int{3000000000, -3000000000} {
			for _, update := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/update=%t", mode, days, update), func(t *testing.T) {
					f := newLinkFix(t, 2)
					var id string
					if update {
						id = f.link(t, 0, 1, "target", "target", oneDay).ID
					}
					before := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
					o := models.ScheduleLinkOffset{Mode: mode, ElapsedSecs: 60, Days: days, Zone: ""}
					if mode == "civil" {
						o.Zone = "UTC"
					}
					var err error
					if update {
						_, err = f.s.UpdateScheduleLink(f.ws.ID, id, models.ScheduleLinkUpdate{Offset: &o}, f.allEdit())
					} else {
						_, err = f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[1].ID, ChildRole: "target", Offset: o}, f.allEdit())
					}
					var refused *LinkRefusedError
					if !errors.As(err, &refused) || refused.Code != "link_invalid" || refused.Detail != "offset is out of range" {
						t.Fatalf("offset refusal = %v, want link_invalid: offset is out of range", err)
					}
					want := 0
					if update {
						want = 1
					}
					if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE offset_elapsed=86400`) != want || f.count(t, `SELECT COUNT(*) FROM schedule_links`) != want || f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides`) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_changes`) != before {
						t.Fatal("refused offset wrote rows")
					}
				})
			}
		}
		o := models.ScheduleLinkOffset{Mode: mode, Days: math.MaxInt32}
		if mode == "civil" {
			o.Zone = "UTC"
		}
		_, err := offsetOf(o)
		if err != nil {
			t.Fatalf("MaxInt32 %s: %v", mode, err)
		}
	}
}

func TestLinkOffsetDaysRangePostgres(t *testing.T) {
	f := newLinkPG(t, 2)
	_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[1].ID, ChildRole: "target", Offset: models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 60, Days: 3000000000}}, f.access())
	var refused *LinkRefusedError
	if !errors.As(err, &refused) || refused.Code != "link_invalid" || refused.Detail != "offset is out of range" {
		t.Fatalf("offset refusal = %v", err)
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM schedule_links`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("links=%d: %v", n, err)
	}
}
