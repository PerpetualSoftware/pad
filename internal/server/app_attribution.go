package server

import (
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// App attribution on human-facing JSON (SPEC-6 U9c, TASK-3413): via_app and
// via_app_name on items (the creating app), comments and version rows, read
// in one batch per response. A lookup failure leaves the fields absent and
// is logged: the label is a decoration, and the response is not refused
// for it.

func (s *Server) hydrateItemsAppAttribution(items []models.Item) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	attr, err := s.store.ItemsCreatedViaApp(ids)
	if err != nil {
		slog.Error("app attribution: items", "error", err)
		return
	}
	for i := range items {
		if a, ok := attr[items[i].ID]; ok {
			items[i].ViaApp, items[i].ViaAppName = a.InstallID, a.Name
		}
	}
}

func (s *Server) hydrateCommentsAppAttribution(comments []models.Comment) {
	if len(comments) == 0 {
		return
	}
	ids := make([]string, len(comments))
	for i := range comments {
		ids[i] = comments[i].ID
	}
	attr, err := s.store.CommentsViaApp(ids)
	if err != nil {
		slog.Error("app attribution: comments", "error", err)
		return
	}
	for i := range comments {
		if a, ok := attr[comments[i].ID]; ok {
			comments[i].ViaApp, comments[i].ViaAppName = a.InstallID, a.Name
		}
	}
}

func (s *Server) hydrateVersionsAppAttribution(versions []models.Version) {
	if len(versions) == 0 {
		return
	}
	ids := make([]string, len(versions))
	for i := range versions {
		ids[i] = versions[i].ID
	}
	attr, err := s.store.VersionsViaApp(ids)
	if err != nil {
		slog.Error("app attribution: versions", "error", err)
		return
	}
	for i := range versions {
		if a, ok := attr[versions[i].ID]; ok {
			versions[i].ViaApp, versions[i].ViaAppName = a.InstallID, a.Name
		}
	}
}
