package collections

import "github.com/PerpetualSoftware/pad/internal/models"

// templates_family.go holds the two hidden household templates registered
// in templates.go: `family-shared` (household lists: groceries, events,
// to-dos, notes) and `family-personal` (notes, to-dos, events). Both are
// built from the same collection helpers below, so a collection has one
// schema whichever template seeds it. Like `blank`, both ship the system
// rails (Conventions, Playbooks) with the minimal blank vocabularies and
// seed no items, conventions or playbooks.

// familySharedTemplate builds the "family-shared" workspace template.
func familySharedTemplate() WorkspaceTemplate {
	return WorkspaceTemplate{
		Name:        "family-shared",
		Category:    CategoryCustom,
		Description: "Household lists: groceries, events, to-dos, notes",
		Icon:        "\U0001F3E0", // 🏠
		Hidden:      true,
		Collections: []DefaultCollection{
			familyGroceriesCollection(0),
			familyEventsCollection(1),
			familyTodoCollection(2),
			familyNotesCollection(3),
			conventionsCollection(4, BlankConventionTriggers, BlankConventionScopes),
			playbooksCollection(5, BlankPlaybookTriggers, BlankPlaybookScopes),
		},
	}
}

// familyPersonalTemplate builds the "family-personal" workspace template.
func familyPersonalTemplate() WorkspaceTemplate {
	return WorkspaceTemplate{
		Name:        "family-personal",
		Category:    CategoryCustom,
		Description: "Personal lists: notes, to-dos, events",
		Icon:        "\U0001F464", // 👤
		Hidden:      true,
		Collections: []DefaultCollection{
			familyNotesCollection(0),
			familyTodoCollection(1),
			familyEventsCollection(2),
			conventionsCollection(3, BlankConventionTriggers, BlankConventionScopes),
			playbooksCollection(4, BlankPlaybookTriggers, BlankPlaybookScopes),
		},
	}
}

func familyGroceriesCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Groceries",
		Slug:        "groceries",
		Prefix:      "GROC",
		Icon:        "\U0001F6D2", // 🛒
		Description: "Things to buy",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "status",
					Label:           "Status",
					Type:            "select",
					Options:         []string{"needed", "got"},
					TerminalOptions: []string{"got"},
					Default:         "needed",
					Required:        true,
				},
				{
					Key:      "store",
					Label:    "Store",
					Type:     "select",
					Options:  []string{"Either", "Costco", "Big Y"},
					Default:  "Either",
					Required: true,
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "fields-primary",
			DefaultView:  "list",
			BoardGroupBy: "status",
			ListGroupBy:  "store",
			ListSortBy:   "created_at",
		},
	}
}

func familyEventsCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Events",
		Slug:        "events",
		Prefix:      "EVT",
		Icon:        "\U0001F4C5", // 📅
		Description: "Appointments, outings and other dated plans",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:              "status",
					Label:            "Status",
					Type:             "select",
					Options:          []string{"upcoming", "done", "cancelled"},
					TerminalOptions:  []string{"done", "cancelled"},
					AbandonedOptions: []string{"cancelled"},
					Default:          "upcoming",
					Required:         true,
				},
				{
					Key:   "due_date",
					Label: "Date",
					Type:  "date",
				},
				{
					Key:   "end_date",
					Label: "End date",
					Type:  "date",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "fields-primary",
			DefaultView:  "list",
			BoardGroupBy: "status",
			ListSortBy:   "due_date",
		},
	}
}

func familyTodoCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "To do",
		Slug:        "todo",
		Prefix:      "TODO",
		Icon:        "✅", // ✅
		Description: "Things to get done",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:              "status",
					Label:            "Status",
					Type:             "select",
					Options:          []string{"open", "done", "cancelled"},
					TerminalOptions:  []string{"done", "cancelled"},
					AbandonedOptions: []string{"cancelled"},
					Default:          "open",
					Required:         true,
				},
				{
					Key:   "due_date",
					Label: "Due date",
					Type:  "date",
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "fields-primary",
			DefaultView:  "list",
			BoardGroupBy: "status",
			ListSortBy:   "due_date",
		},
	}
}

func familyNotesCollection(sortOrder int) DefaultCollection {
	return DefaultCollection{
		Name:        "Notes",
		Slug:        "notes",
		Prefix:      "NOTE",
		Icon:        "\U0001F4DD", // 📝
		Description: "Notes, lists and anything worth keeping",
		SortOrder:   sortOrder,
		Schema: models.CollectionSchema{
			Fields: []models.FieldDef{
				{
					Key:             "status",
					Label:           "Status",
					Type:            "select",
					Options:         []string{"active", "archived"},
					TerminalOptions: []string{"archived"},
					Default:         "active",
					Required:        true,
				},
			},
		},
		Settings: models.CollectionSettings{
			Layout:       "content-primary",
			DefaultView:  "list",
			BoardGroupBy: "status",
			ListSortBy:   "updated_at",
		},
	}
}
