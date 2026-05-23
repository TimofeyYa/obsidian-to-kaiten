package syncengine

import (
	"testing"
	"time"
)

func TestStateRoundtrip(t *testing.T) {
	dir := t.TempDir()
	in := &State{
		Documents: map[string]DocState{
			"1": {Path: "a.md", KaitenUpdated: time.Now().UTC().Truncate(time.Second), ContentHash: "sha256:abc"},
		},
		LastSync: time.Now().UTC().Truncate(time.Second),
		SpaceID:  42,
	}
	if err := SaveState(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.SpaceID != 42 || out.Documents["1"].Path != "a.md" {
		t.Errorf("roundtrip потерял данные: %+v", out)
	}
}

func TestLoadState_Missing(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Documents == nil {
		t.Error("Documents должен быть инициализирован")
	}
}
