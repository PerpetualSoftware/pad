package links

import "regexp"

var linkPattern = regexp.MustCompile(`\[\[([^\]]+)\]\]`)

// Extract returns all [[Doc Title]] references from content.
func Extract(content string) []string {
	matches := linkPattern.FindAllStringSubmatch(content, -1)
	seen := make(map[string]bool)
	var titles []string
	for _, m := range matches {
		title := m[1]
		if !seen[title] {
			seen[title] = true
			titles = append(titles, title)
		}
	}
	return titles
}

// ReplaceTitle replaces all [[oldTitle]] with [[newTitle]] in content.
func ReplaceTitle(content, oldTitle, newTitle string) string {
	old := "[[" + oldTitle + "]]"
	new := "[[" + newTitle + "]]"
	return replaceAll(content, old, new)
}

func replaceAll(s, old, new string) string {
	// Simple string replacement, not regex-based
	result := s
	for {
		i := indexOf(result, old)
		if i < 0 {
			break
		}
		result = result[:i] + new + result[i+len(old):]
	}
	return result
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
