package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/models"
)

type lineageLinkSpec struct {
	linkType       string
	missingMessage string
	successMessage string
}

func splitFromCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "split-from <child-ref> <parent-ref>",
		Short: "Mark that an item was split from another item",
		Long: `Create a lineage relationship showing that one item was split from another.

The first item is the derived item, and the second item is the original source.
For example:
  pad split-from TASK-122 TASK-121`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createLineageLink(lineageLinkSpec{
				linkType:       models.ItemLinkTypeSplitFrom,
				successMessage: "%s is now marked as split from %s\n",
			}, args[0], args[1])
		},
	}
}

func supersedesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "supersedes <new-ref> <old-ref>",
		Short: "Mark that one item supersedes another",
		Long: `Create a lineage relationship showing that one item supersedes another.

The first item is the newer replacement, and the second item is the older item.
For example:
  pad supersedes TASK-130 TASK-118`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createLineageLink(lineageLinkSpec{
				linkType:       models.ItemLinkTypeSupersedes,
				successMessage: "%s now supersedes %s\n",
			}, args[0], args[1])
		},
	}
}

func implementsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "implements <implementer-ref> <target-ref>",
		Short: "Mark that one item implements another",
		Long: `Create a lineage relationship showing that one item implements another.

The first item is the implementation work item, and the second item is the item being implemented.
For example:
  pad implements TASK-121 IDEA-108`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createLineageLink(lineageLinkSpec{
				linkType:       models.ItemLinkTypeImplements,
				successMessage: "%s now implements %s\n",
			}, args[0], args[1])
		},
	}
}

func unsplitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsplit <child-ref> <parent-ref>",
		Short: "Remove a split-from relationship",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteLineageLink(lineageLinkSpec{
				linkType:       models.ItemLinkTypeSplitFrom,
				missingMessage: "no split-from relationship found: %s is not marked as split from %s",
				successMessage: "%s is no longer marked as split from %s\n",
			}, args[0], args[1])
		},
	}
}

func unsupersedeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsupersede <new-ref> <old-ref>",
		Short: "Remove a supersedes relationship",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteLineageLink(lineageLinkSpec{
				linkType:       models.ItemLinkTypeSupersedes,
				missingMessage: "no supersedes relationship found: %s does not supersede %s",
				successMessage: "%s no longer supersedes %s\n",
			}, args[0], args[1])
		},
	}
}

func unimplementsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unimplements <implementer-ref> <target-ref>",
		Short: "Remove an implements relationship",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteLineageLink(lineageLinkSpec{
				linkType:       models.ItemLinkTypeImplements,
				missingMessage: "no implements relationship found: %s does not implement %s",
				successMessage: "%s no longer implements %s\n",
			}, args[0], args[1])
		},
	}
}

func createLineageLink(spec lineageLinkSpec, sourceRef, targetRef string) error {
	client, _ := getClient()
	ws := getWorkspace()

	source, err := client.GetItem(ws, sourceRef)
	if err != nil {
		return fmt.Errorf("resolve source %q: %w", sourceRef, err)
	}
	target, err := client.GetItem(ws, targetRef)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", targetRef, err)
	}

	input := models.ItemLinkCreate{
		TargetID: target.ID,
		LinkType: spec.linkType,
	}
	link, err := client.CreateItemLink(ws, source.Slug, input)
	if err != nil {
		return err
	}

	if formatFlag == "json" {
		return cli.PrintJSON(link)
	}

	fmt.Printf(spec.successMessage, itemDisplayLabel(source), itemDisplayLabel(target))
	return nil
}

func deleteLineageLink(spec lineageLinkSpec, sourceRef, targetRef string) error {
	client, _ := getClient()
	ws := getWorkspace()

	source, err := client.GetItem(ws, sourceRef)
	if err != nil {
		return fmt.Errorf("resolve source %q: %w", sourceRef, err)
	}
	target, err := client.GetItem(ws, targetRef)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", targetRef, err)
	}

	links, err := client.GetItemLinks(ws, source.Slug)
	if err != nil {
		return err
	}

	linkID := findMatchingLinkID(links, spec.linkType, source.ID, target.ID)
	if linkID == "" {
		return fmt.Errorf(spec.missingMessage, itemDisplayLabel(source), itemDisplayLabel(target))
	}

	if err := client.DeleteItemLink(ws, linkID); err != nil {
		return err
	}

	if formatFlag == "json" {
		return cli.PrintJSON(map[string]string{"status": "removed"})
	}

	fmt.Printf(spec.successMessage, itemDisplayLabel(source), itemDisplayLabel(target))
	return nil
}

func findMatchingLinkID(links []models.ItemLink, linkType, sourceID, targetID string) string {
	canonicalType, err := models.NormalizeItemLinkType(linkType)
	if err != nil {
		return ""
	}
	for _, link := range links {
		if link.SourceID != sourceID || link.TargetID != targetID {
			continue
		}
		normalizedType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			continue
		}
		if normalizedType == canonicalType {
			return link.ID
		}
	}
	return ""
}

func itemDisplayLabel(item *models.Item) string {
	ref := cli.ItemRef(*item)
	if ref == "" {
		return item.Title
	}
	return ref + " " + item.Title
}

type relationshipSection struct {
	Title   string
	Entries []string
}

func buildLineageSections(item *models.Item, links []models.ItemLink) []relationshipSection {
	groups := map[string][]string{}
	order := []string{"Split from", "Split into", "Supersedes", "Superseded by", "Implements", "Implemented by"}

	for _, link := range links {
		linkType, err := models.NormalizeItemLinkType(link.LinkType)
		if err != nil {
			continue
		}
		isSource := link.SourceID == item.ID
		switch linkType {
		case models.ItemLinkTypeSplitFrom:
			if isSource {
				groups["Split from"] = append(groups["Split from"], linkEndpointDisplay(link, false))
			} else if link.TargetID == item.ID {
				groups["Split into"] = append(groups["Split into"], linkEndpointDisplay(link, true))
			}
		case models.ItemLinkTypeSupersedes:
			if isSource {
				groups["Supersedes"] = append(groups["Supersedes"], linkEndpointDisplay(link, false))
			} else if link.TargetID == item.ID {
				groups["Superseded by"] = append(groups["Superseded by"], linkEndpointDisplay(link, true))
			}
		case models.ItemLinkTypeImplements:
			if isSource {
				groups["Implements"] = append(groups["Implements"], linkEndpointDisplay(link, false))
			} else if link.TargetID == item.ID {
				groups["Implemented by"] = append(groups["Implemented by"], linkEndpointDisplay(link, true))
			}
		}
	}

	sections := make([]relationshipSection, 0, len(order))
	for _, title := range order {
		entries := groups[title]
		if len(entries) == 0 {
			continue
		}
		sections = append(sections, relationshipSection{Title: title, Entries: entries})
	}
	return sections
}

func linkEndpointDisplay(link models.ItemLink, useSource bool) string {
	var ref, title, status, id string
	if useSource {
		ref = link.SourceRef
		title = link.SourceTitle
		status = link.SourceStatus
		id = link.SourceID
	} else {
		ref = link.TargetRef
		title = link.TargetTitle
		status = link.TargetStatus
		id = link.TargetID
	}
	label := title
	if ref != "" && title != "" {
		label = ref + " " + title
	} else if ref != "" {
		label = ref
	} else if label == "" {
		label = id
	}
	if status != "" {
		label += " (" + status + ")"
	}
	return label
}
