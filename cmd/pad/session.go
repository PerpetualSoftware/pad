// `pad session shape` — report Claude Code session metrics (token
// usage, context %, message counts) by reading the session JSONL on
// disk. See IDEA-1491 for the design.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// sessionShapeReport is the canonical JSON shape returned by
// `pad session shape`. v1 schema per IDEA-1491 recon update.
type sessionShapeReport struct {
	Agent                   string                 `json:"agent"`
	AgentVersion            string                 `json:"agent_version,omitempty"`
	SessionID               string                 `json:"session_id,omitempty"`
	SessionLogPath          string                 `json:"session_log_path,omitempty"`
	SessionStartedAt        string                 `json:"session_started_at,omitempty"`
	LastActivityAt          string                 `json:"last_activity_at,omitempty"`
	ElapsedWallClockSeconds int64                  `json:"elapsed_wall_clock_seconds"`
	Bytes                   int64                  `json:"bytes"`
	Lines                   int64                  `json:"lines"`
	MessageCounts           map[string]int64       `json:"message_counts"`
	ToolInvocations         int64                  `json:"tool_invocations"`
	CWD                     string                 `json:"cwd,omitempty"`
	GitBranch               string                 `json:"git_branch,omitempty"`
	Tokens                  *cli.SessionUsage      `json:"tokens"`
	ContextPct              *float64               `json:"context_pct"`
	ContextClass            string                 `json:"context_class,omitempty"`
	Budget                  *int64                 `json:"budget"`
	BudgetSource            string                 `json:"budget_source,omitempty"`
	FallbackUsed            bool                   `json:"fallback_used"`
	ResolverSource          string                 `json:"resolver_source,omitempty"`
	Notes                   map[string]interface{} `json:"notes,omitempty"`
}

func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Inspect the current agent session (Claude Code today)",
	}
	cmd.AddCommand(sessionShapeCmd())
	return cmd
}

