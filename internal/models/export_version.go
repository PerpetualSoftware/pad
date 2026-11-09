package models

import "encoding/json"

// UnmarshalJSON retains v2 member presence, including explicit null and empty
// values, so a version 1 bundle cannot silently discard commitment data.
func (x *WorkspaceExport) UnmarshalJSON(b []byte) error {
	type plain WorkspaceExport
	var decoded plain
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*x = WorkspaceExport(decoded)
	var raw struct {
		Changes   json.RawMessage              `json:"schedule_changes"`
		Schedules []map[string]json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	x.hasV2Members = raw.Changes != nil
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
