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

func TestMarkdownToHTML_EscapesAmpersand(t *testing.T) {
	// Фикс бага #12 — & должен экранироваться, иначе целостность теряется
	// (Kaiten попытается распарсить &amp; как HTML-сущность).
	got := MarkdownToHTML("A & B")
	if !strings.Contains(got, "A &amp; B") {
		t.Errorf("& не экранирован: %q", got)
	}
}

func TestMarkdownToHTML_EscapesAngleBrackets(t *testing.T) {
	got := MarkdownToHTML("<script>x</script>")
	if strings.Contains(got, "<script>") {
		t.Errorf("HTML-теги не экранированы (XSS-риск): %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("ожидалось экранирование тегов: %q", got)
	}
}

func TestMarkdownToHTML_PreservesLineBreaks(t *testing.T) {
	got := MarkdownToHTML("line1\nline2")
	if !strings.Contains(got, "<br/>") {
		t.Errorf("переносы строк не сохранены: %q", got)
	}
}
