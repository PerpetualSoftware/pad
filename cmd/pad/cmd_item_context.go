package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// itemContextView is the subset of GET /items/{ref}/context (PLAN-3598) the
// human render reads. --format json prints the server's body verbatim, so
// this struct never narrows what a script sees.
type itemContextView struct {
	Intent   string   `json:"intent"`
	Triggers []string `json:"triggers"`
	Role     string   `json:"role"`
	Budget   int      `json:"budget"`
	Used     int      `json:"used"`
	Item     struct {
		Ref     string `json:"ref"`
		Title   string `json:"title"`
		Seq     int64  `json:"seq"`
		Content string `json:"content"`
	} `json:"item"`
	Parents []struct {
		Ref                string `json:"ref"`
		Title              string `json:"title"`
		Seq                int64  `json:"seq"`
		Status             string `json:"status"`
		Content            string `json:"content"`
		AcceptanceCriteria *struct {
			Criteria []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"criteria"`
			Placeholders []string `json:"placeholders"`
		} `json:"acceptance_criteria"`
	} `json:"parents"`
	Conventions []struct {
		Ref      string `json:"ref"`
		Title    string `json:"title"`
		Seq      int64  `json:"seq"`
		Trigger  string `json:"trigger"`
		Priority string `json:"priority"`
		Role     string `json:"role"`
		Content  string `json:"content"`
	} `json:"conventions"`
	Playbooks []struct {
		Ref            string `json:"ref"`
		Title          string `json:"title"`
		InvocationSlug string `json:"invocation_slug"`
	} `json:"playbooks"`
	Deps struct {
		Blocks    []itemContextLinkView `json:"blocks"`
		BlockedBy []itemContextLinkView `json:"blocked_by"`
	} `json:"deps"`
	Decisions []struct {
		QuestionKey string `json:"question_key"`
		Current     bool   `json:"current"`
	} `json:"decisions"`
	Links   []itemContextLinkView `json:"links"`
	Omitted []struct {
		Part string `json:"part"`
		Ref  string `json:"ref"`
		Size int    `json:"size"`
	} `json:"omitted"`
	Conflicts []struct {
		Kind   string `json:"kind"`
		Detail string `json:"detail"`
	} `json:"conflicts"`
}

type itemContextLinkView struct {
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	LinkType string `json:"link_type"`
	Status   string `json:"status"`
}

func contextCmd() *cobra.Command {
	var intent, role string
	var budget int
	cmd := &cobra.Command{
		Use:   "context <ref>",
		Short: "Everything an agent needs before working an item, in one brief",
		Long: `Fetch one deterministic brief for working an item (PLAN-3598): the item
with its seq, its parent chain with parsed acceptance criteria, the bodies
of the conventions whose trigger matches the intent (plus the always-on
ones), matching playbooks, dependencies with statuses, typed decisions,
linked items, and the sections that say what was left out for size and
which rules may conflict.

--intent maps onto the workspace's convention triggers: "implement" means
on-implement when that trigger is declared. --role keeps a convention scoped
to another role out of the brief. --budget caps the brief in characters;
whole parts are dropped from the end, never cut, and each is listed under
omitted.`,
		Example: `  pad item context TASK-5 --intent implement
  pad item context TASK-5 --intent implement --role implementer --format json
  pad item context TASK-5 --intent review --budget 20000`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			raw, err := client.GetItemContext(ws, args[0], intent, role, budget)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				_, err := os.Stdout.Write(append(raw, '\n'))
				return err
			}
			var v itemContextView
			if err := json.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("decode the context brief: %w", err)
			}
			renderItemContextMarkdown(os.Stdout, &v)
			return nil
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "what you are about to do (implement, review, commit, ...): selects the conventions whose trigger matches")
	cmd.Flags().StringVar(&role, "role", "", "your agent role slug: conventions scoped to another role are left out")
	cmd.Flags().IntVar(&budget, "budget", 0, "size cap in characters (default 40000 on the server)")
	return cmd
}

func renderItemContextMarkdown(w io.Writer, v *itemContextView) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("# %s: %s (seq %d)\n\n", v.Item.Ref, v.Item.Title, v.Item.Seq)
	p("Intent: %s (triggers: %s)", orDash(v.Intent), strings.Join(v.Triggers, ", "))
	if v.Role != "" {
		p("; role: %s", v.Role)
	}
	p("; %d of %d characters\n\n", v.Used, v.Budget)
	if strings.TrimSpace(v.Item.Content) != "" {
		p("%s\n\n", strings.TrimSpace(v.Item.Content))
	}
	if len(v.Parents) > 0 {
		p("## Parents\n\n")
		for i, a := range v.Parents {
			p("- %s %s (%s, seq %d)\n", a.Ref, a.Title, orDash(a.Status), a.Seq)
			if a.AcceptanceCriteria != nil {
				for _, c := range a.AcceptanceCriteria.Criteria {
					p("  - %s: %s\n", c.ID, c.Text)
				}
				if n := len(a.AcceptanceCriteria.Placeholders); n > 0 {
					p("  - (%d placeholder criteria not counted)\n", n)
				}
			}
			if i == 0 && strings.TrimSpace(a.Content) != "" {
				p("\n%s\n", indentBlock(strings.TrimSpace(a.Content), "  > "))
			}
		}
		p("\n")
	}
	if len(v.Conventions) > 0 {
		p("## Conventions\n\n")
		for _, c := range v.Conventions {
			p("### %s %s (%s, %s", c.Ref, c.Title, c.Trigger, orDash(c.Priority))
			if c.Role != "" {
				p(", role %s", c.Role)
			}
			p(")\n\n%s\n\n", strings.TrimSpace(c.Content))
		}
	}
	if len(v.Playbooks) > 0 {
		p("## Playbooks\n\n")
		for _, pb := range v.Playbooks {
			p("- %s %s", pb.Ref, pb.Title)
			if pb.InvocationSlug != "" {
				p(" (/%s)", pb.InvocationSlug)
			}
			p("\n")
		}
		p("\n")
	}
	if len(v.Deps.Blocks)+len(v.Deps.BlockedBy) > 0 {
		p("## Dependencies\n\n")
		for _, d := range v.Deps.BlockedBy {
			p("- blocked by %s %s (%s)\n", d.Ref, d.Title, orDash(d.Status))
		}
		for _, d := range v.Deps.Blocks {
			p("- blocks %s %s (%s)\n", d.Ref, d.Title, orDash(d.Status))
		}
		p("\n")
	}
	if len(v.Decisions) > 0 {
		p("## Decisions\n\n")
		for _, d := range v.Decisions {
			state := "current"
			if !d.Current {
				state = "stale"
			}
			p("- %s (%s)\n", d.QuestionKey, state)
		}
		p("\n")
	}
	if len(v.Links) > 0 {
		p("## Linked items\n\n")
		for _, l := range v.Links {
			p("- %s %s %s (%s)\n", l.LinkType, l.Ref, l.Title, orDash(l.Status))
		}
		p("\n")
	}
	if len(v.Conflicts) > 0 {
		p("## Conflicts\n\n")
		for _, c := range v.Conflicts {
			p("- %s: %s\n", c.Kind, c.Detail)
		}
		p("\n")
	}
	if len(v.Omitted) > 0 {
		p("## Omitted for size\n\n")
		for _, o := range v.Omitted {
			p("- %s %s (%d characters)\n", o.Part, orDash(o.Ref), o.Size)
		}
		p("\n")
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
