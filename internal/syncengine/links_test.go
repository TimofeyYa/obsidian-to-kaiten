package syncengine

import (
	"strings"
	"testing"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

func TestLinkResolver_KaitenToObsidian_MD(t *testing.T) {
	r := NewLinkResolver(
		[]kaiten.Document{{ID: 42, Title: "Project Plan"}},
		nil,
	)
	in := "see [the plan](/document/42) for details"
	out := r.KaitenToObsidian(in)
	if !strings.Contains(out, "[[Project Plan|the plan]]") {
		t.Errorf("ожидался wikilink: %q", out)
	}
}

func TestLinkResolver_KaitenToObsidian_HTML(t *testing.T) {
	r := NewLinkResolver(
		[]kaiten.Document{{ID: 42, Title: "Project Plan"}},
		nil,
	)
	in := `see <a href="/document/42">the plan</a>`
	out := r.KaitenToObsidian(in)
	if !strings.Contains(out, "[[Project Plan|the plan]]") {
		t.Errorf("ожидался wikilink: %q", out)
	}
}

func TestLinkResolver_KaitenToObsidian_UnknownPreserved(t *testing.T) {
	r := NewLinkResolver(nil, nil)
	in := "see [the plan](/document/999) for details"
	out := r.KaitenToObsidian(in)
	if !strings.Contains(out, "[the plan](/document/999)") {
		t.Errorf("неизвестная ссылка должна остаться: %q", out)
	}
}

func TestLinkResolver_ObsidianToKaiten(t *testing.T) {
	r := NewLinkResolver(
		[]kaiten.Document{{ID: 42, Title: "Project Plan"}},
		nil,
	)
	in := "see [[Project Plan]] for details"
	out := r.ObsidianToKaiten(in)
	if !strings.Contains(out, "[Project Plan](/document/42)") {
		t.Errorf("ожидалась md-ссылка: %q", out)
	}
}

func TestLinkResolver_ObsidianToKaiten_WithAlias(t *testing.T) {
	r := NewLinkResolver(
		[]kaiten.Document{{ID: 42, Title: "Project Plan"}},
		nil,
	)
	in := "see [[Project Plan|the plan]] for details"
	out := r.ObsidianToKaiten(in)
	if !strings.Contains(out, "[the plan](/document/42)") {
		t.Errorf("alias не сохранён: %q", out)
	}
}

func TestLinkResolver_ObsidianToKaiten_UnknownPreserved(t *testing.T) {
	r := NewLinkResolver(nil, nil)
	in := "see [[Unknown Doc]] for details"
	out := r.ObsidianToKaiten(in)
	if !strings.Contains(out, "[[Unknown Doc]]") {
		t.Errorf("неизвестный wikilink должен остаться: %q", out)
	}
}

func TestLinkResolver_UsesLocalTitle(t *testing.T) {
	// title в Obsidian = имя файла (без .md)
	locals := []obsidian.File{{
		Frontmatter: obsidian.Frontmatter{KaitenID: 7},
		RelPath:     "notes/My Doc.md",
	}}
	r := NewLinkResolver(nil, locals)
	in := "[[My Doc]]"
	out := r.ObsidianToKaiten(in)
	if !strings.Contains(out, "/document/7") {
		t.Errorf("резолв по локальному файлу не сработал: %q", out)
	}
}

func TestImageHandler_IsKaitenInternalURL(t *testing.T) {
	h := &ImageHandler{BaseURL: "https://my.kaiten.ru"}
	cases := map[string]bool{
		"https://my.kaiten.ru/api/x.png": true,
		"/api/latest/files/123":          true, // относительный
		"https://example.com/img.png":    false,
		"":                               false,
	}
	for in, want := range cases {
		if got := h.isKaitenInternalURL(in); got != want {
			t.Errorf("isKaitenInternalURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGuessFileName(t *testing.T) {
	cases := map[string]string{
		"https://x.kaiten.ru/files/photo.png":     "photo.png",
		"https://x.kaiten.ru/api/files/abc/1.jpg": "1.jpg",
	}
	for in, want := range cases {
		if got := guessFileName(in); got != want {
			t.Errorf("guessFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
