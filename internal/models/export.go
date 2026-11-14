package models

// Supported workspace bundle versions. WorkspaceExportVersion is the version an
// export writes and the newest an import accepts.
//
// Version 3 (TASK-4353) added time links (schedule_links). It is a bump rather
// than an additive member on version 2 because a reader that predates links
// ignores an unknown key: a v2 reader would restore the schedules and quietly
// lose every link, whereas a version it does not know is refused outright
// ("unsupported export version").
// Version 4 adds commitment gates and soft-deleted blocker item rows.
const (
	WorkspaceExportVersion1 = 1
	WorkspaceExportVersion2 = 2
	WorkspaceExportVersion3 = 3
	WorkspaceExportVersion  = 4
)

// WorkspaceExport is the complete portable representation of a workspace.
type WorkspaceExport struct {
	Version      int                 `json:"version"` // Workspace export format version.
	ExportedAt   string              `json:"exported_at"`
	Workspace    WorkspaceExportMeta `json:"workspace"`
	Collections  []CollectionExport  `json:"collections"`
	Items        []ItemExport        `json:"items"`
	Comments     []CommentExport     `json:"comments,omitempty"`
	ItemLinks    []ItemLinkExport    `json:"item_links,omitempty"`
	ItemVersions []ItemVersionExport `json:"item_versions,omitempty"`
	// Reminders round-trip with the workspace (IDEA-2641). They are
	// item-scoped workspace CONTENT, like links and versions, not per-user
	// state like stars and watches — which is the line this list has always
	// drawn, and it puts reminders on the exported side of it. Without them a
	// backup/restore or a SQLite→Postgres migration silently loses every
	// pending reminder, and "silently" is the part that matters: nothing in
	// the destination would show that anything was dropped.
	Reminders []ReminderExport `json:"reminders,omitempty"`
	// Verifications round-trip too (TASK-3617). They are user-written
	// evidence (a verdict, its evidence, the revisions it was about) that
	// nothing can rebuild, unlike typed decisions, which a provider
	// regenerates. Without them a SQLite->Postgres migration drops every one.
	// Additive and omitempty, like reminders, so the format version holds.
	Verifications []VerificationExport `json:"verifications,omitempty"`
	// Schedules include commitment roles, overrides and occurrence actuals.
	Schedules       []ScheduleExport       `json:"schedules,omitempty"`
	ScheduleChanges []ScheduleChangeExport `json:"schedule_changes,omitempty"`
	// ScheduleLinks are the time links between the schedules above, each with
	// its per-occurrence overrides (version 3). Both ends of a link name a
	// ScheduleExport.ID of this bundle, remapped on import.
	ScheduleLinks []ScheduleLinkExport `json:"schedule_links,omitempty"`
	// ScheduleGates preserve item blockers and the occurrence captured by each gate (version 4).
	ScheduleGates []ScheduleGateExport `json:"schedule_gates,omitempty"`
	// OmittedScheduleLinks counts the links an export left out because an end
	// is not in the bundle (its item is deleted). It names nothing about them,
	// is not part of the bundle's wire form, and is 0 on a decoded bundle.
	OmittedScheduleLinks int `json:"-"`
	hasV2Members         bool
	hasV3Members         bool
	hasV4Members         bool
}

// ScheduleGateExport carries a gate with source ids for remapping on import.
// A nil blocker schedule means the blocker is judged by its item status.
type ScheduleGateExport struct {
	ID                   string  `json:"id"`
	BlockedItemID        string  `json:"blocked_item_id"`
	BlockerItemID        string  `json:"blocker_item_id"`
	BlockerScheduleID    *string `json:"blocker_schedule_id"`
	BlockerOccurrenceKey string  `json:"blocker_occurrence_key"`
	OpenedAt             *string `json:"opened_at"`
	CreatedAt            string  `json:"created_at"`
}

