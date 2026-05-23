// E2E-тесты движка синхронизации с моком Kaiten API через httptest.
// Покрывают: initial pull, remote/local changes, conflict, dry-run,
// фильтрацию card-level, скрытые папки, HTML-конвертацию, отмену контекста,
// обработку точек в названиях.
package syncengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

// ---------- Mock Kaiten ----------

// mockKaiten — потокобезопасный мок Kaiten API.
type mockKaiten struct {
	mu       sync.Mutex
	docs     map[int]*kaiten.Document // documents by ID
	spaceID  int
	patchHit map[int]int // сколько раз PATCH вызывался для документа
}

func newMockKaiten(spaceID int, docs ...kaiten.Document) *mockKaiten {
	m := &mockKaiten{
		docs:     map[int]*kaiten.Document{},
		spaceID:  spaceID,
		patchHit: map[int]int{},
	}
	for i := range docs {
		d := docs[i]
		m.docs[d.ID] = &d
	}
	return m
}

func (m *mockKaiten) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (m *mockKaiten) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/latest/users/current":
		_ = json.NewEncoder(w).Encode(kaiten.User{ID: 1, Email: "t@e", FullName: "Tester"})

	case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/api/latest/spaces/%d/documents", m.spaceID):
		out := make([]kaiten.Document, 0, len(m.docs))
		for _, d := range m.docs {
			out = append(out, *d)
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/latest/documents/"):
		id := parseDocID(r.URL.Path)
		d, ok := m.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(d)

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/latest/documents/"):
		id := parseDocID(r.URL.Path)
		d, ok := m.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var p kaiten.PatchPayload
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &p)
		if p.Content != "" {
			d.Content = p.Content
		}
		if p.Type != "" {
			d.Type = p.Type
		}
		if p.Title != "" {
			d.Title = p.Title
		}
		d.Updated = time.Now().UTC()
		m.patchHit[id]++
		_ = json.NewEncoder(w).Encode(d)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// updateRemote — мутирует документ на стороне Kaiten (для имитации внешних правок).
func (m *mockKaiten) updateRemote(id int, content string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.docs[id]; ok {
		d.Content = content
		d.Updated = at
	}
}

// deleteRemote — удаляет документ на стороне Kaiten.
func (m *mockKaiten) deleteRemote(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.docs, id)
}

// getRemote — для assertion'ов в тестах.
func (m *mockKaiten) getRemote(id int) *kaiten.Document {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.docs[id]; ok {
		c := *d
		return &c
	}
	return nil
}

func (m *mockKaiten) patchCount(id int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.patchHit[id]
}

func parseDocID(path string) int {
	tail := strings.TrimPrefix(path, "/api/latest/documents/")
	var id int
	_, _ = fmt.Sscanf(tail, "%d", &id)
	return id
}

// ---------- Test helpers ----------

func newTestEngine(t *testing.T, vault string, srv *httptest.Server) *Engine {
	t.Helper()
	c := kaiten.New(srv.URL, "test-token")
	c.MaxRetries = 0     // в тестах не ждём
	c.SetRateLimit(1000) // отключаем rate-limit в тестах
	state, err := LoadState(vault)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Vault:   vault,
		BaseURL: srv.URL,
		Client:  c,
		State:   state,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func newDoc(id int, title, content string, updated time.Time) kaiten.Document {
	return kaiten.Document{
		ID:      id,
		Title:   title,
		Type:    "markdown",
		Content: content,
		Updated: updated,
	}
}

func readVault(t *testing.T, vault string) map[string]string {
	t.Helper()
	files, err := obsidian.Walk(vault)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.RelPath] = f.Body
	}
	return out
}

// ---------- Бизнес-сценарии ----------

