package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xarmian/pad/internal/models"
)

func TestCreateActivityDebounced_NonUpdateActions(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// "created" should always produce a new row, never debounce
	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "created",
		Actor:       "user",
		Source:      "web",
	})
	if err != nil {
		t.Fatalf("first CreateActivityDebounced error: %v", err)
	}

	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "created",
		Actor:       "user",
		Source:      "web",
	})
	if err != nil {
		t.Fatalf("second CreateActivityDebounced error: %v", err)
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "created"})
	if len(activities) != 2 {
		t.Errorf("non-update actions should not be debounced: expected 2, got %d", len(activities))
	}
}

func TestCreateActivityDebounced_CoalescesUpdates(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// First "updated" activity
	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		Metadata:    `{"changes":"status: open -> active"}`,
	})
	if err != nil {
		t.Fatalf("first update error: %v", err)
	}

	// Second "updated" activity within cooldown — should coalesce
	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		Metadata:    `{"changes":"priority: low -> high"}`,
	})
	if err != nil {
		t.Fatalf("second update error: %v", err)
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities) != 1 {
		t.Fatalf("expected 1 coalesced activity, got %d", len(activities))
	}

	// Verify metadata was merged
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(activities[0].Metadata), &meta); err != nil {
		t.Fatalf("failed to parse metadata: %v", err)
	}
	changes, _ := meta["changes"].(string)
	if changes != "status: open -> active; priority: low -> high" {
		t.Errorf("expected merged changes, got %q", changes)
	}
}

func TestCreateActivityDebounced_DifferentUsersDontCoalesce(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// Create two real users so foreign key constraints are satisfied
	userA, err := s.CreateUser(models.UserCreate{Email: "alice@test.com", Name: "Alice", Password: "pass-a"})
	if err != nil {
		t.Fatalf("create user A error: %v", err)
	}
	userB, err := s.CreateUser(models.UserCreate{Email: "bob@test.com", Name: "Bob", Password: "pass-b"})
	if err != nil {
		t.Fatalf("create user B error: %v", err)
	}

	// First update by user A
	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		UserID:      userA.ID,
	})
	if err != nil {
		t.Fatalf("user A update error: %v", err)
	}

	// Second update by user B — should NOT coalesce
	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		UserID:      userB.ID,
	})
	if err != nil {
		t.Fatalf("user B update error: %v", err)
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities) != 2 {
		t.Errorf("different users should not coalesce: expected 2, got %d", len(activities))
	}
}

func TestCreateActivityDebounced_DifferentDocsDontCoalesce(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc1 := createTestDoc(t, s, ws.ID, "Doc 1", "content 1")
	doc2 := createTestDoc(t, s, ws.ID, "Doc 2", "content 2")

	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc1.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
	})
	if err != nil {
		t.Fatalf("doc1 update error: %v", err)
	}

	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc2.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
	})
	if err != nil {
		t.Fatalf("doc2 update error: %v", err)
	}

	// Each doc should have its own activity
	act1, _ := s.ListDocumentActivity(doc1.ID, models.ActivityListParams{Action: "updated"})
	act2, _ := s.ListDocumentActivity(doc2.ID, models.ActivityListParams{Action: "updated"})
	if len(act1) != 1 || len(act2) != 1 {
		t.Errorf("different docs should not coalesce: doc1=%d, doc2=%d", len(act1), len(act2))
	}
}

func TestCreateActivityDebounced_TimestampBumped(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// First update
	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
	})
	if err != nil {
		t.Fatalf("first update error: %v", err)
	}

	activities1, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	ts1 := activities1[0].CreatedAt

	// Pause so timestamps differ (RFC3339 has 1-second resolution)
	time.Sleep(1100 * time.Millisecond)

	// Second update — should bump timestamp
	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
	})
	if err != nil {
		t.Fatalf("second update error: %v", err)
	}

	activities2, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities2) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(activities2))
	}
	ts2 := activities2[0].CreatedAt

	if !ts2.After(ts1) {
		t.Errorf("timestamp should be bumped: first=%v, second=%v", ts1, ts2)
	}
}

