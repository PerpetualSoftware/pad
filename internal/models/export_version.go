package models

import "encoding/json"

// UnmarshalJSON retains versioned member presence, including explicit null
// and empty values, so older bundles cannot silently discard commitment data.
func (x *WorkspaceExport) UnmarshalJSON(b []byte) error {
	type plain WorkspaceExport
	var decoded plain
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*x = WorkspaceExport(decoded)
	var raw struct {
		Changes json.RawMessage `json:"schedule_changes"`
		Links   json.RawMessage `json:"schedule_links"`
		Gates   json.RawMessage `json:"schedule_gates"`
		// Only the presence of deleted_at: a map here would hold every body twice.
		Items []struct {
			DeletedAt json.RawMessage `json:"deleted_at"`
		} `json:"items"`
		Schedules []map[string]json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	x.hasV2Members = raw.Changes != nil
	x.hasV3Members = raw.Links != nil
	x.hasV4Members = raw.Gates != nil
	for _, it := range raw.Items {
		if it.DeletedAt != nil {
			x.hasV4Members = true
		}
	}
	for _, sc := range raw.Schedules {
		for _, key := range []string{"id", "roles", "overrides", "forecast", "forecast_events", "children", "completion_evidence"} {
			if _, ok := sc[key]; ok {
				x.hasV2Members = true
			}
		}
		var occs []map[string]json.RawMessage
		if err := json.Unmarshal(sc["occurrences"], &occs); len(sc["occurrences"]) != 0 && err != nil {
			return err
		}
		for _, o := range occs {
			for _, key := range []string{"actual_done_at", "actual_source"} {
				if _, ok := o[key]; ok {
					x.hasV2Members = true
				}
			}
		}
	}
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
			if o.ActualDoneAt != "" || o.ActualSource != "" {
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
