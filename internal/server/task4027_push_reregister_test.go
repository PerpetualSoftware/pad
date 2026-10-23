package server

import (
	"fmt"
	"testing"
	"time"
)

// TASK-4027. A same-user re-register keeps the device's created_at, so a row
// created between the first registration and the re-register is still SENT:
// G7 reads devs[0].CreatedAt and must not see the re-register as a new device.
func TestPushDispatch_ReregisterKeepsPendingRowDeliverable(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		// The device was first registered 2h ago; the row is 30m old, after that
		// registration and well inside pushExpireAfter. A row aged exactly
		// pushExpireAfter expired whenever a second boundary fell between the
		// insert and the tick, since both run on the real clock (BUG-4033).
		f.exec(`UPDATE push_devices SET created_at = ?, updated_at = ? WHERE id = ?`, f.ts(2*time.Hour), f.ts(2*time.Hour), f.devID)
		id := f.row("comment", func(o *rowOpt) { o.age = 30 * time.Minute })
		d, err := srv.store.RegisterPushDevice(f.bob.ID, f.devSess, "conn-again", "android", "tok-bob-"+fmt.Sprint(f.n), "2")
		must(t, err)
		if d.ID != f.devID {
			t.Fatalf("re-register minted id %s, want the kept %s", d.ID, f.devID)
		}
		f.tick()
		if st := f.state(id); st.State != "sent" {
			t.Fatalf("row = %s/%q, want sent: the re-register must not look like a new device", st.State, st.Reason)
		}
	})
}

// TASK-4027, cross-user reclaim at the dispatcher: bob registers the phone, carol
// signs in on it later (carol owns it), then bob re-registers (bob owns it
// again). Carol's pending row must reach no message; bob's row, created after
// bob's ORIGINAL registration, is sent.
func TestPushDispatch_CrossUserReclaimRoutesToTheLatestRegistrant(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		carol := f.user("carol")
		must(t, srv.store.AddWorkspaceMember(f.ws.ID, carol.ID, "editor"))
		f.exec(`DELETE FROM push_devices WHERE user_id = ?`, f.bob.ID)
		_, bobSess := f.device(f.bob, "tok-phone", 3*time.Hour)
		f.device(carol, "tok-phone", time.Hour)
		bobRow := f.row("comment")
		carolRow := f.row("comment", func(o *rowOpt) { o.user = carol })
		_, err := srv.store.RegisterPushDevice(f.bob.ID, bobSess, "conn-back", "android", "tok-phone", "2")
		must(t, err)
		f.tick()
		if st := f.state(bobRow); st.State != "sent" {
			t.Fatalf("bob's row = %s/%q, want sent: bob reclaimed the phone", st.State, st.Reason)
		}
		if st := f.state(carolRow); st.State == "sent" {
			t.Fatalf("carol's row was sent after bob reclaimed the phone")
		}
		msgs := f.fake.all()
		if len(msgs) != 1 || msgs[0].Data.NID != bobRow {
			t.Fatalf("messages = %+v, want exactly bob's one", msgs)
		}
	})
}

// TASK-4031: two users registered one phone in the same second and the PREVIOUS
// registrant (bob) holds the larger id. Ownership follows the registration
// sequence, so bob's pending row reaches no message and carol's is sent.
// Kills: the ownership clause comparing updated_at then id.
func TestPushDispatch_SameSecondReclaimIgnoresIDOrder(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		carol := f.user("carol")
		must(t, srv.store.AddWorkspaceMember(f.ws.ID, carol.ID, "editor"))
		f.exec(`DELETE FROM push_devices WHERE user_id = ?`, f.bob.ID)
		bobDev, _ := f.device(f.bob, "tok-phone", 3*time.Hour)
		carolDev, _ := f.device(carol, "tok-phone", 3*time.Hour)
		same := f.ts(3 * time.Hour)
		f.exec(`UPDATE push_devices SET created_at = ?, updated_at = ? WHERE token = ?`, same, same, "tok-phone")
		f.exec(`UPDATE push_devices SET id = ? WHERE id = ?`, "zzzz-bob-larger", bobDev)
		f.exec(`UPDATE push_devices SET id = ? WHERE id = ?`, "aaaa-carol-smaller", carolDev)
		bobRow := f.row("comment")
		carolRow := f.row("comment", func(o *rowOpt) { o.user = carol })
		f.tick()
		if st := f.state(carolRow); st.State != "sent" {
			t.Fatalf("carol's row = %s/%q, want sent: she registered last", st.State, st.Reason)
		}
		if st := f.state(bobRow); st.State == "sent" {
			t.Fatalf("bob's row was sent to a phone carol registered after him")
		}
		msgs := f.fake.all()
		if len(msgs) != 1 || msgs[0].Data.NID != carolRow {
			t.Fatalf("messages = %+v, want exactly carol's one", msgs)
		}
	})
}
