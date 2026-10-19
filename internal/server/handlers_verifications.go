package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/acceptance"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/verification"
)

// Verification evidence tied to a revision (TASK-3617). A verdict is recorded
// on the item that CARRIES the acceptance criteria (a spec or plan), per
// criterion, optionally at a commit and naming the item that did the work.
// Freshness is judged at read time by internal/verification; nothing about a
// row is ever updated.

const (
	// maxVerificationResults bounds one batch. A spec with more criteria
	// than this is recorded in more than one call.
	maxVerificationResults = 100
	// maxVerificationEvidence is the evidence text cap, in bytes. Evidence
	// is a pointer (a test name, file:line, why it can't be told), not a log.
	maxVerificationEvidence = 2048
	maxVerificationURL      = 2048
	// verificationWriteAttempts bounds the re-read when the item changes
	// between parsing its criteria and storing the batch.
	verificationWriteAttempts = 3
)

type verificationResultInput struct {
	Criterion   string `json:"criterion"`
	Verdict     string `json:"verdict"`
	Evidence    string `json:"evidence"`
	EvidenceURL string `json:"evidence_url"`
}

type verificationWriteInput struct {
	RunID        string                    `json:"run_id"`
	CodeRevision string                    `json:"code_revision"`
	Via          string                    `json:"via"`
	Results      []verificationResultInput `json:"results"`
}

// handleListItemVerifications returns an item's verdicts with their state.
// GET /api/v1/workspaces/{slug}/items/{itemSlug}/verifications[?all=1]
//
// By default, the newest verdict per criterion; ?all=1 returns the history,
// in which older rows read superseded.
func (s *Server) handleListItemVerifications(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveVisibleItem(w, r, workspaceID)
	if item == nil {
		return
	}
	all := r.URL.Query().Get("all")
	history := all == "1" || all == "true"

	rows, err := s.store.ListItemVerifications(workspaceID, item.ID, !history)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	rows, err = s.judgeVerifications(r, workspaceID, item, rows)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":           item.Ref,
		"criteria":      verification.Index(acceptance.Parse(item.Content)).IDs(),
		"verifications": rows,
	})
}

