package server

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Store-seeded as in TASK-4060: reads cannot create a DM or change markers.
func TestChat_GetCallerDeliveredFloorBoundary(t *testing.T) {
	f := newChatFixture(t)
	alpha, _, err := f.srv.store.EnsureAgentDM(f.owner.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	bravo, _, err := f.srv.store.EnsureAgentDM(f.owner.ID, "bravo")
	if err != nil {
		t.Fatal(err)
	}
	other := mkUser(t, f.srv, "floor-other@example.com")
	own, _, err := f.srv.store.EnsureAgentDM(other.ID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// Conversation numbers belong to each owner. Seed a different number so
	// the other user's request cannot accidentally name their own conversation.
	own.Number = 99
	if _, err = f.srv.store.DB().Exec(`UPDATE conversations SET number=? WHERE id=?`, own.Number, own.ID); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []struct {
		id, key string
		seq     int64
	}{{alpha.ID, "alpha", 2}, {alpha.ID, "", 3}, {bravo.ID, "bravo", 5}, {bravo.ID, "", 4}, {own.ID, "alpha", 7}, {own.ID, "", 6}} {
		if _, err = f.srv.store.DB().Exec(`UPDATE conversation_members SET delivered_seq=? WHERE conversation_id=? AND agent_key=?`, seed.seq, seed.id, seed.key); err != nil {
			t.Fatal(err)
		}
	}
	for _, dm := range []*models.Conversation{alpha, bravo, own} {
		if _, err = f.srv.store.DB().Exec(`UPDATE conversations SET last_seq=8 WHERE id=?`, dm.ID); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]string{}
	for _, id := range []string{alpha.ID, bravo.ID, own.ID} {
		before[id] = chatDMState(t, f, id)
	}
	token := f.token(other)
	cases := []struct {
		name, token, agent string
		number, status     int
		want               float64
	}{
		{"alpha own", f.ownerTok, "alpha", alpha.Number, 200, 2},
		{"owner distinct marker", f.ownerTok, "", alpha.Number, 200, 3},
		{"bravo own", f.ownerTok, "bravo", bravo.Number, 200, 5},
		{"alpha cannot read bravo", f.ownerTok, "alpha", bravo.Number, 404, 0},
		{"bravo cannot read alpha", f.ownerTok, "bravo", alpha.Number, 404, 0},
		{"other cannot read alpha", token, "alpha", alpha.Number, 404, 0},
		{"other human cannot read alpha", token, "", alpha.Number, 404, 0},
		{"other own", token, "alpha", own.Number, 200, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", fmt.Sprintf("%s/%d", chatBase, tc.number), nil)
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.Header.Set("X-Pad-Agent", tc.agent)
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, r)
			f.must(rr, tc.status, tc.name)
			if tc.status == 200 && chatObject(t, rr)["delivered_seq"] != tc.want {
				t.Fatalf("wrong caller marker: %s", rr.Body.String())
			}
		})
	}
	for _, id := range []string{alpha.ID, bravo.ID, own.ID} {
		if after := chatDMState(t, f, id); after != before[id] {
			t.Fatalf("GET changed DM: %s -> %s", before[id], after)
		}
	}
}

func TestChat_InboxOmitsDeliveredFloor(t *testing.T) {
	f := newChatFixture(t)
	if _, _, err := f.srv.store.EnsureAgentDM(f.owner.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	r := f.request("GET", chatBase, "alpha", nil)
	f.must(r, 200, "inbox")
	rows := chatObject(t, r)["conversations"].([]any)
	if len(rows) != 1 {
		t.Fatal("missing inbox row")
	}
	if _, ok := rows[0].(map[string]any)["delivered_seq"]; ok {
		t.Fatalf("inbox leaks GET-only marker: %s", r.Body.String())
	}
}
