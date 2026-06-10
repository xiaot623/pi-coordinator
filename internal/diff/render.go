package diff

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
)

//go:embed template/diff.html
var diffTemplateFS embed.FS

// RenderOptions configures the HTML diff output
type RenderOptions struct {
	SessionID    string
	Branch       string
	WorktreePath string
}

// RenderHTML generates a self-contained HTML page with diff2html rendering
func RenderHTML(diffOutput string, opts RenderOptions) ([]byte, error) {
	diffJSON, err := json.Marshal(diffOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal diff: %w", err)
	}

	title := fmt.Sprintf("Diff: %s", opts.SessionID)
	if opts.Branch != "" {
		title = fmt.Sprintf("Diff: %s @ %s", opts.SessionID, opts.Branch)
	}

	data := struct {
		Title    string
		DiffJSON template.JS
	}{
		Title:    title,
		DiffJSON: template.JS(diffJSON),
	}

	tmpl, err := template.ParseFS(diffTemplateFS, "template/diff.html")
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	return []byte(buf.String()), nil
}
