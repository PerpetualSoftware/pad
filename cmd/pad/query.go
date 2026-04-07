package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/server"
)

type relatedEntry struct {
	Ref            string `json:"ref,omitempty"`
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	Status         string `json:"status,omitempty"`
}

type relatedGroup struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Entries []relatedEntry `json:"entries"`
}

func readyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ready",
		Short: "Show actionable next items for an agent",
		Long: `List the items that Pad currently considers ready to work on.

This is the broader query-oriented counterpart to 'pad project next'. It reuses the
dashboard's suggested-next logic and returns the current actionable backlog
for active plans.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			dash, err := loadDashboard(client, ws)
			if err != nil {
				return err
			}

			suggestions := dash.SuggestedNext
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"count":   len(suggestions),
					"results": suggestions,
				})
			}

			if len(suggestions) == 0 {
				fmt.Println("No ready items found.")
				return nil
			}

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			fmt.Println("Ready to work on:")
			for i, s := range suggestions {
				label := strings.TrimSpace(strings.Join([]string{s.ItemRef, s.ItemTitle}, " "))
				fmt.Printf("  %s %s\n", dim.Sprintf("%d.", i+1), bold.Sprint(label))
				fmt.Printf("     %s\n", dim.Sprint(s.Reason))
			}
			return nil
		},
	}
}

func staleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stale",
		Short: "Show stale or attention-worthy items for an agent",
		Long: `List the current workspace items that need attention because they are stalled,
blocked, overdue, or otherwise falling out of the active workflow.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			dash, err := loadDashboard(client, ws)
			if err != nil {
				return err
			}

			attention := filterAgentAttention(dash.Attention)
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"count":   len(attention),
					"results": attention,
				})
			}

			if len(attention) == 0 {
				fmt.Println("No stale items found.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "REF\tTITLE\tTYPE\tREASON\n")
			for _, item := range attention {
				label := item.ItemTitle
				if item.ItemRef != "" {
					label = item.ItemRef
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", label, item.ItemTitle, item.Type, item.Reason)
			}
			w.Flush()
			return nil
		},
	}
}

func relatedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "related <ref>",
		Short: "Show direct relationships around an item",
		Long: `Show the direct dependency, lineage, wiki-link, and related-item graph around a single item.

This is a query-oriented view of the item's immediate context.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}
			links, err := client.GetItemLinks(ws, item.Slug)
			if err != nil {
				return err
			}

			groups := buildRelatedGroups(item, links)
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"item_ref":    cli.ItemRef(*item),
					"item_title":  item.Title,
					"collection":  item.CollectionSlug,
					"group_count": len(groups),
					"groups":      groups,
				})
			}

			fmt.Printf("Related context for %s\n", itemDisplayLabel(item))
			if len(groups) == 0 {
				fmt.Println()
				fmt.Println("No related items found.")
				return nil
			}
			for _, group := range groups {
				fmt.Printf("\n%s\n", color.New(color.Bold).Sprint(group.Label+":"))
				for _, entry := range group.Entries {
					line := strings.TrimSpace(strings.Join([]string{entry.Ref, entry.Title}, " "))
					if line == "" {
						line = entry.Title
					}
					fmt.Printf("  • %s", line)
					if entry.Status != "" {
						fmt.Printf("  %s", color.New(color.Faint).Sprint(entry.Status))
					}
					fmt.Println()
				}
			}
			return nil
		},
	}
}

func implementedByCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "implemented-by <ref>",
		Short: "Show items that implement the given item",
		Long: `List the direct incoming "implements" lineage relationships for an item.

