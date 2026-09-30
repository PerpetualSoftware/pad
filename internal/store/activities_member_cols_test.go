package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3314: the activity reads a workspace reader can reach (viewers and guests
// included) must never return a member's ip_address or user_agent; only the
// instance-admin audit reads may. This pins it at the store, so a later edit to
// any of these SELECTs that brings a column back fails here, whatever handler
// serialises the rows.
func TestActivityReads_OnlyAdminReadsReturnIPAndUserAgent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := createTestUser(t, s, "probe-3314@example.com", "Probe", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Probe", Slug: "probe-3314", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	const docID = "doc-3314"
	if _, err := s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID, DocumentID: docID, Action: "updated", Actor: "user", Source: "web",
		Metadata: `{"changes":"status: open → done"}`, UserID: u.ID,
		IPAddress: "203.0.113.77", UserAgent: "LeakProbe/3314",
	}); err != nil {
		t.Fatalf("create activity: %v", err)
	}

	one := func(name string, got []models.Activity, err error) models.Activity {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// The row must be returned, or an empty result would pass as stripped.
		if len(got) != 1 {
			t.Fatalf("%s: %d rows, want the 1 probe row", name, len(got))
		}
		return got[0]
	}

	member := map[string]models.Activity{}
	rows, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{})
	member["ListWorkspaceActivity"] = one("ListWorkspaceActivity", rows, err)
	rows, err = s.ListDocumentActivity(docID, models.ActivityListParams{})
	member["ListDocumentActivity"] = one("ListDocumentActivity", rows, err)
	rows, err = s.ListDocumentActivityBeforeTime(docID, time.Now().Add(time.Hour), "", 10)
	member["ListDocumentActivityBeforeTime"] = one("ListDocumentActivityBeforeTime", rows, err)
	for name, a := range member {
		if a.IPAddress != "" || a.UserAgent != "" {
			t.Errorf("%s (a member read) returned ip_address=%q user_agent=%q", name, a.IPAddress, a.UserAgent)
		}
	}

	admin := map[string]models.Activity{}
	rows, err = s.ListAuditLog(models.AuditLogParams{WorkspaceID: ws.ID})
	admin["ListAuditLog"] = one("ListAuditLog", rows, err)
	rows, err = s.ListUserActivity(u.ID, models.ActivityListParams{})
	admin["ListUserActivity"] = one("ListUserActivity", rows, err)
	for name, a := range admin {
		if a.IPAddress != "203.0.113.77" || a.UserAgent != "LeakProbe/3314" {
			t.Errorf("%s (an admin read) returned ip_address=%q user_agent=%q, want the stored values", name, a.IPAddress, a.UserAgent)
		}
	}
}
