package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2696 skew: a server older than this CLI ignores the typed github_pr
// member and answers 200 having written nothing. The CLI must not report that
// as a successful link or unlink.
func TestVerifyPRLinkedCatchesAServerThatIgnoredTheWrite(t *testing.T) {
	linked := &models.Item{CodeContext: &models.ItemCodeContext{PullRequest: &models.ItemPullRequestMetadata{Number: 41}}}
	if err := verifyPRLinked(linked, 41); err != nil {
		t.Fatalf("a landed link was refused: %v", err)
	}
	for name, item := range map[string]*models.Item{
		"no code context (old server wrote nothing)": {},
		"a different PR still linked":                {CodeContext: &models.ItemCodeContext{PullRequest: &models.ItemPullRequestMetadata{Number: 9}}},
		"nil response":                               nil,
	} {
		err := verifyPRLinked(item, 41)
		if err == nil || !strings.Contains(err.Error(), "older than this CLI") {
			t.Errorf("%s: want the older-server error, got %v", name, err)
		}
	}
}

func TestVerifyPRUnlinkedCatchesAServerThatIgnoredTheClear(t *testing.T) {
	if err := verifyPRUnlinked(&models.Item{}); err != nil {
		t.Fatalf("a landed unlink was refused: %v", err)
	}
	stillLinked := &models.Item{CodeContext: &models.ItemCodeContext{PullRequest: &models.ItemPullRequestMetadata{Number: 41}}}
	if err := verifyPRUnlinked(stillLinked); err == nil || !strings.Contains(err.Error(), "older than this CLI") {
		t.Fatalf("want the older-server error, got %v", err)
	}
}
