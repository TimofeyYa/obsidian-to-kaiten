package syncengine

import (
	"html"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown"
)

// HTMLToMarkdown конвертирует HTML-контент Kaiten в Markdown.
func HTMLToMarkdown(htmlStr string) (string, error) {
	conv := md.NewConverter("", true, nil)
	out, err := conv.ConvertString(htmlStr)
	if err != nil {
		return "", err
	}
	return out, nil
}

// MarkdownToHTML — упрощённая обратная конвертация для отправки в Kaiten.
// Использует html.EscapeString → корректно экранирует &, <, >, ", '.
// Это безопасный fallback; для полноценного рендера подключите goldmark.
func MarkdownToHTML(text string) string {
	var b strings.Builder
	b.WriteString("<div>")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		b.WriteString(html.EscapeString(line))
		if i < len(lines)-1 {
			b.WriteString("<br/>")
		}
	}
	b.WriteString("</div>")
	return b.String()
}
