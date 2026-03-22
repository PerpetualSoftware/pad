package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/xarmian/pad/internal/config"
)

// OpenInEditor opens a temp file with content in the user's editor,
// waits for the editor to close, and returns the (possibly modified) content.
func OpenInEditor(cfg *config.Config, content, suffix string) (string, error) {
	editor := cfg.Editor
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}

	// Create temp file
	tmpFile, err := os.CreateTemp("", "pad-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()

	// Parse editor command (may have flags like "code --wait")
	parts := strings.Fields(editor)
	args := append(parts[1:], tmpPath)
	cmd := exec.Command(parts[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited with error: %w", err)
	}

	// Read modified content
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read temp file: %w", err)
	}

	return string(data), nil
}

// ParseFrontmatter splits content into YAML frontmatter and body.
// Returns frontmatter map and body content.
func ParseFrontmatter(content string) (map[string]string, string) {
	meta := make(map[string]string)

	if !strings.HasPrefix(content, "---\n") {
		return meta, content
	}

	end := strings.Index(content[4:], "\n---\n")
	if end < 0 {
		return meta, content
	}

	frontmatter := content[4 : 4+end]
	body := content[4+end+5:] // skip past \n---\n

	for _, line := range strings.Split(frontmatter, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			meta[key] = value
		}
	}

	return meta, body
}

// BuildFrontmatter creates YAML frontmatter from document metadata.
func BuildFrontmatter(title, docType, status, tags string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("title: %s\n", title))
	b.WriteString(fmt.Sprintf("type: %s\n", docType))
	b.WriteString(fmt.Sprintf("status: %s\n", status))
	b.WriteString(fmt.Sprintf("tags: %s\n", tags))
	b.WriteString("---\n\n")
	return b.String()
}