// handleCreateItemVerifications records one verify pass's verdicts.
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/verifications
//
// The batch is atomic: one unknown criterion, bad verdict or oversized
// evidence refuses all of it, so a report is never half-recorded.
func (s *Server) handleCreateItemVerifications(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveVisibleItem(w, r, workspaceID)
	if item == nil {
		return
	}
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}

	var in verificationWriteInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be JSON")
		return
	}
	if msg := validateVerificationInput(&in); msg != "" {
		writeError(w, http.StatusBadRequest, "validation_error", msg)
		return
	}

	// resolveVia resolves the via item in THIS workspace and requires it to
	// be visible to the caller. Missing and invisible are one answer, so the
	// write is not an existence oracle for items the caller cannot see. It
	// runs again on every retry: a check on the first read authorizes only
	// that read (TASK-3617 r1 F1 for the item, r2 L1 for via).
	resolveVia := func() (*models.Item, bool) {
		if in.Via == "" {
			return nil, true
		}
		v, err := s.store.ResolveItem(workspaceID, in.Via)
		if err != nil {
			writeInternalError(w, err)
			return nil, false
		}
		if v != nil {
			visible, err := s.itemVisibleTo(r, workspaceID, v)
			if err != nil {
				writeInternalError(w, err)
				return nil, false
			}
			if !visible {
				v = nil
			}
		}
		if v == nil {
			writeError2(w, http.StatusBadRequest, "unknown_via", "via does not name an item in this workspace", map[string]interface{}{"via": in.Via})
			return nil, false
		}
		return v, true
	}
	via, ok := resolveVia()
	if !ok {
		return
	}

	runID := in.RunID
	if runID == "" {
		runID = uuid.NewString()
	}
	actor, _ := actorFromRequest(r)
	agent := agentNameFromRequest(r)

	var stored []models.ItemVerification
	for attempt := 0; ; attempt++ {
		crit := verification.Index(acceptance.Parse(item.Content))
		rows := make([]models.ItemVerification, 0, len(in.Results))
		var unknown []string
		for _, res := range in.Results {
			lookup, hash := crit.Find(res.Criterion)
			if lookup != verification.LookupFound {
				unknown = append(unknown, res.Criterion)
				continue
			}
			row := models.ItemVerification{
				CriterionID:   res.Criterion,
				CriterionHash: hash,
				CodeRevision:  in.CodeRevision,
				Verdict:       res.Verdict,
				Evidence:      res.Evidence,
				EvidenceURL:   res.EvidenceURL,
				RunID:         runID,
				CreatedBy:     actor,
				UserID:        currentUserID(r),
				AgentName:     agent,
			}
			if via != nil {
				row.ViaItemID = via.ID
			}
			rows = append(rows, row)
		}
		if len(unknown) > 0 {
			writeError2(w, http.StatusBadRequest, "unknown_criterion",
				"Each criterion must be a non-placeholder AC-N id that appears exactly once in the item's acceptance criteria",
				map[string]interface{}{"unknown": unknown, "criteria": crit.IDs()})
			return
		}

		if s.verificationBeforeInsertHook != nil {
			s.verificationBeforeInsertHook(attempt)
		}
		var err error
		stored, err = s.store.InsertItemVerifications(workspaceID, item.ID, item.Seq, rows)
		if err == nil {
			break
		}
		switch {
		case errors.Is(err, store.ErrVerificationItemGone):
			writeError(w, http.StatusNotFound, "not_found", "Item not found")
			return
		case errors.Is(err, store.ErrVerificationViaGone):
			writeError2(w, http.StatusBadRequest, "unknown_via", "via does not name an item in this workspace", map[string]interface{}{"via": in.Via})
			return
		case errors.Is(err, store.ErrVerificationItemChanged) && attempt+1 < verificationWriteAttempts:
			// Re-read and re-parse: the hashes must come from the version
			// they are stored against.
			fresh, rerr := s.store.ResolveItem(workspaceID, item.ID)
			if rerr != nil {
				writeInternalError(w, rerr)
				return
			}
			if fresh == nil {
				writeError(w, http.StatusNotFound, "not_found", "Item not found")
				return
			}
			// The write that moved the item may have moved it out of the
			// caller's reach (another collection, a revoked grant), so the
			// doors are re-run on the version about to be written (TASK-3617
			// r1). A check on the first read authorizes only that read.
			if !s.requireItemVisible(w, r, workspaceID, fresh) {
				return
			}
			if !s.requireEditPermission(w, r, workspaceID, fresh.ID, fresh.CollectionID) {
				return
			}
			if via, ok = resolveVia(); !ok {
				return
			}
			item = fresh
			continue
		case errors.Is(err, store.ErrVerificationItemChanged):
			writeError(w, http.StatusConflict, "item_changed", "The item kept changing while its criteria were read; try again")
			return
		default:
			writeInternalError(w, err)
			return
		}
	}

	s.logActivityWithMeta(workspaceID, item.ID, "verified", r, verificationActivityMeta(r, runID, stored))
	s.publishItemVerified(workspaceID, item, r)

	all, err := s.store.ListItemVerifications(workspaceID, item.ID, false)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	all, err = s.judgeVerifications(r, workspaceID, item, all)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	byID := make(map[string]models.ItemVerification, len(all))
	for _, v := range all {
		byID[v.ID] = v
	}
	out := make([]models.ItemVerification, 0, len(stored))
	for _, v := range stored {
		if j, ok := byID[v.ID]; ok {
			out = append(out, j)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ref":           item.Ref,
		"run_id":        runID,
		"verifications": out,
	})
}

// validateVerificationInput normalizes in and returns the first refusal, or
// "" when the batch is well-formed. Criterion ids are checked against the
// item later, because that needs its body.
func validateVerificationInput(in *verificationWriteInput) string {
	if len(in.Results) == 0 {
		return "results must name at least one criterion"
	}
	if len(in.Results) > maxVerificationResults {
		return "results may name at most 100 criteria per call"
	}
	if in.RunID != "" {
		if _, err := uuid.Parse(in.RunID); err != nil {
			return "run_id must be a UUID"
		}
		in.RunID = strings.ToLower(in.RunID)
	}
	in.CodeRevision = strings.ToLower(strings.TrimSpace(in.CodeRevision))
	if in.CodeRevision != "" && !models.IsFullCommitSHA(in.CodeRevision) {
		return "code_revision must be a full 40-hex commit SHA, or empty"
	}
	in.Via = strings.TrimSpace(in.Via)
	seen := make(map[string]bool, len(in.Results))
	for i := range in.Results {
		res := &in.Results[i]
		res.Criterion = strings.TrimSpace(res.Criterion)
		if res.Criterion == "" {
			return "each result needs a criterion"
		}
		if seen[res.Criterion] {
			return "criterion " + res.Criterion + " appears more than once in results"
		}
		seen[res.Criterion] = true
		if !models.ValidVerdict(res.Verdict) {
			return "verdict must be pass, fail or cant_tell"
		}
		if len(res.Evidence) > maxVerificationEvidence {
			return "evidence may be at most 2048 bytes"
		}
		res.EvidenceURL = strings.TrimSpace(res.EvidenceURL)
		if res.EvidenceURL != "" {
			if len(res.EvidenceURL) > maxVerificationURL {
				return "evidence_url may be at most 2048 bytes"
			}
			u, err := url.Parse(res.EvidenceURL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return "evidence_url must be an absolute http(s) URL"
			}
		}
	}
	return ""
}

