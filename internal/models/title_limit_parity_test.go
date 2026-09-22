package models

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// The Go half of the item-title limit parity harness (BUG-3115).
//
// The web checks a typed title against the server's limit before sending it
// (web/src/lib/items/titleLimit.ts), so the text never leaves the input for
// the common case. That check is only right while it agrees with
// ValidateItemTitle on the limit, on what "character" means (runes, not
// UTF-16 units) and on what is trimmed first (unicode.IsSpace, which is not
// the set JS's String.trim uses). Both halves assert against the SAME corpus,
// testdata/item_title_limit.json, rather than against each other.

const titleLimitCorpusRelPath = "../../testdata/item_title_limit.json"

type titleLimitCase struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Unit   string `json:"unit"`
	Count  int    `json:"count"`
	Suffix string `json:"suffix"`
	Runes  int    `json:"runes"`
	OK     bool   `json:"ok"`
}

type titleLimitCorpus struct {
	MaxRunes int              `json:"max_runes"`
	GoSpace  []rune           `json:"go_space"`
	Cases    []titleLimitCase `json:"cases"`
}

func loadTitleLimitCorpus(t *testing.T) titleLimitCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(titleLimitCorpusRelPath))
	if err != nil {
		t.Fatalf("read shared title-limit corpus: %v", err)
	}
	var c titleLimitCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse shared title-limit corpus: %v", err)
	}
	// A truncated corpus would make the loops below vacuous.
	if len(c.Cases) < 8 || len(c.GoSpace) < 20 {
		t.Fatalf("title-limit corpus looks truncated: %d cases, %d spaces", len(c.Cases), len(c.GoSpace))
	}
	return c
}

func TestTitleLimitCorpus_MaxMatchesServer(t *testing.T) {
	c := loadTitleLimitCorpus(t)
	if c.MaxRunes != MaxItemTitleRunes {
		t.Fatalf("corpus max_runes = %d, MaxItemTitleRunes = %d — update the corpus AND web/src/lib/items/titleLimit.ts", c.MaxRunes, MaxItemTitleRunes)
	}
}

// The web trims by the corpus's go_space list; this proves that list is
// exactly unicode.IsSpace, over the whole code space.
func TestTitleLimitCorpus_GoSpaceIsExactlyUnicodeIsSpace(t *testing.T) {
	c := loadTitleLimitCorpus(t)
	listed := make(map[rune]bool, len(c.GoSpace))
	for _, r := range c.GoSpace {
		listed[r] = true
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) != listed[r] {
			t.Errorf("U+%04X: unicode.IsSpace=%v, listed in go_space=%v", r, unicode.IsSpace(r), listed[r])
		}
	}
}

func TestTitleLimitCorpus_ServerVerdicts(t *testing.T) {
	c := loadTitleLimitCorpus(t)
	for _, tc := range c.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			title := tc.Prefix + strings.Repeat(tc.Unit, tc.Count) + tc.Suffix
			got := ValidateItemTitle(NormalizeItemTitle(title))
			want := ""
			if !tc.OK {
				want = fmt.Sprintf("Title is too long: %d characters, maximum %d", tc.Runes, MaxItemTitleRunes)
			}
			if got != want {
				t.Fatalf("ValidateItemTitle = %q, corpus expects %q", got, want)
			}
		})
	}
}