func TestCreateActivityDebounced_MetadataMerge(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// Update with no changes metadata
	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		Metadata:    `{}`,
	})
	if err != nil {
		t.Fatalf("first update error: %v", err)
	}

	// Update with changes metadata — should add changes
	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		Metadata:    `{"changes":"title: Old -> New"}`,
	})
	if err != nil {
		t.Fatalf("second update error: %v", err)
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(activities))
	}

	var meta map[string]interface{}
	json.Unmarshal([]byte(activities[0].Metadata), &meta)
	changes, _ := meta["changes"].(string)
	if changes != "title: Old -> New" {
		t.Errorf("expected changes from second update, got %q", changes)
	}
}

func TestCreateActivityDebounced_AgentMetaPreserved(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// First update with agent metadata
	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "agent",
		Source:      "cli",
		Metadata:    `{"agent":"claude","changes":"status: open -> active"}`,
	})
	if err != nil {
		t.Fatalf("first update error: %v", err)
	}

	// Second update with agent metadata
	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "agent",
		Source:      "cli",
		Metadata:    `{"agent":"claude","changes":"priority: low -> high"}`,
	})
	if err != nil {
		t.Fatalf("second update error: %v", err)
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(activities))
	}

	var meta map[string]interface{}
	json.Unmarshal([]byte(activities[0].Metadata), &meta)
	if meta["agent"] != "claude" {
		t.Errorf("agent metadata lost: %v", meta)
	}
	changes, _ := meta["changes"].(string)
	if changes != "status: open -> active; priority: low -> high" {
		t.Errorf("expected merged changes, got %q", changes)
	}
}

func TestCreateActivityDebounced_MultipleRapidSaves(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// Simulate 10 rapid autosaves
	for i := 0; i < 10; i++ {
		err := s.CreateActivityDebounced(models.Activity{
			WorkspaceID: ws.ID,
			DocumentID:  doc.ID,
			Action:      "updated",
			Actor:       "user",
			Source:      "web",
		})
		if err != nil {
			t.Fatalf("save %d error: %v", i, err)
		}
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities) != 1 {
		t.Errorf("10 rapid saves should coalesce to 1 activity, got %d", len(activities))
	}
}

func TestCreateActivityDebounced_NoUserID(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	// Two updates with no user ID (pre-auth mode)
	err := s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		UserID:      "",
	})
	if err != nil {
		t.Fatalf("first update error: %v", err)
	}

	err = s.CreateActivityDebounced(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		UserID:      "",
	})
	if err != nil {
		t.Fatalf("second update error: %v", err)
	}

	activities, _ := s.ListDocumentActivity(doc.ID, models.ActivityListParams{Action: "updated"})
	if len(activities) != 1 {
		t.Errorf("expected 1 coalesced activity for no-user mode, got %d", len(activities))
	}
}

func TestMergeActivityMeta(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		incoming string
		wantKey  string
		wantVal  string
	}{
		{
			name:     "both have changes",
			existing: `{"changes":"status: open -> active"}`,
			incoming: `{"changes":"priority: low -> high"}`,
			wantKey:  "changes",
			wantVal:  "status: open -> active; priority: low -> high",
		},
		{
			name:     "only incoming has changes",
			existing: `{}`,
			incoming: `{"changes":"title: Old -> New"}`,
			wantKey:  "changes",
			wantVal:  "title: Old -> New",
		},
		{
			name:     "only existing has changes",
			existing: `{"changes":"status: open -> active"}`,
			incoming: `{}`,
			wantKey:  "changes",
			wantVal:  "status: open -> active",
		},
		{
			name:     "neither has changes",
			existing: `{}`,
			incoming: `{}`,
			wantKey:  "",
			wantVal:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeActivityMeta(tt.existing, tt.incoming)
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(result), &m); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			got, _ := m[tt.wantKey].(string)
			if tt.wantKey != "" && got != tt.wantVal {
				t.Errorf("expected %q=%q, got %q", tt.wantKey, tt.wantVal, got)
			}
			if tt.wantKey == "" {
				if _, exists := m["changes"]; exists {
					t.Errorf("expected no changes key, but found one: %v", m)
				}
			}
		})
	}
}
