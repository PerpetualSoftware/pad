package models

// Reminder is a one-shot, fire-at-an-instant signal attached to an item
// (IDEA-2641, GitHub #1010).
//
// It is deliberately NOT a schema field. See migration 085 for why the
// annotation-on-a-FieldDef shape was overturned; the short form is that any
// key added to FieldDef is silently dropped both by the web collection editor
// (which rebuilds each field key-by-key from an allowlist) and by any Go
// unmarshal+marshal round-trip through CollectionSchema, which has fixed
// fields. A reminder also has a LIFECYCLE that a field definition has nowhere
// to keep.
//
// The lifecycle is three states, and they are three states rather than two
// because acking must not re-arm:
//
//	ARMED         fired_at IS NULL                     — a tick may fire it
//	FIRED-UNACKED fired_at set, acked_at NULL           — on the poll surface
//	FIRED-ACKED   both set                              — history
//
// Re-arming (moving RemindAt on a fired row) returns it to ARMED by clearing
// both marks.
type Reminder struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ItemID      string `json:"item_id"`

	// RemindAt is an RFC3339 instant in UTC, always. It is not a `date`
	// schema value: those admit both YYYY-MM-DD and RFC3339 and are compared
	// against the server's LOCAL calendar day, an ambiguity a fire-at time
	// cannot carry.
	RemindAt string `json:"remind_at"`

	// FiredAt is nil while armed. Non-nil means the scheduler emitted this
	// reminder's event; it is never cleared except by an explicit re-arm.
	FiredAt *string `json:"fired_at,omitempty"`

	// AckedAt is nil until a caller explicitly acknowledges. NOTHING else
	// acks — in particular an item reaching a terminal status does not, which
	// would both couple every status write to reminder state and silently
	// consume a reminder set to fire after the work was done. The poll
	// surface filters terminal-item reminders out of its listing instead,
	// leaving the row untouched.
	AckedAt *string `json:"acked_at,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Armed reports whether a tick would still consider this reminder.
func (r *Reminder) Armed() bool { return r.FiredAt == nil }

// PendingAck reports whether this reminder has fired and not been
// acknowledged — the set the agent poll surface reads.
func (r *Reminder) PendingAck() bool { return r.FiredAt != nil && r.AckedAt == nil }

// ReminderCreate is the input shape for arming a reminder. RemindAt is
// required and must parse as RFC3339; the handler normalizes it to UTC before
// it reaches the store, so the store never has to reason about zones.
type ReminderCreate struct {
	ItemID   string `json:"item_id"`
	RemindAt string `json:"remind_at"`
}

// ReminderUpdate carries a re-arm. RemindAt is the only mutable field: a
// reminder has no other content to change, and making the ONE mutation that
// exists also the one that clears the fire marks keeps the re-arm rule
// ("changing remind_at on a fired row re-arms it") impossible to apply
// half-way.
type ReminderUpdate struct {
	RemindAt string `json:"remind_at"`
}

// PendingReminder is a fired-and-unacked reminder joined to the item it is
// about — what the poll surface renders. The item fields are carried here
// rather than fetched per-row because the surface's whole job is to be one
// cheap query an agent runs often.
type PendingReminder struct {
	Reminder

	ItemRef        string `json:"item_ref"`
	ItemTitle      string `json:"item_title"`
	ItemSlug       string `json:"item_slug"`
	CollectionSlug string `json:"collection_slug"`

	// ItemFields and CollectionID exist for the caller's terminal-status
	// filter and are not part of the wire shape. Terminality is defined by a
	// collection's schema, so the filter cannot run in SQL; it runs where the
	// dashboard already builds that context. json:"-" because a pending
	// reminder is a notification, not a second way to read an item's fields.
	ItemFields   string `json:"-"`
	CollectionID string `json:"-"`
}