This is useful for ideas or parent tasks that are realized by one or more
implementation tasks.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}
			links, err := client.GetItemLinks(ws, item.Slug)
			if err != nil {
				return err
			}

			entries := incomingImplementedBy(item, links)
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"item_ref":   cli.ItemRef(*item),
					"item_title": item.Title,
					"count":      len(entries),
					"results":    entries,
				})
			}

			fmt.Printf("Implemented by for %s\n", itemDisplayLabel(item))
			if len(entries) == 0 {
				fmt.Println()
				fmt.Println("No implementing items found.")
				return nil
			}
			for _, entry := range entries {
				line := strings.TrimSpace(strings.Join([]string{entry.Ref, entry.Title}, " "))
				fmt.Printf("  • %s", line)
				if entry.Status != "" {
					fmt.Printf("  %s", color.New(color.Faint).Sprint(entry.Status))
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func loadDashboard(client *cli.Client, ws string) (*server.DashboardResponse, error) {
	dashJSON, err := client.GetDashboard(ws)
	if err != nil {
		return nil, err
	}
	var dash server.DashboardResponse
	if err := json.Unmarshal(dashJSON, &dash); err != nil {
		return nil, err
	}
	return &dash, nil
}

func filterAgentAttention(attention []server.DashboardAttention) []server.DashboardAttention {
	interesting := map[string]bool{
		"stalled":       true,
		"blocked":       true,
		"overdue":       true,
		"orphaned_task": true,
	}

	results := make([]server.DashboardAttention, 0, len(attention))
	for _, item := range attention {
		if interesting[item.Type] {
			results = append(results, item)
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Type != results[j].Type {
			return results[i].Type < results[j].Type
		}
		if results[i].ItemRef != results[j].ItemRef {
			return results[i].ItemRef < results[j].ItemRef
		}
		return results[i].ItemTitle < results[j].ItemTitle
	})
	return results
}

func buildRelatedGroups(item *models.Item, links []models.ItemLink) []relatedGroup {
	if item == nil || len(links) == 0 {
		return nil
	}

	type groupDef struct {
		label string
	}
	definitions := map[string]groupDef{
		"blocks":         {label: "Blocks"},
		"blocked_by":     {label: "Blocked by"},
		"links_to":       {label: "Links to"},
		"referenced_by":  {label: "Referenced by"},
		"split_from":     {label: "Split from"},
		"split_into":     {label: "Split into"},
		"supersedes":     {label: "Supersedes"},
		"superseded_by":  {label: "Superseded by"},
		"implements":     {label: "Implements"},
		"implemented_by": {label: "Implemented by"},
		"related":        {label: "Related"},
	}
	order := []string{"blocks", "blocked_by", "links_to", "referenced_by", "split_from", "split_into", "supersedes", "superseded_by", "implements", "implemented_by", "related"}

	grouped := map[string][]relatedEntry{}
	for _, link := range links {
		linkType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			linkType = models.ItemLinkTypeRelated
		}
		isSource := link.SourceID == item.ID

		switch linkType {
		case models.ItemLinkTypeBlocks:
			if isSource {
				grouped["blocks"] = append(grouped["blocks"], relatedEntryFromLink(link, false))
			} else {
				grouped["blocked_by"] = append(grouped["blocked_by"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeWikiLink:
			if isSource {
				grouped["links_to"] = append(grouped["links_to"], relatedEntryFromLink(link, false))
			} else {
				grouped["referenced_by"] = append(grouped["referenced_by"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeSplitFrom:
			if isSource {
				grouped["split_from"] = append(grouped["split_from"], relatedEntryFromLink(link, false))
			} else {
				grouped["split_into"] = append(grouped["split_into"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeSupersedes:
			if isSource {
				grouped["supersedes"] = append(grouped["supersedes"], relatedEntryFromLink(link, false))
			} else {
				grouped["superseded_by"] = append(grouped["superseded_by"], relatedEntryFromLink(link, true))
			}
		case models.ItemLinkTypeImplements:
			if isSource {
				grouped["implements"] = append(grouped["implements"], relatedEntryFromLink(link, false))
			} else {
				grouped["implemented_by"] = append(grouped["implemented_by"], relatedEntryFromLink(link, true))
			}
		default:
			grouped["related"] = append(grouped["related"], relatedEntryFromLink(link, !isSource))
		}
	}

	results := make([]relatedGroup, 0, len(order))
	for _, key := range order {
		entries := grouped[key]
		if len(entries) == 0 {
			continue
		}
		results = append(results, relatedGroup{
			Key:     key,
			Label:   definitions[key].label,
			Entries: entries,
		})
	}
	return results
}

func incomingImplementedBy(item *models.Item, links []models.ItemLink) []relatedEntry {
	if item == nil {
		return nil
	}

	results := make([]relatedEntry, 0)
	for _, link := range links {
		linkType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			continue
		}
		if linkType != models.ItemLinkTypeImplements || link.TargetID != item.ID {
			continue
		}
		results = append(results, relatedEntryFromLink(link, true))
	}
	return results
}

func relatedEntryFromLink(link models.ItemLink, useSource bool) relatedEntry {
	if useSource {
		return relatedEntry{
			Ref:            link.SourceRef,
			Title:          link.SourceTitle,
			CollectionSlug: link.SourceCollectionSlug,
			Status:         link.SourceStatus,
		}
	}
	return relatedEntry{
		Ref:            link.TargetRef,
		Title:          link.TargetTitle,
		CollectionSlug: link.TargetCollectionSlug,
		Status:         link.TargetStatus,
	}
}
