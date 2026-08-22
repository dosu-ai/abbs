package ui

import (
	"bytes"
	"html/template"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// markdownRenderer is the only path from untrusted message markdown to
// template.HTML. Goldmark's unsafe/raw HTML renderer is deliberately not
// enabled, then bluemonday sanitizes the generated HTML as a second layer.
type markdownRenderer struct {
	markdown goldmark.Markdown
	policy   *bluemonday.Policy
}

func newMarkdownRenderer() *markdownRenderer {
	return &markdownRenderer{
		markdown: goldmark.New(goldmark.WithExtensions(extension.GFM)),
		policy:   bluemonday.UGCPolicy(),
	}
}

func (r *markdownRenderer) Render(source string) template.HTML {
	var rendered bytes.Buffer
	if err := r.markdown.Convert([]byte(source), &rendered); err != nil {
		return ""
	}
	return template.HTML(r.policy.SanitizeBytes(rendered.Bytes())) // #nosec G203 -- sanitized immediately above.
}