func TestE2E_InitialPull(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42,
		newDoc(1, "Doc A", "# Hello A\n\nBody A\n", updated),
		newDoc(2, "Doc B", "# Hello B\n\nBody B\n", updated),
	)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	rep, err := eng.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Synced != 2 || rep.Errors != 0 {
		t.Errorf("report = %s", rep)
	}
	got := readVault(t, vault)
	if len(got) != 2 {
		t.Fatalf("ожидалось 2 файла, получено %d: %v", len(got), got)
	}
	if !strings.Contains(got["Doc A.md"], "Body A") {
		t.Errorf("Doc A.md потерял тело: %q", got["Doc A.md"])
	}
}

func TestE2E_RemoteChange(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1", updated))
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	if _, err := eng.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	// Меняем remote.
	mk.updateRemote(1, "v2-from-kaiten", updated.Add(time.Hour))
	// Повторный синк.
	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Synced != 1 || rep.Uploaded != 0 {
		t.Errorf("ожидался pull, получено: %s", rep)
	}
	got := readVault(t, vault)
	if !strings.Contains(got["Doc.md"], "v2-from-kaiten") {
		t.Errorf("файл не обновился: %q", got["Doc.md"])
	}
}

func TestE2E_LocalChange(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	if _, err := eng.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	// Эмулируем правку пользователя: перезаписываем body + двигаем mtime вперёд.
	abs := filepath.Join(vault, "Doc.md")
	f, err := obsidian.ReadFile(vault, abs)
	if err != nil {
		t.Fatal(err)
	}
	if err := obsidian.WriteAtomic(abs, f.Frontmatter, "v2-local-edit\n", false); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(abs, future, future)

	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Uploaded != 1 {
		t.Errorf("ожидался upload, получено: %s", rep)
	}
	if mk.patchCount(1) != 1 {
		t.Errorf("PATCH не пришёл (count=%d)", mk.patchCount(1))
	}
	if r := mk.getRemote(1); !strings.Contains(r.Content, "v2-local-edit") {
		t.Errorf("на сервере не обновилось: %q", r.Content)
	}
}

func TestE2E_Conflict(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)
	if _, err := eng.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}

	// Локально правим…
	abs := filepath.Join(vault, "Doc.md")
	f, _ := obsidian.ReadFile(vault, abs)
	_ = obsidian.WriteAtomic(abs, f.Frontmatter, "local-version\n", false)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(abs, future, future)
	// …и remote.
	mk.updateRemote(1, "remote-version\n", updated.Add(2*time.Hour))

	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Conflicts != 1 {
		t.Fatalf("ожидался конфликт, отчёт: %s", rep)
	}
	// Должен появиться .Doc.conflict-*.md (ведущая точка — фикс R-15).
	matches, _ := filepath.Glob(filepath.Join(vault, ".Doc.conflict-*.md"))
	if len(matches) != 1 {
		t.Fatalf("ожидался 1 conflict-файл, найдено: %v", matches)
	}
	conflictBody, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(conflictBody), "local-version") {
		t.Errorf("в conflict-файле нет локальной версии")
	}
	// Основной файл = remote.
	mainBody, _ := os.ReadFile(abs)
	if !strings.Contains(string(mainBody), "remote-version") {
		t.Errorf("основной файл должен быть remote-версией")
	}
}

func TestE2E_CardLevelDocsIgnored(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	card := 99
	cardDoc := newDoc(2, "On Card", "x", updated)
	cardDoc.CardID = &card
	mk := newMockKaiten(42,
		newDoc(1, "Space Doc", "ok", updated),
		cardDoc,
	)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)
	rep, err := eng.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Synced != 1 {
		t.Errorf("ожидался 1 synced (только space-level), получено: %s", rep)
	}
	if _, err := os.Stat(filepath.Join(vault, "On Card.md")); !os.IsNotExist(err) {
		t.Errorf("card-level документ попал в vault")
	}
}

