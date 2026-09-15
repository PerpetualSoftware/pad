package main

import (
	"regexp"
	"strings"
	"testing"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
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

func TestAgentDispatcherGuideTopicsResolve(t *testing.T) {
	dispatcher := cli.FormatForTool(*cli.ResolveTool("agents"), pad.PadSkill)
	guide := string(cli.StripFrontmatter(pad.PadSkill))
	matches := regexp.MustCompile(`pad agent guide ([a-z0-9][a-z0-9-]*)`).FindAllSubmatch(dispatcher, -1)
	seen := map[string]bool{}
	for _, match := range matches {
		topic := string(match[1])
		if topic == "all" || seen[topic] {
			continue
		}
		seen[topic] = true
		if _, ok := agentGuideSection(guide, topic); !ok {
			t.Errorf("dispatcher guide topic %q does not resolve", topic)
		}
	}
	if len(seen) == 0 {
		t.Fatal("dispatcher contains no guide topics")
	}
}