// ScheduleExport is one item schedule in a workspace bundle. ID is a source
// key for remapping and is never stored at the destination. error_code is
// recomputed; the managed reminder acknowledgement is not carried.
//
// RDates and ExDates are []any, not []string, so ONE malformed entry (a
// number, an object) is a dropped and reported schedule at import rather than
// a decode failure that loses the whole bundle. The import re-marshals them
// from the parsed values and never keeps the bundle's text (BUG-4193).
type ScheduleExport struct {
	ID          string                   `json:"id,omitempty"`
	Roles       *ScheduleRolesExport     `json:"roles,omitempty"`
	Overrides   []ScheduleOverrideExport `json:"overrides,omitempty"`
	ItemID      string                   `json:"item_id"`
	Kind        string                   `json:"kind"`
	Dtstart     string                   `json:"dtstart"`
	Dtend       string                   `json:"dtend,omitempty"`
	Tz          string                   `json:"tz,omitempty"`
	Floating    bool                     `json:"floating,omitempty"`
	Rrule       string                   `json:"rrule,omitempty"`
	RDates      []any                    `json:"rdates,omitempty"`
	ExDates     []any                    `json:"exdates,omitempty"`
	Anchor      string                   `json:"anchor"`
	Audience    string                   `json:"audience"`
	LeadMinutes int                      `json:"lead_minutes,omitempty"`

	// The outstanding occurrence. Empty on an ended schedule.
	NextAt         string `json:"next_at,omitempty"`
	NextWall       string `json:"next_wall,omitempty"`
	NextOverrideAt string `json:"next_override_at,omitempty"`
	AnchorWall     string `json:"anchor_wall,omitempty"`
	EndedAt        string `json:"ended_at,omitempty"`
	// EndedDismissedAt is when the ended schedule was dismissed from
	// Outstanding (TASK-4537); it requires EndedAt. Always export the key:
	// its absence identifies an ending from before dismissal was supported.
	EndedDismissedAt string `json:"ended_dismissed_at"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`

	// Reminder is the managed reminder row's state; nil on an ended schedule.
	// The row itself is rebuilt on import (export skips managed rows).
	Reminder    *ScheduleReminderExport    `json:"reminder,omitempty"`
	Occurrences []ScheduleOccurrenceExport `json:"occurrences,omitempty"`

	// Milestone forecast and outcome members (TASK-4358). Optional, so the
	// bundle stays v2; a v1 bundle that carries any of them is refused.
	Forecast           *ForecastExport           `json:"forecast,omitempty"`
	ForecastEvents     []ForecastEvent           `json:"forecast_events,omitempty"`
	Children           []MilestoneChildExport    `json:"children,omitempty"`
	CompletionEvidence []MilestoneEvidenceExport `json:"completion_evidence,omitempty"`
}

// ForecastExport is a schedule's current forecast.
type ForecastExport struct {
	Value     string `json:"value"`
	SetBy     string `json:"set_by"`
	SetByKind string `json:"set_by_kind"`
	SetAt     string `json:"set_at"`
	Source    string `json:"source,omitempty"`
}

// MilestoneChildExport is one selection row, active or removed. ItemID is the
// source item id, remapped on import.
type MilestoneChildExport struct {
	ItemID    string `json:"item_id"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason,omitempty"`
	AddedBy   string `json:"added_by"`
	AddedAt   string `json:"added_at"`
	RemovedBy string `json:"removed_by,omitempty"`
	RemovedAt string `json:"removed_at,omitempty"`
}

// MilestoneEvidenceExport is one completion-evidence row.
type MilestoneEvidenceExport struct {
	OccurrenceKey string `json:"occurrence_key"`
	ItemID        string `json:"item_id"`
	Kind          string `json:"kind"`
	CloseState    string `json:"close_state"`
	DoneValue     string `json:"done_value,omitempty"`
	RecordedAt    string `json:"recorded_at"`
}