func TestE2E_HiddenFolderSkipped(t *testing.T) {
	vault := t.TempDir()
	// Создаём .archive/note.md с валидным frontmatter — он НЕ должен учитываться.
	hiddenDir := filepath.Join(vault, ".archive")
	_ = os.MkdirAll(hiddenDir, 0o755)
	_ = obsidian.WriteAtomic(filepath.Join(hiddenDir, "note.md"),
		obsidian.Frontmatter{KaitenID: 777, Updated: time.Now()}, "hidden", false)

	mk := newMockKaiten(42)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)
	rep, err := eng.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	// .archive/note.md не должен попасть в Walk → 0 решений.
	if rep.NewLocal != 0 || rep.Synced != 0 {
		t.Errorf("скрытая папка попала в диф: %s", rep)
	}
}

func TestE2E_DryRun(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)
	eng.DryRun = true

	rep, err := eng.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Synced != 1 {
		t.Errorf("ожидался pull, отчёт: %s", rep)
	}
	// На диске ничего не появилось.
	if _, err := os.Stat(filepath.Join(vault, "Doc.md")); !os.IsNotExist(err) {
		t.Errorf("dry-run записал файл на диск")
	}
	// State.json тоже не появился.
	if _, err := os.Stat(filepath.Join(vault, ".kaiten-sync", "state.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run записал state.json")
	}
}

func TestE2E_Unchanged(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)

	// Первый прогон.
	eng1 := newTestEngine(t, vault, srv)
	if _, err := eng1.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	// Второй прогон без изменений — должен быть только Skipped.
	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Synced != 0 || rep.Uploaded != 0 {
		t.Errorf("ожидался unchanged: %s", rep)
	}
	if mk.patchCount(1) != 0 {
		t.Errorf("PATCH случился без причины")
	}
}

func TestE2E_DeletedRemote(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng1 := newTestEngine(t, vault, srv)
	if _, err := eng1.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	// Удаляем remote.
	mk.deleteRemote(1)

	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 {
		t.Errorf("ожидался DeletedRemote → Skipped: %s", rep)
	}
	// Локальный файл оставлен.
	if _, err := os.Stat(filepath.Join(vault, "Doc.md")); err != nil {
		t.Errorf("локальный файл не должен быть удалён")
	}
	// Из state ID 1 исчез.
	if _, ok := eng2.State.Documents["1"]; ok {
		t.Errorf("запись state не удалена для отсутствующего удалённо документа")
	}
}

func TestE2E_HTMLConversion(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	html := "<h1>Hello</h1><p>World &amp; <strong>bold</strong></p>"
	doc := kaiten.Document{ID: 1, Title: "HTMLDoc", Type: "html", Content: html, Updated: updated}
	mk := newMockKaiten(42, doc)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	if _, err := eng.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(vault, "HTMLDoc.md"))
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "kaiten_type: html") {
		t.Errorf("нет kaiten_type в frontmatter: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "# Hello") || !strings.Contains(bodyStr, "**bold**") {
		t.Errorf("HTML не конвертирован в Markdown: %s", bodyStr)
	}
	if strings.Contains(bodyStr, "<h1>") {
		t.Errorf("в body остались HTML-теги: %s", bodyStr)
	}
}

// ---------- Краевые случаи ----------

func TestPullRemote_ZeroUpdated(t *testing.T) {
	vault := t.TempDir()
	// Updated = zero time
	doc := kaiten.Document{ID: 1, Title: "Doc", Type: "markdown", Content: "x"}
	mk := newMockKaiten(42, doc)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	if _, err := eng.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	st, ok := eng.State.Documents["1"]
	if !ok {
		t.Fatal("state не записан")
	}
	if st.KaitenUpdated.IsZero() {
		t.Errorf("zero-time не подменён на текущее время")
	}
}

func TestTargetRelPath_HiddenTitle(t *testing.T) {
	vault := t.TempDir()
	eng := &Engine{Vault: vault, BaseURL: "https://x"}
	r := &kaiten.Document{ID: 1, Title: ".secret"}
	got := eng.targetRelPath(r, nil)
	if strings.HasPrefix(filepath.Base(got), ".") {
		t.Errorf("ведущая точка не убрана: %q", got)
	}
}

