// Package syncengine реализует движок двусторонней синхронизации
// Kaiten ↔ Obsidian: state, diff, разрешение конфликтов, HTML↔Markdown.
package syncengine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// DocState — запись в state.json про один документ.
type DocState struct {
	Path          string    `json:"path"`
	KaitenUpdated time.Time `json:"kaiten_updated"`
	LocalMtime    time.Time `json:"local_mtime"`
	ContentHash   string    `json:"content_hash"`
}

// State — содержимое .kaiten-sync/state.json.
// Поля LastError / LastSuccess нужны для health-мониторинга (риск R-07):
// пользователь может быстро увидеть, давно ли синк успешно отработал.
type State struct {
	Documents   map[string]DocState `json:"documents"` // key — kaiten_doc_id как строка
	LastSync    time.Time           `json:"last_sync"`
	LastSuccess time.Time           `json:"last_success,omitempty"`
	LastError   string              `json:"last_error,omitempty"`
	SpaceID     int                 `json:"space_id"`
}

// StatePath — путь к state.json внутри vault.
func StatePath(vault string) string {
	return filepath.Join(vault, ".kaiten-sync", "state.json")
}

// LoadState читает state.json; если файла нет — возвращает пустой State.
func LoadState(vault string) (*State, error) {
	p := StatePath(vault)
	data, err := os.ReadFile(p) //nolint:gosec // путь задаётся явным образом
	if os.IsNotExist(err) {
		return &State{Documents: map[string]DocState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Documents == nil {
		s.Documents = map[string]DocState{}
	}
	return &s, nil
}

// SaveState пишет state.json атомарно с fsync (защита от R-02).
// 1) пишем во временный файл,
// 2) fsync содержимого,
// 3) rename (атомарен),
// 4) fsync каталога (чтобы rename долетел до диска).
// encoding/json сам сортирует ключи map[string]X лексикографически (Go 1.12+),
// поэтому файл детерминирован между запусками без изменений данных.
func SaveState(vault string, s *State) error {
	p := StatePath(vault)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if s.Documents == nil {
		s.Documents = map[string]DocState{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// fsync каталога — чтобы rename точно зафиксировался на диске.
	if df, dErr := os.Open(dir); dErr == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
}
