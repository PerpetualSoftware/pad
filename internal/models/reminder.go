package models

// Reminder is the delivery state of a commitment alarm. Armed rows have no
// fired_at, pending rows have fired_at but no acked_at, and acknowledged rows
// retain both. Explicit rearm clears both marks while retaining the alarm ID.
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

	// AckedAt records acknowledgement without completing the outstanding work.
	// Terminal items pause firing; reopening resumes their commitment.
	AckedAt *string `json:"acked_at,omitempty"`

	// ScheduleID identifies the commitment. OccurrenceAt is its outstanding
	// instant. Pointers retain decoding compatibility with historical events.
	ScheduleID   *string `json:"schedule_id,omitempty"`
	OccurrenceAt *string `json:"occurrence_at,omitempty"`

	// FireStamp is a nanosecond UTC stamp taken once in the transaction that
	// wins a fire. It is not a column: it is set on the fired snapshot only,
	// so it travels in the item.reminder_due payload, and the notification
	// dedupe key is built from it. A second fire inside the same second (a
	// re-armed row, a snooze) therefore has its own key, and a redelivery of
	// THIS fire re-derives the same one (TASK-4103).
	FireStamp *string `json:"fire_stamp,omitempty"`

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