func TestTargetRelPath_NestedHiddenPath(t *testing.T) {
	vault := t.TempDir()
	eng := &Engine{Vault: vault, BaseURL: "https://x"}
	r := &kaiten.Document{ID: 1, Title: "Doc", Path: "folder/.archive/sub"}
	got := eng.targetRelPath(r, nil)
	if obsidian.IsHiddenPath(got) {
		t.Errorf("путь всё ещё считается скрытым: %q", got)
	}
}

func TestBuildDecisions_DeterministicOrder(t *testing.T) {
	updated := time.Now()
	remotes := []kaiten.Document{
		newDoc(3, "C", "x", updated),
		newDoc(1, "A", "x", updated),
		newDoc(2, "B", "x", updated),
	}
	st := &State{Documents: map[string]DocState{}}
	d1 := BuildDecisions(remotes, nil, st)
	d2 := BuildDecisions(remotes, nil, st)
	if len(d1) != 3 || len(d2) != 3 {
		t.Fatal("неожиданное число решений")
	}
	for i := range d1 {
		if d1[i].KaitenID != d2[i].KaitenID {
			t.Errorf("порядок различается: %v vs %v", d1, d2)
		}
	}
	// Должен быть отсортирован по ID.
	if d1[0].KaitenID != 1 || d1[1].KaitenID != 2 || d1[2].KaitenID != 3 {
		t.Errorf("порядок не отсортирован: %v", []int{d1[0].KaitenID, d1[1].KaitenID, d1[2].KaitenID})
	}
}

func TestSaveState_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	s1 := &State{
		Documents: map[string]DocState{
			"10": {Path: "a.md", ContentHash: "h10"},
			"2":  {Path: "b.md", ContentHash: "h2"},
			"3":  {Path: "c.md", ContentHash: "h3"},
		},
	}
	if err := SaveState(dir, s1); err != nil {
		t.Fatal(err)
	}
	data1, _ := os.ReadFile(StatePath(dir))
	// Перезаписываем без изменений → байты должны совпадать.
	if err := SaveState(dir, s1); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(StatePath(dir))
	if string(data1) != string(data2) {
		t.Errorf("две записи без изменений дали разный JSON")
	}
}

func TestEngine_ContextCancel(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now()
	// Много документов, чтобы успеть отменить между ними.
	docs := make([]kaiten.Document, 0, 20)
	for i := 1; i <= 20; i++ {
		docs = append(docs, newDoc(i, fmt.Sprintf("Doc%d", i), "x", updated))
	}
	mk := newMockKaiten(42, docs...)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменяем сразу

	_, err := eng.Run(ctx, 42)
	if err == nil {
		t.Fatal("ожидалась ошибка отмены контекста")
	}
}

func TestEngine_HasErrors(t *testing.T) {
	r := Report{Errors: 2}
	if !r.HasErrors() {
		t.Error("HasErrors должен быть true")
	}
	r2 := Report{}
	if r2.HasErrors() {
		t.Error("HasErrors должен быть false")
	}
}

// Повторный синк после успешного push не должен инициировать второй push
// (защита от циклической выгрузки — fix бага #6).
func TestE2E_NoFlappingAfterPush(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)

	eng1 := newTestEngine(t, vault, srv)
	if _, err := eng1.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	// Локально редактируем.
	abs := filepath.Join(vault, "Doc.md")
	f, _ := obsidian.ReadFile(vault, abs)
	_ = obsidian.WriteAtomic(abs, f.Frontmatter, "v2\n", false)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(abs, future, future)

	// Push.
	eng2 := newTestEngine(t, vault, srv)
	if _, err := eng2.Run(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	// Повторный синк без новых правок.
	eng3 := newTestEngine(t, vault, srv)
	rep, err := eng3.Run(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Uploaded != 0 || rep.Synced != 0 {
		t.Errorf("после успешного push повторный синк не должен ничего делать: %s", rep)
	}
	if mk.patchCount(1) != 1 {
		t.Errorf("PATCH должен был случиться ровно 1 раз, было %d", mk.patchCount(1))
	}
}
