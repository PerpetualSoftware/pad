package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2819 on BOTH backends: the provenance columns, their DEFAULT for rows
// that never recorded one, and the CHECK constraint that makes the Go enum the
// only vocabulary either dialect will store.

func provenanceWorkspace(t *testing.T, s *store.Store) *models.Workspace {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Prov", Slug: "prov"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return ws
}

func newAttachment(ws *models.Workspace, source string) *models.Attachment {
	return &models.Attachment{
		WorkspaceID: ws.ID, UploadedBy: "u", StorageKey: "fs:" + fmt.Sprint(time.Now().UnixNano()),
		ContentHash: "h", MimeType: "image/png", SizeBytes: 1, Filename: "upload.bin",
		FilenameSource: source,
	}
}

func TestAttachmentFilenameSourceColumn(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			ws := provenanceWorkspace(t, s)

			for _, v := range []string{"caller", "normalised", "substituted", "derived", "unknown"} {
				a := newAttachment(ws, v)
				if err := s.CreateAttachment(a); err != nil {
					t.Fatalf("CreateAttachment(%s): %v", v, err)
				}
				got, err := s.GetAttachment(a.ID)
				if err != nil || got == nil {
					t.Fatalf("GetAttachment: %v", err)
				}
				if got.FilenameSource != v {
					t.Errorf("round trip %q: got %q", v, got.FilenameSource)
				}
			}

			// An unset source is recorded as unknown, never as caller.
			a := newAttachment(ws, "")
			if err := s.CreateAttachment(a); err != nil {
				t.Fatalf("CreateAttachment(unset): %v", err)
			}
			if got, _ := s.GetAttachment(a.ID); got.FilenameSource != "unknown" {
				t.Errorf("unset source stored as %q, want unknown", got.FilenameSource)
			}

			// Outside the enum: refused by the store before the database.
			if err := s.CreateAttachment(newAttachment(ws, "server")); err == nil {
				t.Error("CreateAttachment accepted filename_source \"server\"")
			}

			// The DATABASE enforces it too, on this dialect: a write that
			// bypasses the store is refused by the CHECK constraint.
			if _, err := s.DB().Exec(fmt.Sprintf(
				`UPDATE attachments SET filename_source = 'server' WHERE id = '%s'`, a.ID)); err == nil {
				t.Error("the database accepted filename_source 'server'; the CHECK constraint is missing")
			}

			// A row written without the column (every row that predates the
			// migration) takes the DEFAULT, which is unknown.
			legacyID := "00000000-0000-0000-0000-000000002819"
			if _, err := s.DB().Exec(fmt.Sprintf(`INSERT INTO attachments
				(id, workspace_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, created_at)
				VALUES ('%s', '%s', 'u', 'fs:legacy', 'h', 'image/png', 1, 'upload.bin', '2026-01-01T00:00:00Z')`,
				legacyID, ws.ID)); err != nil {
				t.Fatalf("legacy insert: %v", err)
			}
			if got, _ := s.GetAttachment(legacyID); got == nil || got.FilenameSource != "unknown" {
				t.Errorf("legacy row source = %v, want unknown", got)
			}
		})
	}
}

func TestMCPAuditToolNameSourceColumn(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			u, err := s.CreateUser(models.UserCreate{Email: "audit-2819@example.com", Name: "A", Password: "correct-horse-battery-staple"})
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			in := func(source models.MCPToolNameSource, req string) models.MCPAuditEntryInput {
				return models.MCPAuditEntryInput{
					UserID: u.ID, TokenKind: models.TokenKind("pat"), TokenRef: "r", ToolName: "(unknown)",
					ToolNameSource: source, ResultStatus: models.MCPAuditResultStatus("ok"), RequestID: req,
				}
			}
			for i, v := range []models.MCPToolNameSource{"caller", "sanitised", "synthesised", "unknown", ""} {
				if err := s.InsertMCPAuditEntry(in(v, fmt.Sprint("req-", i))); err != nil {
					t.Fatalf("InsertMCPAuditEntry(%q): %v", v, err)
				}
			}
			if err := s.InsertMCPAuditEntry(in("server", "req-bad")); err == nil {
				t.Error("InsertMCPAuditEntry accepted tool_name_source \"server\"")
			}
			rows, err := s.ListMCPAuditByUser(u.ID, 50, 0)
			if err != nil {
				t.Fatalf("ListMCPAuditByUser: %v", err)
			}
			got := map[string]models.MCPToolNameSource{}
			for _, r := range rows {
				got[r.RequestID] = r.ToolNameSource
			}
			want := map[string]models.MCPToolNameSource{
				"req-0": "caller", "req-1": "sanitised", "req-2": "synthesised", "req-3": "unknown",
				// Unset is recorded as unknown, never as caller.
				"req-4": "unknown",
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s: source %q, want %q", k, got[k], v)
				}
			}
			if _, err := s.DB().Exec(`UPDATE mcp_audit_log SET tool_name_source = 'server'`); err == nil {
				t.Error("the database accepted tool_name_source 'server'; the CHECK constraint is missing")
			}
		})
	}
}
