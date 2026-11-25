package models

import (
	"bytes"
	"encoding/json"
)

// UnmarshalJSON retains versioned member presence, including explicit null
// and empty values, so older bundles cannot silently discard commitment data.
func (x *WorkspaceExport) UnmarshalJSON(b []byte) error {
	decoded, err := DecodeWorkspaceExport(bytes.NewReader(b), nil)
	if err != nil {
		return err
	}
	*x = *decoded
	return nil
}

// HasV2ScheduleMembers also covers bundles built directly by Go callers.
func (x *WorkspaceExport) HasV2ScheduleMembers() bool {
	if x.hasV2Members || x.ScheduleChanges != nil {
		return true
	}
	for _, sc := range x.Schedules {
		if sc.ID != "" || sc.Roles != nil || sc.Overrides != nil ||
			sc.Forecast != nil || sc.ForecastEvents != nil || sc.Children != nil || sc.CompletionEvidence != nil {
			return true
		}
		for _, o := range sc.Occurrences {
			if o.ActualDoneAt != "" || o.ActualSource != "" || o.Roles != nil {
				return true
			}
		}
	}
	return false
}

// HasV3ScheduleMembers reports whether the bundle carries time links, a member
// of version 3. Like the version 2 check it sees an explicit null or empty
// value, and bundles built directly by Go callers.
func (x *WorkspaceExport) HasV3ScheduleMembers() bool {
	return x.hasV3Members || x.ScheduleLinks != nil
}

// HasV4ScheduleMembers sees gates and item tombstones, including explicit null
// and empty JSON members and bundles built directly by Go callers.
func (x *WorkspaceExport) HasV4ScheduleMembers() bool {
	if x.hasV4Members || x.ScheduleGates != nil {
		return true
	}
	for _, it := range x.Items {
		if it.DeletedAt != "" {
			return true
		}
	}
	return false
}

// UnmarshalJSON dismisses historical endings from bundles that predate the
// dismissal key. An explicitly empty or null value keeps the ending visible.
func (x *ScheduleExport) UnmarshalJSON(b []byte) error {
	type plain ScheduleExport
	var decoded plain
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	var raw struct {
		Dismissed json.RawMessage `json:"ended_dismissed_at"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw.Dismissed == nil {
		decoded.EndedDismissedAt = decoded.EndedAt
	}
	*x = ScheduleExport(decoded)
	return nil
}
