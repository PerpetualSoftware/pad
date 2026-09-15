package main

import (
	"strings"
	"testing"
)

const guideFixture = `# Pad

## Context Loading

Start here.

### Why one call

It is cheaper.

## Role Awareness

Pick a role.
`

func TestAgentGuideTopics(t *testing.T) {
	got := strings.Join(agentGuideTopics(guideFixture), ",")
	if got != "context-loading,why-one-call,role-awareness" {
		t.Fatalf("topics = %q", got)
	}
}

func TestAgentGuideSectionIncludesChildrenOnly(t *testing.T) {
	got, ok := agentGuideSection(guideFixture, "Context Loading")
	if !ok {
		t.Fatal("context-loading topic not found")
	}
	if !strings.Contains(got, "### Why one call") {
		t.Errorf("child section missing: %q", got)
	}
	if strings.Contains(got, "Role Awareness") {
		t.Errorf("next peer section leaked into result: %q", got)
	}
}

func TestAgentGuideSectionRejectsUnknownTopic(t *testing.T) {
	if _, ok := agentGuideSection(guideFixture, "missing"); ok {
		t.Fatal("unknown topic resolved")
	}
}
