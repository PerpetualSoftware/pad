package server

import (
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4423_R06Redaction(t *testing.T) {
	f := newGraphHTTP(t, graphHard(), models.CommitmentRolesInput{}, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
	f.link(0, 1, "deadline", "target", "")
	f.link(0, 2, "deadline", "target", "")
	f.link(0, 3, "deadline", "target", "")
	for _, i := range []int{1, 2} {
		if _, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.item.ID, f.items[i].ID, f.access); err != nil {
			t.Fatal(err)
		}
	}
	g := f.guest(0)
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.items[1].ID, g.ID, "view", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	rr := f.as(f.token(g, ""), "GET", f.path(f.item), nil)
	if rr.Code != 200 {
		t.Fatalf("GET %d %s", rr.Code, rr.Body.String())
	}
	m := schedMap(t, rr)
	links, _ := m["links"].([]any)
	gates, _ := m["gates"].(map[string]any)
	gs, _ := gates["gates"].([]any)
	if len(links) != 1 || len(gs) != 1 {
		t.Errorf("filtered graph absent: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.items[2].ID) {
		t.Fatal("hidden graph end disclosed")
	}
	for _, i := range []int{1, 2} {
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET fields='{"status":"done"}' WHERE id=?`, f.items[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	// This fixture's task schema has a terminal status, so finished dependents stay put.
	p := f.reneg(f.token(g, ""), true, "", false)
	if p.Code != 200 {
		t.Fatalf("preview %d %s", p.Code, p.Body.String())
	}
	pm := schedMap(t, p)
	rev, _ := pm["revision"].(string)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(rev) {
		t.Fatalf("revision %q", rev)
	}
	cascade, _ := pm["cascade"].(map[string]any)
	left, _ := cascade["left_in_place"].([]any)
	if len(left) != 1 {
		t.Errorf("readable left-in-place absent: %s", p.Body.String())
	}
	if strings.Contains(p.Body.String(), f.items[2].ID) {
		t.Fatal("hidden preview end disclosed")
	}
}