// judgeVerifications sets state and possibly_stale on rows, and fills via
// refs the caller may see. The PRs considered are the item's own github_pr
// and each visible via item's.
func (s *Server) judgeVerifications(r *http.Request, workspaceID string, item *models.Item, rows []models.ItemVerification) ([]models.ItemVerification, error) {
	var prs []verification.LinkedPR
	if pr := mergedPR(item); pr != nil {
		prs = append(prs, *pr)
	}
	viaRefs := map[string]string{}
	for _, v := range rows {
		if v.ViaItemID == "" {
			continue
		}
		if _, done := viaRefs[v.ViaItemID]; done {
			continue
		}
		viaRefs[v.ViaItemID] = ""
		vi, err := s.store.GetItem(v.ViaItemID)
		if err != nil || vi == nil || vi.WorkspaceID != workspaceID || vi.DeletedAt != nil {
			continue
		}
		visible, err := s.itemVisibleTo(r, workspaceID, vi)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
		viaRefs[v.ViaItemID] = vi.Ref
		if pr := mergedPR(vi); pr != nil {
			prs = append(prs, *pr)
		}
	}
	for i := range rows {
		rows[i].ViaRef = viaRefs[rows[i].ViaItemID]
	}
	crit := verification.Index(acceptance.Parse(item.Content))
	verification.Judge(rows, crit, prs)
	verification.SortByCriteria(rows, crit)
	return rows, nil
}

// mergedPR is an item's linked PR when it has merged with a known merge SHA.
func mergedPR(item *models.Item) *verification.LinkedPR {
	if item == nil || item.CodeContext == nil || item.CodeContext.PullRequest == nil {
		return nil
	}
	pr := item.CodeContext.PullRequest
	if pr.MergeSHA == "" || pr.MergedAt == "" {
		return nil
	}
	return &verification.LinkedPR{Number: pr.Number, MergeSHA: pr.MergeSHA, MergedAt: pr.MergedAt}
}

// itemVisibleTo is requireItemVisible without the response: the app ceiling,
// then the store's visibility rules.
func (s *Server) itemVisibleTo(r *http.Request, workspaceID string, item *models.Item) (bool, error) {
	if !appCeilingAllows(r, item.CollectionID) {
		return false, nil
	}
	return s.checkItemVisible(workspaceID, item, currentUser(r), workspaceRole(r), isBearerAuth(r))
}

// verificationActivityMeta is the activity's metadata: the run and a verdict
// count, never evidence text.
func verificationActivityMeta(r *http.Request, runID string, rows []models.ItemVerification) string {
	counts := map[string]int{}
	for _, v := range rows {
		counts[v.Verdict]++
	}
	raw, err := json.Marshal(map[string]any{"run_id": runID, "criteria": len(rows), "verdicts": counts})
	if err != nil {
		return agentMeta(r, "")
	}
	return agentMeta(r, string(raw))
}

// publishItemVerified rides the existing item_updated SSE event rather than a
// new type: every client already subscribes to it, and a new type would be
// dropped by the web's ITEM_EVENTS allowlist until each consumer learned it.
// The item's seq is unchanged by a verification, so a delta-syncing client
// finds nothing new in the row itself, which is true.
func (s *Server) publishItemVerified(workspaceID string, item *models.Item, r *http.Request) {
	actor, source := actorFromRequest(r)
	name := ""
	if u := currentUser(r); u != nil {
		name = u.Name
	}
	s.publishItemEventWithName(sseItemUpdated, workspaceID, item.ID, item.Title, item.CollectionSlug, actor, name, source, item.Seq)
}
