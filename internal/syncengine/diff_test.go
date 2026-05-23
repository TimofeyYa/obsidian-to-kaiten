package syncengine

import (
	"testing"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

func tt(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func TestDecide_Unchanged(t *testing.T) {
	body := "hello"
	r := &kaiten.Document{ID: 1, Updated: tt("2026-01-01T10:00:00Z"), Content: body}
	l := &obsidian.File{
		Frontmatter: obsidian.Frontmatter{KaitenID: 1, Updated: r.Updated},
		Body:        body,
		Mtime:       tt("2026-01-01T10:00:00Z"),
	}
	prev := &DocState{
		KaitenUpdated: r.Updated,
		LocalMtime:    l.Mtime,
		ContentHash:   obsidian.HashBody(body),
	}
	d := Decide(r, l, prev)
	if d.Direction != Unchanged {
		t.Errorf("got %s, want Unchanged", d.Direction)
	}
}

func TestDecide_RemoteNewer(t *testing.T) {
	r := &kaiten.Document{ID: 1, Updated: tt("2026-01-02T10:00:00Z")}
	l := &obsidian.File{
		Frontmatter: obsidian.Frontmatter{KaitenID: 1},
		Body:        "x",
		Mtime:       tt("2026-01-01T10:00:00Z"),
	}
	prev := &DocState{
		KaitenUpdated: tt("2026-01-01T10:00:00Z"),
		LocalMtime:    tt("2026-01-01T10:00:00Z"),
		ContentHash:   obsidian.HashBody("x"),
	}
	if d := Decide(r, l, prev); d.Direction != RemoteNewer {
		t.Errorf("got %s, want RemoteNewer", d.Direction)
	}
}

func TestDecide_LocalNewer(t *testing.T) {
	r := &kaiten.Document{ID: 1, Updated: tt("2026-01-01T10:00:00Z")}
	l := &obsidian.File{
		Frontmatter: obsidian.Frontmatter{KaitenID: 1},
		Body:        "changed",
		Mtime:       tt("2026-01-02T10:00:00Z"),
	}
	prev := &DocState{
		KaitenUpdated: tt("2026-01-01T10:00:00Z"),
		LocalMtime:    tt("2026-01-01T10:00:00Z"),
		ContentHash:   obsidian.HashBody("original"),
	}
	if d := Decide(r, l, prev); d.Direction != LocalNewer {
		t.Errorf("got %s, want LocalNewer", d.Direction)
	}
}

func TestDecide_Conflict(t *testing.T) {
	r := &kaiten.Document{ID: 1, Updated: tt("2026-01-02T10:00:00Z")}
	l := &obsidian.File{
		Frontmatter: obsidian.Frontmatter{KaitenID: 1},
		Body:        "changed-local",
		Mtime:       tt("2026-01-02T11:00:00Z"),
	}
	prev := &DocState{
		KaitenUpdated: tt("2026-01-01T10:00:00Z"),
		LocalMtime:    tt("2026-01-01T10:00:00Z"),
		ContentHash:   obsidian.HashBody("original"),
	}
	if d := Decide(r, l, prev); d.Direction != Conflict {
		t.Errorf("got %s, want Conflict", d.Direction)
	}
}

func TestDecide_NewRemote(t *testing.T) {
	r := &kaiten.Document{ID: 7, Updated: tt("2026-01-01T10:00:00Z")}
	if d := Decide(r, nil, nil); d.Direction != NewRemote {
		t.Errorf("got %s, want NewRemote", d.Direction)
	}
}

func TestDecide_DeletedRemote(t *testing.T) {
	l := &obsidian.File{Frontmatter: obsidian.Frontmatter{KaitenID: 7}}
	prev := &DocState{KaitenUpdated: tt("2026-01-01T10:00:00Z")}
	if d := Decide(nil, l, prev); d.Direction != DeletedRemote {
		t.Errorf("got %s, want DeletedRemote", d.Direction)
	}
}
