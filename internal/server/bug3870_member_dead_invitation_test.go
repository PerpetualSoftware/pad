package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3870: an invitee who is already a member, holding an invitation whose
// inviter can no longer grant it (BUG-3770, BUG-3634), is already in: the
// accept by code is BUG-3281's idempotent 200 with the role they hold, and the
// /join preview, asked by that member's own session, reports
// inviter_can_grant true so the page offers the Accept. Anyone else asking
// the preview, including an anonymous code holder, reads what they read before.

func bug3870PreviewCanGrant(t *testing.T, f *accessFixture, code, tok string) bool {
	t.Helper()
	rr := f.do("GET", "/api/v1/invitations/"+code+"/preview", tok, nil)
	f.must(rr, http.StatusOK, "preview")
	var preview struct {
		Found           bool  `json:"found"`
		InviterCanGrant *bool `json:"inviter_can_grant"`
	}
	parseJSON(t, rr, &preview)
	if !preview.Found || preview.InviterCanGrant == nil {
		t.Fatalf("preview found=%v inviter_can_grant=%v, want found with the field", preview.Found, preview.InviterCanGrant)
	}
	return *preview.InviterCanGrant
}

func TestBUG3870_AMemberWithADeadInvitationIsAlreadyIn(t *testing.T) {
	losses := append([]bug3770Loss{}, bug3770Losses...)
	losses = append(losses, bug3770Loss{"restricted to specific collections", func(f *accessFixture, u *models.User) {
		if err := f.srv.store.SetMemberCollectionAccess(f.wsID, u.ID, "specific", nil); err != nil {
			t.Fatal(err)
		}
	}})
	for _, loss := range losses {
		t.Run(loss.name, func(t *testing.T) {
			forEachDialect(t, func(t *testing.T, d store.DriverType) {
				f := newAccessFixture(t, d)
				email := "member-3870@example.com"
				member := mkUser(t, f.srv, email)
				if err := f.srv.store.AddWorkspaceMember(f.wsID, member.ID, "viewer"); err != nil {
					t.Fatal(err)
				}
				inv := bug3770Inviter(t, f, email, loss.lose)

				if bug3870PreviewCanGrant(t, f, inv.Code, "") {
					t.Error("anonymous preview: inviter_can_grant true; a code holder must not learn the invitee's membership")
				}
				if bug3870PreviewCanGrant(t, f, inv.Code, f.ownerTok) {
					t.Error("preview as a different signed-in member: inviter_can_grant true, want false")
				}
				if !bug3870PreviewCanGrant(t, f, inv.Code, f.token(member)) {
					t.Error("preview as the invitee, already a member: inviter_can_grant false, so the page tells them to get re-invited")
				}

				rr := f.do("POST", "/api/v1/invitations/"+inv.Code+"/accept", f.token(member), nil)
				f.must(rr, http.StatusOK, "accept by code as an existing member")
				if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, member.ID); m == nil || m.Role != "viewer" {
					t.Fatalf("membership after accept: %+v, want the viewer role kept", m)
				}
				if got, _ := f.srv.store.GetInvitation(inv.ID); got == nil || got.AcceptedAt == nil {
					t.Errorf("invitation after accept: %+v, want accepted", got)
				}
			})
		})
	}
}

// Control: signed in as the invitee who is NOT a member, the preview still
// reports the dead invitation, so the true above is the membership and not
// the session.
func TestBUG3870_ANonMemberInviteeStillSeesTheDeadInvitation(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		email := "outsider-3870@example.com"
		invitee := mkUser(t, f.srv, email)
		inv := bug3770Inviter(t, f, email, bug3770Losses[0].lose)
		if bug3870PreviewCanGrant(t, f, inv.Code, f.token(invitee)) {
			t.Error("preview as a non-member invitee: inviter_can_grant true, want false")
		}
	})
}
