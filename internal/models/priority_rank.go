package models

import "strings"

// PriorityRank returns a sort rank for task priority and severity
// (lower = higher priority; critical > high > medium > low, case-insensitive,
// anything else last). The dashboard, project next and the item list's
// priority sort (BUG-4400) all read this one function, so the three cannot
// disagree about which item is first.
func PriorityRank(priority string) int {
	switch strings.ToLower(priority) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

// ConventionPriorityRank ranks convention priority strings
// (must > should > nice-to-have). Lower rank = higher priority. Unknown
// values rank last so untyped data doesn't dominate the head of the list.
// Distinct from PriorityRank because conventions and tasks have disjoint
// priority vocabularies.
func ConventionPriorityRank(p string) int {
	switch p {
	case "must":
		return 0
	case "should":
		return 1
	case "nice-to-have":
		return 2
	default:
		return 3
	}
}
