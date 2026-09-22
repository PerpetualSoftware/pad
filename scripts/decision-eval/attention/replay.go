package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Replay (TASK-3137 checkpoint 3): rebuild an item as it stood at a moment it
// was OPEN, so the 70/70 labels — which describe the need, not the
// resolution — are compared with the production question asked at a time
// production would actually ask it.

type version struct {
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type activity struct {
	Action    string    `json:"action"`
	CreatedAt time.Time `json:"created_at"`
	Metadata  string    `json:"metadata"`
}

type transition struct {
	At       time.Time
	From, To string
}

// terminal is each collection's terminal_options, read from the workspace
// schema on 2026-09-22 (human-tasks: done; tasks: done, cancelled).
var terminal = map[string]map[string]bool{
	"human-tasks": {"done": true},
	"tasks":       {"done": true, "cancelled": true},
}

func statusTransitions(acts []activity) []transition {
	var out []transition
	for _, a := range acts {
		var md struct {
			Changes string `json:"changes"`
		}
		_ = json.Unmarshal([]byte(a.Metadata), &md)
		for _, part := range strings.Split(md.Changes, "; ") {
			rest, ok := strings.CutPrefix(part, "status: ")
			if !ok {
				continue
			}
			from, to, ok := strings.Cut(rest, " → ")
			if !ok {
				continue
			}
			out = append(out, transition{At: a.CreatedAt, From: strings.TrimSpace(from), To: strings.TrimSpace(to)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// replayed returns the item and comments as they stood at the replay point,
// or excluded=true with a reason.
func replayed(dump, mode string, item models.Item, comments []models.Comment) (models.Item, []models.Comment, string) {
	var versions []version
	var acts []activity
	if err := readJSON(filepath.Join(dump, "items", item.Ref+".history.json"), &versions); err != nil {
		return item, nil, "history: " + err.Error()
	}
	if err := readJSON(filepath.Join(dump, "items", item.Ref+".activity.json"), &acts); err != nil {
		return item, nil, "activity: " + err.Error()
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].CreatedAt.Before(versions[j].CreatedAt) })
	trans := statusTransitions(acts)
	term := terminal[item.CollectionSlug]
	cur := statusOf(item.Fields)

	setStatus := func(it models.Item, s string) models.Item {
		var f map[string]any
		_ = json.Unmarshal([]byte(it.Fields), &f)
		if f == nil {
			f = map[string]any{}
		}
		f["status"] = s
		b, _ := json.Marshal(f)
		it.Fields = string(b)
		return it
	}

	switch mode {
	case "r0":
		if len(versions) > 0 {
			item.Content = versions[0].Content
		}
		initial := cur
		if len(trans) > 0 {
			initial = trans[0].From
		}
		if term[initial] {
			return item, nil, "created terminal"
		}
		return setStatus(item, initial), nil, ""
	case "r1":
		var t1 *transition
		for i := range trans {
			if term[trans[i].To] && !term[trans[i].From] {
				t1 = &trans[i]
				break
			}
		}
		if t1 == nil {
			if term[cur] {
				return item, nil, "closed, no logged terminal transition"
			}
			return item, comments, "" // still open: its current state
		}
		content := ""
		found := false
		for _, v := range versions {
			if v.CreatedAt.Before(t1.At) {
				content, found = v.Content, true
			}
		}
		if !found && len(versions) > 0 {
			content = versions[0].Content
		}
		item.Content = content
		var before []models.Comment
		for _, c := range comments {
			if c.CreatedAt.Before(t1.At) {
				before = append(before, c)
			}
		}
		return setStatus(item, t1.From), before, ""
	}
	return item, comments, "unknown replay mode"
}

// replayDry is the replay's instrument check: exclusions by class, the
// replayed status distribution (none may be terminal), trail sizes, and a
// sample line per item for eyeballing.
func replayDry(dump, mode string) {
	var pop []member
	mustRead(filepath.Join(dump, "population.json"), &pop)
	exPos, exNeg, keptPos, keptNeg, terminalLeak := 0, 0, 0, 0, 0
	statuses := map[string]int{}
	reasons := map[string]int{}
	for i, m := range pop {
		var item models.Item
		var comments []models.Comment
		mustRead(filepath.Join(dump, "items", m.Ref+".json"), &item)
		mustRead(filepath.Join(dump, "items", m.Ref+".comments.json"), &comments)
		sort.Slice(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
		it, cs, ex := replayed(dump, mode, item, comments)
		if ex != "" {
			reasons[ex]++
			if m.Truth {
				exPos++
			} else {
				exNeg++
			}
			continue
		}
		if m.Truth {
			keptPos++
		} else {
			keptNeg++
		}
		s := statusOf(it.Fields)
		statuses[item.CollectionSlug+":"+s]++
		if terminal[item.CollectionSlug][s] {
			terminalLeak++
		}
		if i%20 == 0 {
			println(m.Ref, "now:", statusOf(item.Fields), "replayed:", s, "body:", len(it.Content), "of", len(item.Content), "trail:", len(cs), "of", len(comments))
		}
	}
	println("kept pos", keptPos, "neg", keptNeg, "| excluded pos", exPos, "neg", exNeg, "| terminal leaks", terminalLeak)
	for k, v := range reasons {
		println("  excluded:", k, v)
	}
	for k, v := range statuses {
		println("  replayed status", k, v)
	}
}
