package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

var ErrOccurrenceNotFired = errors.New("This occurrence has not fired yet, so there is nothing to acknowledge.")

type scheduleAckEvidence struct {
	AckedAt      *string `json:"acked_at"`
	ReminderID   string  `json:"reminder_id"`
	OccurrenceAt string  `json:"occurrence_at"`
	ActorUserID  string  `json:"actor_user_id"`
	AgentName    string  `json:"agent_name,omitempty"`
}

func (s *Store) ackOccurrenceTx(tx *sql.Tx, sc *models.ItemSchedule, in ResolveInput, occurrence, now time.Time) (*ResolveResult, error) {
	if sc.ReminderID == nil || !sc.ReminderFired {
		return nil, ErrOccurrenceNotFired
	}
	res := &ResolveResult{}
	if sc.ReminderAckedAt != nil {
		return s.commitResolve(tx, sc.WorkspaceID, sc.ItemID, res)
	}
	ts := now.UTC().Format(time.RFC3339)
	actor := in.Actor
	if actor != "agent" {
		actor = "user"
	}
	evidence := scheduleAckEvidence{ReminderID: *sc.ReminderID, OccurrenceAt: NormalizeInstant(occurrence), ActorUserID: in.ActorUserID, AgentName: in.AgentName}
	before, _ := json.Marshal(evidence)
	evidence.AckedAt = &ts
	after, _ := json.Marshal(evidence)
	if _, err := tx.Exec(s.q(`UPDATE item_reminders SET acked_at=?,updated_at=? WHERE id=?`), ts, ts, *sc.ReminderID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(s.q(`UPDATE item_schedules SET updated_at=? WHERE id=?`), ts, sc.ID); err != nil {
		return nil, err
	}
	if err := s.journalScheduleChangeReasonTx(tx, sc.ID, "ack", OccurrenceKey(sc, evidence.OccurrenceAt, *sc.NextWall), "", string(before), string(after), actor, "", "user"); err != nil {
		return nil, err
	}
	prefix := "reminder:" + *sc.ReminderID + ":" + evidence.OccurrenceAt + ":"
	if _, err := tx.Exec(s.q(`UPDATE user_notifications SET push_state='skipped',skip_reason='occurrence_acknowledged',claim_token='',claimed_at=NULL
 WHERE workspace_id=? AND item_id=? AND kind='reminder' AND push_state='pending' AND substr(dedupe_key,1,?)=?`), sc.WorkspaceID, sc.ItemID, len(prefix), prefix); err != nil {
		return nil, fmt.Errorf("cancel acknowledged occurrence pushes: %w", err)
	}
	res.AckChanged = true
	return s.commitResolve(tx, sc.WorkspaceID, sc.ItemID, res)
}

// fillScheduleAcknowledgements reads evidence for current occurrences in one
// journal query and resolves all account names in one query. It uses the caller
// connection, including when the caller holds a transaction or a single-slot pool.
func (s *Store) fillScheduleAcknowledgements(q Queryer, schedules []*models.ItemSchedule) error {
	current := map[string]*models.ItemSchedule{}
	var ids []any
	for _, sc := range schedules {
		if sc.ReminderAckedAt != nil && sc.NextAt != nil && sc.NextWall != nil && sc.EndedAt == nil {
			// The reminder row is the source of the time; the journal row only
			// adds who acknowledged, so a missing journal row still shows when.
			sc.AcknowledgedAt = sc.ReminderAckedAt
			current[sc.ID] = sc
			ids = append(ids, sc.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(s.q(`SELECT c.schedule_id,c.occurrence_key,c.after_json,c.actor
 FROM schedule_changes c JOIN item_schedules sc ON sc.id=c.schedule_id
 WHERE c.change_type='ack' AND c.schedule_id IN (`+placeholders(len(ids))+`)
 AND (c.occurrence_key=sc.next_wall OR c.occurrence_key=sc.next_at||'|'||sc.next_wall)
 ORDER BY c.recorded_at DESC,c.id DESC`), ids...)
	if err != nil {
		return fmt.Errorf("read occurrence acknowledgements: %w", err)
	}
	users := map[string]bool{}
	for rows.Next() {
		var id, key, raw, actor string
		if err = rows.Scan(&id, &key, &raw, &actor); err != nil {
			rows.Close()
			return err
		}
		sc := current[id]
		if sc == nil || sc.AcknowledgedBy != nil || key != OccurrenceKey(sc, *sc.NextAt, *sc.NextWall) {
			continue
		}
		var v scheduleAckEvidence
		if json.Unmarshal([]byte(raw), &v) != nil || v.AckedAt == nil || *v.AckedAt != *sc.ReminderAckedAt {
			continue
		}
		sc.AcknowledgedBy = &models.AcknowledgedBy{UserID: v.ActorUserID, Actor: actor, AgentName: v.AgentName}
		if v.ActorUserID != "" {
			users[v.ActorUserID] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	ids = nil
	for id := range users {
		ids = append(ids, id)
	}
	rows, err = q.Query(s.q(`SELECT id,name FROM users WHERE id IN (`+placeholders(len(ids))+`)`), ids...)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		names[id] = name
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, sc := range current {
		if sc.AcknowledgedBy != nil {
			sc.AcknowledgedBy.Name = names[sc.AcknowledgedBy.UserID]
		}
	}
	return nil
}

// AcknowledgedScheduleItemIDs suppresses ordinary next/ready suggestions while
// preserving independent reminder suggestions and all ordinary item listings.
func (s *Store) AcknowledgedScheduleItemIDs(workspaceID string) (map[string]bool, error) {
	rows, err := s.db.Query(s.q(`SELECT sc.item_id FROM item_schedules sc JOIN item_reminders r ON r.schedule_id=sc.id
 WHERE sc.workspace_id=? AND sc.ended_at IS NULL AND sc.next_at IS NOT NULL AND r.acked_at IS NOT NULL`), workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// importedAckEvidenceTx rewrites an imported acknowledgement's before or after
// JSON into the canonical evidence shape. It decodes through the same struct
// the agenda reader uses (encoding/json matches field names without regard to
// case, so checking a map key would let ACTOR_USER_ID through), keeps
// actor_user_id only when that user is a member of the destination workspace,
// and normalizes the agent name as the live write path does. A bundle is not
// trusted to name an account: without this, a crafted row could attribute an
// acknowledgement to any user and read that user's name back on the agenda.
// Text that does not decode as evidence is not stored.
func (s *Store) importedAckEvidenceTx(tx *sql.Tx, workspaceID string, raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	var v scheduleAckEvidence
	if json.Unmarshal([]byte(*raw), &v) != nil {
		return nil, nil
	}
	v.AgentName = models.NormalizeAgentName(v.AgentName)
	if v.ActorUserID != "" {
		var one int
		err := tx.QueryRow(s.q(`SELECT 1 FROM workspace_members WHERE workspace_id=? AND user_id=?`), workspaceID, v.ActorUserID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			v.ActorUserID = ""
		} else if err != nil {
			return nil, fmt.Errorf("check imported acknowledgement actor: %w", err)
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := string(b)
	return &out, nil
}