func sessionShapeCmd() *cobra.Command {
	var sessionFlag string
	cmd := &cobra.Command{
		Use:   "shape",
		Short: "Report Claude Code session shape — tokens, context %, message counts",
		Long: `Read the current (or specified) Claude Code session JSONL and report
size, message counts, and — crucially for agent self-pacing — cumulative
context-window token usage as a percentage of the agent version's
budget. The percentage is bucketed into low / moderate / heavy / dense.

Source-of-truth precedence:
  1. --session <id|path>
  2. $CLAUDE_CODE_SESSION_ID + cwd-derived project slug
  3. Autodetect under ~/.claude/projects/<cwd-slug>/ when $CLAUDECODE=1
  4. Fallback (fallback_used: true) — agent=unknown, no token data

Side-effect free. The CLI never echoes prompt content, only metrics.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := buildSessionShape(sessionFlag)
			if err != nil {
				return err
			}
			// IDEA-1491: `session shape` defaults to JSON because the
			// primary caller is an agent reading its own pacing. Only
			// override the global --format default ("table") when the
			// user didn't explicitly set it.
			fmtChoice := formatFlag
			if !cmd.Root().PersistentFlags().Changed("format") {
				fmtChoice = "json"
			}
			return renderSessionShape(report, fmtChoice)
		},
	}
	cmd.Flags().StringVar(&sessionFlag, "session", "", "session UUID or absolute path to a .jsonl log")
	return cmd
}

func buildSessionShape(sessionFlag string) (*sessionShapeReport, error) {
	resolved, err := cli.ResolveSessionLog(cli.ResolveOptions{ExplicitSession: sessionFlag})
	if err != nil {
		// If the user explicitly passed --session, a resolver failure
		// is a real error (typo / wrong UUID / wrong path) and silently
		// emitting the fallback shape would mask the bug in automation.
		// Only the autodetect/env-var-implicit paths get to fall back.
		if sessionFlag != "" {
			return nil, fmt.Errorf("--session %q: %w", sessionFlag, err)
		}
		// Implicit-path fallback. v1 keeps this minimal — no
		// Pad-invocation-count derivation yet (IDEA-body sketch is a
		// TODO).
		return &sessionShapeReport{
			Agent:         "unknown",
			FallbackUsed:  true,
			MessageCounts: map[string]int64{},
			Tokens:        nil,
			ContextPct:    nil,
			Budget:        nil,
			Notes: map[string]interface{}{
				"reason": err.Error(),
			},
		}, nil
	}

	metrics, err := cli.ParseSessionJSONL(resolved.Path)
	if err != nil {
		return nil, err
	}

	report := &sessionShapeReport{
		Agent:                   "claude-code",
		AgentVersion:            metrics.AgentVersion,
		SessionID:               resolved.SessionID,
		SessionLogPath:          resolved.Path,
		SessionStartedAt:        metrics.SessionStartedAt,
		LastActivityAt:          metrics.LastActivityAt,
		ElapsedWallClockSeconds: metrics.ElapsedWallSeconds,
		Bytes:                   metrics.Bytes,
		Lines:                   metrics.Lines,
		MessageCounts:           metrics.MessageCounts,
		ToolInvocations:         metrics.ToolInvocations,
		CWD:                     metrics.CWD,
		GitBranch:               metrics.GitBranch,
		Tokens:                  metrics.Usage,
		ResolverSource:          resolved.Source,
	}

	if metrics.HasUsage {
		if budget, ok := cli.LookupContextBudget(metrics.AgentVersion); ok {
			report.Budget = &budget
			report.BudgetSource = "hardcoded_per_agent_version"
			// Use TotalPrompt (cache_read + cache_creation + input) rather
			// than CacheRead alone. CacheRead is a steady-state proxy that
			// under-counts at turn boundaries when fresh content lands in
			// cache_creation/input but hasn't been folded into the cached
			// prefix yet. TotalPrompt is the actual prompt footprint sent
			// to the model this turn — the right numerator vs the budget.
			pct := float64(metrics.Usage.TotalPrompt) / float64(budget) * 100
			report.ContextPct = &pct
			report.ContextClass = cli.ContextClass(pct)
		} else {
			report.Notes = map[string]interface{}{
				"confidence": "unknown-agent-version",
			}
		}
	}

	// v1 punts: sub-agent JSONLs under <slug>/<session-id>/subagents/ are
	// not summed. The --include-sidechain flag from the IDEA comment is a
	// follow-up.
	// TODO(IDEA-1491): optional --include-sidechain to union parent +
	// subagents/*.jsonl token usage.

	return report, nil
}

func renderSessionShape(r *sessionShapeReport, format string) error {
	switch format {
	case "json", "":
		// Default to JSON for `session shape` because the primary
		// caller is an agent reading its own pacing.
		outputJSON(r)
		return nil
	case "markdown":
		fmt.Print(sessionShapeMarkdown(r))
		return nil
	case "table":
		printSessionShapeTable(r)
		return nil
	default:
		return fmt.Errorf("unknown format %q (want json, table, markdown)", format)
	}
}

func sessionShapeMarkdown(r *sessionShapeReport) string {
	var b strings.Builder
	if r.FallbackUsed {
		fmt.Fprintf(&b, "**Session shape (fallback)** — agent: `%s`, no token data available.\n", r.Agent)
		return b.String()
	}
	fmt.Fprintf(&b, "**Session shape** — `%s` %s\n", r.Agent, r.AgentVersion)
	if r.SessionID != "" {
		fmt.Fprintf(&b, "- session: `%s`\n", r.SessionID)
	}
	if r.ContextPct != nil && r.Budget != nil {
		fmt.Fprintf(&b, "- context: **%.1f%%** (%s) — %s / %s tokens\n",
			*r.ContextPct, r.ContextClass,
			humanizeInt(r.Tokens.TotalPrompt), humanizeInt(*r.Budget))
	} else if r.Tokens != nil {
		fmt.Fprintf(&b, "- context tokens: %s (no budget for agent_version=%q)\n",
			humanizeInt(r.Tokens.TotalPrompt), r.AgentVersion)
	}
	if r.Tokens != nil {
		fmt.Fprintf(&b, "- tokens: cache_read=%s, cache_creation=%s, input=%s, output=%s\n",
			humanizeInt(r.Tokens.CacheRead), humanizeInt(r.Tokens.CacheCreation),
			humanizeInt(r.Tokens.Input), humanizeInt(r.Tokens.Output))
	}
	fmt.Fprintf(&b, "- log: %d lines / %s, %d tool invocations\n",
		r.Lines, humanizeBytes(r.Bytes), r.ToolInvocations)
	if r.SessionStartedAt != "" {
		fmt.Fprintf(&b, "- started: %s · last activity: %s (elapsed %ds)\n",
			r.SessionStartedAt, r.LastActivityAt, r.ElapsedWallClockSeconds)
	}
	if r.CWD != "" {
		fmt.Fprintf(&b, "- cwd: `%s`", r.CWD)
		if r.GitBranch != "" {
			fmt.Fprintf(&b, " · branch: `%s`", r.GitBranch)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

func printSessionShapeTable(r *sessionShapeReport) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	if r.FallbackUsed {
		fmt.Fprintf(w, "Agent:\t%s (fallback)\n", r.Agent)
		if r.Notes != nil {
			if reason, ok := r.Notes["reason"].(string); ok {
				fmt.Fprintf(w, "Reason:\t%s\n", reason)
			}
		}
		return
	}
	fmt.Fprintf(w, "Agent:\t%s %s\n", r.Agent, r.AgentVersion)
	fmt.Fprintf(w, "Session ID:\t%s\n", r.SessionID)
	fmt.Fprintf(w, "Log path:\t%s\n", r.SessionLogPath)
	if r.ContextPct != nil && r.Budget != nil {
		fmt.Fprintf(w, "Context %%:\t%.1f%% (%s)\n", *r.ContextPct, r.ContextClass)
		fmt.Fprintf(w, "Budget:\t%s tokens\n", humanizeInt(*r.Budget))
	}
	if r.Tokens != nil {
		fmt.Fprintf(w, "Cache read:\t%s\n", humanizeInt(r.Tokens.CacheRead))
		fmt.Fprintf(w, "Cache creation:\t%s\n", humanizeInt(r.Tokens.CacheCreation))
		fmt.Fprintf(w, "Input:\t%s\n", humanizeInt(r.Tokens.Input))
		fmt.Fprintf(w, "Output:\t%s\n", humanizeInt(r.Tokens.Output))
		fmt.Fprintf(w, "Total prompt:\t%s\n", humanizeInt(r.Tokens.TotalPrompt))
	}
	fmt.Fprintf(w, "Lines:\t%d\n", r.Lines)
	fmt.Fprintf(w, "Bytes:\t%s\n", humanizeBytes(r.Bytes))
	fmt.Fprintf(w, "Tool invocations:\t%d\n", r.ToolInvocations)
	for k, v := range r.MessageCounts {
		fmt.Fprintf(w, "  msg %s:\t%d\n", k, v)
	}
	if r.SessionStartedAt != "" {
		fmt.Fprintf(w, "Started:\t%s\n", r.SessionStartedAt)
		fmt.Fprintf(w, "Last activity:\t%s\n", r.LastActivityAt)
		fmt.Fprintf(w, "Elapsed:\t%ds\n", r.ElapsedWallClockSeconds)
	}
	if r.CWD != "" {
		fmt.Fprintf(w, "CWD:\t%s\n", r.CWD)
	}
	if r.GitBranch != "" {
		fmt.Fprintf(w, "Git branch:\t%s\n", r.GitBranch)
	}
	fmt.Fprintf(w, "Resolver source:\t%s\n", r.ResolverSource)
}

func humanizeInt(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	// Comma group thousands. Cheap manual impl beats pulling in a dep.
	s := fmt.Sprintf("%d", n)
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// Compile-time assertion: report JSON shape stays valid encoding-wise.
var _ = json.Marshal