// ScheduleRolesExport carries the stored offsets from the recurrence seed.
type ScheduleRolesExport struct {
	TargetDays       int     `json:"target_days"`
	TargetSecs       int     `json:"target_secs"`
	EarliestDays     *int    `json:"earliest_days,omitempty"`
	EarliestSecs     *int    `json:"earliest_secs,omitempty"`
	DeadlineDays     *int    `json:"deadline_days,omitempty"`
	DeadlineSecs     *int    `json:"deadline_secs,omitempty"`
	OccupiedDays     *int    `json:"occupied_days,omitempty"`
	OccupiedSecs     *int    `json:"occupied_secs,omitempty"`
	DeadlineBehavior *string `json:"deadline_behavior,omitempty"`
	Outcome          string  `json:"outcome,omitempty"`
	RolesOrigin      string  `json:"roles_origin"`
}

// ScheduleOverrideExport is a sparse override of one occurrence role.
type ScheduleOverrideExport struct {
	OccurrenceKey string `json:"occurrence_key"`
	Role          string `json:"role"`
	ValueAt       string `json:"value_at"`
	ValueWall     string `json:"value_wall,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// ScheduleLinkExport is one time link in a workspace bundle (version 3): the
// child's ChildRole date follows the parent's ParentRole date plus Offset. The
// ends are source schedule ids (ScheduleExport.ID), remapped on import; the
// link's own id is not carried and is minted at the destination. The import
// re-checks the link as a live write does and never moves a date.
type ScheduleLinkExport struct {
	ParentScheduleID string                       `json:"parent_schedule_id"`
	ParentRole       string                       `json:"parent_role"`
	ChildScheduleID  string                       `json:"child_schedule_id"`
	ChildRole        string                       `json:"child_role"`
	Offset           ScheduleLinkOffset           `json:"offset"`
	CancelPolicy     string                       `json:"cancel_policy"`
	CreatedAt        string                       `json:"created_at"`
	UpdatedAt        string                       `json:"updated_at"`
	Overrides        []ScheduleLinkOverrideExport `json:"overrides,omitempty"`
}

// ScheduleLinkOverrideExport is a link's exception for one occurrence of its
// child: its own offset, a "no longer needed" mark, or both. OccurrenceKey is
// the child's key, canonicalised on import by the child's floating policy.
type ScheduleLinkOverrideExport struct {
	OccurrenceKey    string              `json:"occurrence_key"`
	Offset           *ScheduleLinkOffset `json:"offset,omitempty"`
	NoLongerNeededAt string              `json:"no_longer_needed_at,omitempty"`
	CreatedAt        string              `json:"created_at"`
	UpdatedAt        string              `json:"updated_at"`
}

// ScheduleChangeExport retains journal rows independently of live schedules.
type ScheduleChangeExport struct {
	ID             string  `json:"id"`
	ItemID         string  `json:"item_id"`
	ScheduleID     string  `json:"schedule_id"`
	OccurrenceKey  *string `json:"occurrence_key,omitempty"`
	ChangeType     string  `json:"change_type"`
	Role           *string `json:"role,omitempty"`
	BeforeJSON     *string `json:"before_json,omitempty"`
	AfterJSON      *string `json:"after_json,omitempty"`
	Actor          string  `json:"actor"`
	Reason         string  `json:"reason"`
	ReasonSource   string  `json:"reason_source"`
	RootChangeID   string  `json:"root_change_id"`
	ParentChangeID *string `json:"parent_change_id,omitempty"`
	RecordedAt     string  `json:"recorded_at"`
}

// ScheduleReminderExport is the managed reminder's lifecycle.
type ScheduleReminderExport struct {
	RemindAt string `json:"remind_at"`
	FiredAt  string `json:"fired_at,omitempty"`
	AckedAt  string `json:"acked_at,omitempty"`
}

// ScheduleOccurrenceExport is one resolved occurrence. CommentID names a
// comment of the same bundle (CommentExport.ID); the import remaps it.
type ScheduleOccurrenceExport struct {
	ActualDoneAt   string `json:"actual_done_at,omitempty"`
	ActualSource   string `json:"actual_source,omitempty"`
	OccurrenceAt   string `json:"occurrence_at"`
	OccurrenceWall string `json:"occurrence_wall"`
	State          string `json:"state"`
	CommentID      string `json:"comment_id,omitempty"`
	Actor          string `json:"actor,omitempty"`
	ResolvedAt     string `json:"resolved_at"`
}

// ImportScheduleDrop is one bundled schedule an import did not store: its rule
// or state failed the checks every other schedule writer applies. The item
// imported; Item is its ref in the NEW workspace ("" when the item itself did
// not import). Reason is a stable code, never bundle text. Count is set only on
// an aggregate entry (links:bundle_limit, link_overrides:bundle_limit, and
// link_overrides:invalid or :duplicate past the individually listed rows): the
// number of records it stands for, which have no Item. 0 means one record.
type ImportScheduleDrop struct {
	Item   string `json:"item"`
	Reason string `json:"reason"`
	Count  int    `json:"count,omitempty"`
}

// VerificationExport is one acceptance-criterion verdict in a workspace
// bundle. Everything is carried VERBATIM, including the criterion hash, the
// spec and code revisions and created_at, so a verdict reads the same state
// after the move as before it: freshness is judged against the criterion's
// text, which moves with the item. The verifier's user id does not travel
// (user ids are per instance); created_by and agent_name do.
type VerificationExport struct {
	ItemID        string `json:"item_id"`
	CriterionID   string `json:"criterion"`
	CriterionHash string `json:"criterion_hash"`
	SpecSeq       int64  `json:"spec_seq"`
	CodeRevision  string `json:"code_revision,omitempty"`
	Verdict       string `json:"verdict"`
	Evidence      string `json:"evidence,omitempty"`
	EvidenceURL   string `json:"evidence_url,omitempty"`
	ViaItemID     string `json:"via_item_id,omitempty"`
	RunID         string `json:"run_id"`
	CreatedBy     string `json:"created_by,omitempty"`
	AgentName     string `json:"agent_name,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// ReminderExport is one item reminder in a workspace bundle.
//
// The LIFECYCLE MARKS ARE CARRIED, not reset. A fired-and-unacknowledged
// reminder is still owed to whoever armed it, so it arrives pending on the
// destination; an armed one whose instant has passed fires once on the first
// tick there, which is the same thing that would have happened had the
// workspace never moved. Re-arming everything on import would be inventing a
// new schedule the user did not set.
type ReminderExport struct {
	ItemID    string `json:"item_id"`
	RemindAt  string `json:"remind_at"`
	FiredAt   string `json:"fired_at,omitempty"`
	AckedAt   string `json:"acked_at,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// AttachmentManifestEntry describes one attachment blob in the
// tar-bundle export's attachments/manifest.json. The bundle layout is:
//
//	pad-export.json                     # the WorkspaceExport above
//	attachments/manifest.json           # uuid → AttachmentManifestEntry
//	attachments/<uuid>.<ext>            # the actual blob bytes
//
// Thumbnails are NOT bundled — they're re-derived on import via the
// existing thumbnail pipeline. ParentID and Variant therefore stay
// nil/empty for every entry shipped in a bundle, but the fields are
// kept here for forward compatibility (e.g. if a future format
// version starts shipping pre-derived variants).
type AttachmentManifestEntry struct {
	ID       string `json:"id"`       // attachment UUID (the original)
	Filename string `json:"filename"` // user-facing filename
	// FilenameSource is the exporter's provenance for Filename (BUG-2819).
	// Omitted by bundles that predate it. On import it is the BUNDLE's
	// claim, not an observation by the importing server.
	FilenameSource string `json:"filename_source,omitempty"`
	MIME           string `json:"mime"`         // canonical MIME from upload time
	SizeBytes      int64  `json:"size_bytes"`   // bytes on disk (matches the blob)
	ContentHash    string `json:"content_hash"` // sha256 hex, the dedupe key
	Width          *int   `json:"width,omitempty"`
	Height         *int   `json:"height,omitempty"`
	ItemID         string `json:"item_id,omitempty"` // exporter's item UUID; remapped on import
	ParentID       string `json:"parent_id,omitempty"`
	Variant        string `json:"variant,omitempty"`
	UploadedBy     string `json:"uploaded_by"`
	CreatedAt      string `json:"created_at"`
}

// AttachmentManifest is the top-level shape of attachments/manifest.json
// inside an export bundle. Wraps a list of entries plus a small
// "schema" version so the import path can validate / migrate.
type AttachmentManifest struct {
	Version int                       `json:"version"` // manifest version, 1
	Entries []AttachmentManifestEntry `json:"entries"`
}

// WorkspaceExportMeta holds workspace metadata for export.
type WorkspaceExportMeta struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Settings    string `json:"settings"`
}

// CollectionExport holds a collection's data for export.
type CollectionExport struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Icon        string `json:"icon"`
	Description string `json:"description"`
	Schema      string `json:"schema"`
	Settings    string `json:"settings"`
	// Traits carries the collection's kernel-trait declarations (SPEC-5).
	// omitempty so an archive from a deployment where nothing declares traits
	// omits the key entirely rather than carrying a noise "{}" on every
	// collection, and so an import of an older archive is unambiguous (absent,
	// not empty).
	//
	// This does NOT make a pre-TASK-2657 archive round-trip byte-identically:
	// import defaults a missing value to "{}" in the column, and any collection
	// the migration then backfills re-exports WITH declarations. Round-tripping
	// an old archive through a current deployment is expected to gain traits —
	// that is the backfill working, not export drift.
	Traits string `json:"traits,omitempty"`
	Prefix string `json:"prefix"`
	// Source is how the collection was created ('web' | 'cli' | 'mcp'),
	// BUG-3447. omitempty: an older bundle has none, and imports it as ''
	// (provenance unknown). A value outside that set imports as '' too.
	Source    string `json:"source,omitempty"`
	SortOrder int    `json:"sort_order"`
	IsDefault bool   `json:"is_default"`
	IsSystem  bool   `json:"is_system"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// DeletedAt carries a collection's soft-delete mark, empty for a live
	// collection (BUG-2884).
	//
	// The bundle used to skip soft-deleted collections while exporting every
	// live ITEM, and DeleteCollection soft-deletes only the collection row —
	// its items keep deleted_at IS NULL and stay reachable by ref, by id, and
	// through search, because no item-bearing read joins collection liveness.
	// So the two sections were filtered by different rules and the bundle
	// named a collection it did not carry; ImportWorkspace then dropped those
	// items on its orphan gate, silently. Since pad db migrate is
	// ExportWorkspace piped into ImportWorkspace, that was live data lost on a
	// SQLite→Postgres migration, not just a lossy backup.
	//
	// Filtering the items to live collections instead would have made the
	// migration DELETE reachable rows, so the bundle carries the archived
	// collection and the importer reproduces the archive.
	//
	// omitempty for the same reason as Traits: an archive from a workspace
	// with nothing deleted omits the key rather than carrying a noise "" on
	// every collection. Absent decodes to "" and MUST mean live — that is what
	// keeps every archive written before this field importable.
	DeletedAt string `json:"deleted_at,omitempty"`
}

// ItemExport holds an item's data for export.
type ItemExport struct {
	// DeletedAt is set only for soft-deleted blockers carried by version 4.
	DeletedAt    string `json:"deleted_at,omitempty"`
	ID           string `json:"id"`
	CollectionID string `json:"collection_id"`
	Title        string `json:"title"`
	Slug         string `json:"slug"`
	Content      string `json:"content"`
	// ContentState records that CONTENT above was behind the item's live
	// collaborative document when the bundle was written (BUG-3032 / BUG-3000).
	// Same values as models.Item.ContentState, named the same, and empty for
	// every item whose row is current — so a bundle from a workspace with
	// nothing pending is byte-identical to one produced before this field
	// existed.
	//
	// Additive with NO WorkspaceExport.Version bump, deliberately: no bundle
	// decode path sets DisallowUnknownFields, so an older binary ignores this
	// key. ImportWorkspace accepts supported workspace bundle versions; optional
	// content-state metadata does not change their meaning.
	//
	// Why the bundle needs its own marker rather than inheriting one: this is
	// not a late read. WorkspaceExport carries no op-log, and ImportWorkspace
	// writes Content back as the destination's canonical content, so on
	// `pad db migrate-to-pg` — export piped into import across two databases,
	// after which the source is abandoned — an unflushed body becomes the only
	// copy. The marker is what lets that door refuse; it is not a repair.
	ContentState string `json:"content_state,omitempty"`
	// CollabSetAside carries the edits a collab schema-version rebuild set
	// aside on this item (BUG-3244), which no body holds. Unlike the op-log,
	// which a bundle never carries, these travel: they can only be recovered
	// by a decoder, not by a tab, so a copy of the rows is the whole of what
	// can be kept. Additive and omitempty, on the same no-version-bump grounds
	// as ContentState.
	CollabSetAside []YjsSetAsideExport `json:"collab_set_aside,omitempty"`
	// BuiltinOrigin is the built-in convention or playbook this item was made
	// from (TASK-3462), so a restored workspace can still be offered updates
	// to Pad's text. Additive and omitempty on the same grounds as
	// CollabSetAside; a content hash means the same thing on any instance.
	BuiltinOrigin  *BuiltinOrigin `json:"builtin_origin,omitempty"`
	Fields         string         `json:"fields"`
	Tags           string         `json:"tags"`
	Pinned         bool           `json:"pinned"`
	SortOrder      int            `json:"sort_order"`
	ParentID       string         `json:"parent_id,omitempty"`
	CreatedBy      string         `json:"created_by"`
	LastModifiedBy string         `json:"last_modified_by"`
	Source         string         `json:"source"`
	ItemNumber     int            `json:"item_number"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
}

// YjsSetAsideExport is one set-aside edit row in a bundle (BUG-3244). The item
// it belongs to is the enclosing ItemExport; update_data is base64 in JSON.
type YjsSetAsideExport struct {
	OpLogID       int64  `json:"op_log_id"`
	SchemaVersion string `json:"schema_version"`
	CreatedAt     string `json:"created_at"`
	SetAsideAt    string `json:"set_aside_at"`
	UpdateData    []byte `json:"update_data"`
}

// CommentExport holds a comment's data for export.
type CommentExport struct {
	ID        string `json:"id"`
	ItemID    string `json:"item_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedBy string `json:"created_by"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ItemLinkExport holds an item link's data for export.
type ItemLinkExport struct {
	ID        string `json:"id"`
	SourceID  string `json:"source_id"`
	TargetID  string `json:"target_id"`
	LinkType  string `json:"link_type"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// ItemVersionExport holds an item version's data for export.
type ItemVersionExport struct {
	ID            string `json:"id"`
	ItemID        string `json:"item_id"`
	Content       string `json:"content"`
	ChangeSummary string `json:"change_summary"`
	CreatedBy     string `json:"created_by"`
	Source        string `json:"source"`
	IsDiff        bool   `json:"is_diff"`
	CreatedAt     string `json:"created_at"`
	// PLAN-2348 U2. The line counts and the create marker travel; user_id does
	// not, because an id from another instance names nobody on this one.
	// Additive and omitempty: a bundle from before them imports as unknown.
	LinesAdded   *int `json:"lines_added,omitempty"`
	LinesRemoved *int `json:"lines_removed,omitempty"`
	IsCreate     bool `json:"is_create,omitempty"`
	// BUG-3820: the hex sha256 of the body the row stands for. Travels so an
	// imported chain stays verifiable; a bundle without it imports as NULL.
	BodySHA256 string `json:"body_sha256,omitempty"`
}
