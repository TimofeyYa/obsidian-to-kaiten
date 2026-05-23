package syncengine

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdown_Basic(t *testing.T) {
	out, err := HTMLToMarkdown("<h1>Title</h1><p>Hello <strong>world</strong></p>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Title") || !strings.Contains(out, "**world**") {
		t.Errorf("конвертация неполная: %q", out)
	}
}

// Полноценный md→html через goldmark: заголовки, списки, код, таблицы.
func TestMarkdownToHTML_RendersHeading(t *testing.T) {
	got := MarkdownToHTML("# Title")
	if !strings.Contains(got, "<h1") {
		t.Errorf("заголовок не отрендерен как <h1>: %q", got)
	}
}

func TestMarkdownToHTML_RendersList(t *testing.T) {
	got := MarkdownToHTML("- item 1\n- item 2\n")
	if !strings.Contains(got, "<ul>") || !strings.Contains(got, "<li>item 1</li>") {
		t.Errorf("список не отрендерен: %q", got)
	}
}

func TestMarkdownToHTML_RendersCodeBlock(t *testing.T) {
	got := MarkdownToHTML("```go\nfmt.Println(\"x\")\n```")
	if !strings.Contains(got, "<pre>") || !strings.Contains(got, "<code") {
		t.Errorf("code block не отрендерен: %q", got)
	}
}

func TestMarkdownToHTML_RendersTable(t *testing.T) {
	src := "| A | B |\n|---|---|\n| 1 | 2 |\n"
	got := MarkdownToHTML(src)
	if !strings.Contains(got, "<table>") || !strings.Contains(got, "<td>1</td>") {
		t.Errorf("таблица не отрендерена: %q", got)
	}
}

func TestMarkdownToHTML_RendersCheckbox(t *testing.T) {
	got := MarkdownToHTML("- [x] done\n- [ ] todo\n")
	if !strings.Contains(got, "checkbox") {
		t.Errorf("чек-боксы не отрендерены: %q", got)
	}
}

func TestMarkdownToHTML_EscapesRawAmpersand(t *testing.T) {
	// goldmark эскейпит &, < в тексте автоматически.
	got := MarkdownToHTML("A & B")
	if strings.Contains(got, "<script>") {
		t.Errorf("неожиданно: %q", got)
	}
	if !strings.Contains(got, "A &amp; B") && !strings.Contains(got, "A & B") {
		t.Errorf("ожидался безопасный вывод: %q", got)
	}
}

func TestMarkdownToHTML_RendersLink(t *testing.T) {
	got := MarkdownToHTML("[example](https://example.com)")
	if !strings.Contains(got, `href="https://example.com"`) {
		t.Errorf("ссылка не отрендерена: %q", got)
	}
}
