package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsHiddenPath(t *testing.T) {
	cases := map[string]bool{
		"notes/a.md":              false,
		".obsidian/workspace":     true,
		"folder/.hidden/file.md":  true,
		"folder/visible/file.md":  false,
		".kaiten-sync/state.json": true,
		"":                        false,
		"file.md":                 false,
	}
	for in, want := range cases {
		if got := IsHiddenPath(in); got != want {
			t.Errorf("IsHiddenPath(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSplitFrontmatter(t *testing.T) {
	data := []byte(`---
kaiten_id: 42
kaiten_url: https://example.com/document/42
updated: 2026-05-23T10:00:00Z
---

# Hello

Body here.
`)
	fm, body, err := SplitFrontmatter(data)
	if err != nil {
		t.Fatal(err)
	}
	if fm.KaitenID != 42 {
		t.Errorf("kaiten_id = %d", fm.KaitenID)
	}
	if !strings.Contains(body, "Hello") {
		t.Errorf("body не содержит ожидаемого текста: %q", body)
	}
}

func TestSplitFrontmatter_NoFrontmatter(t *testing.T) {
	_, _, err := SplitFrontmatter([]byte("просто текст без frontmatter"))
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
}

func TestRoundtripWriteRead(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "sub", "doc.md")
	fm := Frontmatter{
		KaitenID:  77,
		KaitenURL: "https://example/document/77",
		Updated:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Type:      "html",
	}
	body := "# Заголовок\n\nТело документа\n"
	if err := WriteAtomic(abs, fm, body, false); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFile(dir, abs)
	if err != nil {
		t.Fatal(err)
	}
	if f.Frontmatter.KaitenID != 77 {
		t.Errorf("id = %d", f.Frontmatter.KaitenID)
	}
	if !strings.Contains(f.Body, "Заголовок") {
		t.Errorf("body lost: %q", f.Body)
	}
	if f.RelPath != "sub/doc.md" {
		t.Errorf("rel = %q", f.RelPath)
	}
}

func TestWalkSkipsHidden(t *testing.T) {
	dir := t.TempDir()
	// валидный файл
	if err := WriteAtomic(filepath.Join(dir, "a.md"), Frontmatter{KaitenID: 1, Updated: time.Now()}, "x", false); err != nil {
		t.Fatal(err)
	}
	// в скрытой папке — не должен попасть
	_ = os.MkdirAll(filepath.Join(dir, ".kaiten-sync"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".kaiten-sync", "state.json"), []byte("{}"), 0o644)
	// файл-точка
	_ = os.WriteFile(filepath.Join(dir, ".hidden.md"), []byte("---\nkaiten_id: 9\n---\nbody"), 0o644)

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Frontmatter.KaitenID != 1 {
		t.Errorf("ожидался 1 файл с id=1, получено %+v", files)
	}
}

// Фикс бага #21: после Render → SplitFrontmatter тело должно совпадать байт-в-байт.
// Иначе hash после round-trip не совпадает и движок зацикливается на LocalNewer.
func TestRenderSplitRoundtripHashStable(t *testing.T) {
	body := "v1\n"
	fm := Frontmatter{KaitenID: 1, Updated: time.Now().UTC()}
	out, err := Render(fm, body)
	if err != nil {
		t.Fatal(err)
	}
	_, gotBody, err := SplitFrontmatter(out)
	if err != nil {
		t.Fatal(err)
	}
	if HashBody(gotBody) != HashBody(body) {
		t.Errorf("hash расходится после round-trip: original=%q got=%q", body, gotBody)
	}
}

func TestRenderSplitRoundtripMultiline(t *testing.T) {
	cases := []string{
		"",
		"single line\n",
		"line1\nline2\n",
		"# heading\n\nparagraph\n",
		"with trailing spaces   \n",
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			fm := Frontmatter{KaitenID: 42, Updated: time.Now().UTC()}
			out, _ := Render(fm, body)
			_, gotBody, err := SplitFrontmatter(out)
			if err != nil {
				t.Fatal(err)
			}
			// hash должен совпадать с исходным body (с точностью до завершающего \n).
			if HashBody(strings.TrimRight(gotBody, "\n")) != HashBody(strings.TrimRight(body, "\n")) {
				t.Errorf("hash != after roundtrip: orig=%q got=%q", body, gotBody)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("a/b:c*d?e"); strings.ContainsAny(got, `/:*?`) {
		t.Errorf("санитайз не сработал: %q", got)
	}
	if Sanitize("   ") != "untitled" {
		t.Errorf("пустое имя должно стать untitled")
	}
}
