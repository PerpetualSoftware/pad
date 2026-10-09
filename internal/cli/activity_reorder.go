package cli

import (
	"encoding/json"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// DisplayActivity is an activity row as the text outputs show it. ReorderCount
// is set when the row stands for a whole reorder batch (TASK-3517).
type DisplayActivity struct {
	models.Activity
	ReorderCount int
}

// CollapseReorderBatches folds each run of consecutive "reordered" rows that
// share a reorder_batch into its first row, counting the run (TASK-3517). A
// reorder writes one row per moved item so the server's per-row visibility
// filter stays correct; a twenty-card drag would otherwise be twenty lines in
// `pad project activity`, which agents read. The count is the rows the
// caller received, so it never counts items they cannot see. JSON output does
// not use this: it stays one object per row.
func CollapseReorderBatches(rows []models.Activity) []DisplayActivity {
	out := make([]DisplayActivity, 0, len(rows))
	runBatch := ""
	for _, a := range rows {
		batch := reorderBatch(a)
		if batch != "" && batch == runBatch && len(out) > 0 {
			last := &out[len(out)-1]
			if last.ReorderCount == 0 {
				last.ReorderCount = 1
			}
			last.ReorderCount++
			continue
		}
		runBatch = batch
		out = append(out, DisplayActivity{Activity: a})
	}
	return out
}

func reorderBatch(a models.Activity) string {
	if a.Action != "reordered" || a.Metadata == "" {
		return ""
	}
	var meta struct {
		Batch string `json:"reorder_batch"`
	}
	if json.Unmarshal([]byte(a.Metadata), &meta) != nil {
		return ""
	}
	return meta.Batch
}
