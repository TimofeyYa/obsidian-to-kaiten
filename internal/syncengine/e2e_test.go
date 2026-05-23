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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

// ---------- Mock Kaiten ----------

// testRootUID — фиксированный UID корневой папки/пространства в тестах.
const testRootUID = "root-uid"

// mockKaiten — потокобезопасный мок Kaiten API с поддержкой
// tree-entities, document attachments и patch document.
type mockKaiten struct {
	mu           sync.Mutex
	docs         map[int]*kaiten.Document     // documents by ID
	attach       map[int][]*kaiten.Attachment // attachments by docID
	attachData   map[int][]byte               // content by attachment ID (для Download)
	nextAttachID int
	rootUID      string
	patchHit     map[int]int
	uploads      map[int]int // количество заливок attachments по docID
}

func newMockKaiten(_ int, docs ...kaiten.Document) *mockKaiten {
	m := &mockKaiten{
		docs:         map[int]*kaiten.Document{},
		attach:       map[int][]*kaiten.Attachment{},
		attachData:   map[int][]byte{},
		rootUID:      testRootUID,
		patchHit:     map[int]int{},
		uploads:      map[int]int{},
		nextAttachID: 1000,
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

	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && path == "/api/latest/users/current":
		_ = json.NewEncoder(w).Encode(kaiten.User{ID: 1, Email: "t@e", FullName: "Tester"})

	case r.Method == http.MethodGet && path == "/api/latest/tree-entities":
		// Простая модель: все документы висят на testRootUID.
		parent := r.URL.Query().Get("parent_entity_uid")
		if parent != m.rootUID {
			_ = json.NewEncoder(w).Encode([]kaiten.TreeEntity{})
			return
		}
		out := make([]kaiten.TreeEntity, 0, len(m.docs))
		parentUID := m.rootUID
		for _, d := range m.docs {
			out = append(out, kaiten.TreeEntity{
				UID:             fmt.Sprintf("doc-uid-%d", d.ID),
				ID:              d.ID,
				Title:           d.Title,
				EntityType:      kaiten.EntityTypeDocument,
				ParentEntityUID: &parentUID,
			})
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/files") &&
		strings.HasPrefix(path, "/api/latest/documents/"):
		id := parseDocIDFromFilesPath(path)
		list := m.attach[id]
		out := make([]kaiten.Attachment, 0, len(list))
		for _, a := range list {
			out = append(out, *a)
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPut && strings.HasSuffix(path, "/files") &&
		strings.HasPrefix(path, "/api/latest/documents/"):
		// Upload (multipart). Парсим форму.
		id := parseDocIDFromFilesPath(path)
		_ = r.ParseMultipartForm(64 << 20)
		f, fh, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer func() { _ = f.Close() }()
		data, _ := io.ReadAll(f)
		m.nextAttachID++
		a := &kaiten.Attachment{
			ID:   m.nextAttachID,
			Name: fh.Filename,
			Size: int64(len(data)),
			URL:  fmt.Sprintf("http://%s/api/latest/_attach_blob/%d", r.Host, m.nextAttachID),
		}
		m.attach[id] = append(m.attach[id], a)
		m.attachData[a.ID] = data
		m.uploads[id]++
		_ = json.NewEncoder(w).Encode(a)

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/latest/documents/") &&
		strings.Contains(path, "/files/"):
		docID, fileID := parseDocAndFileID(path)
		filtered := m.attach[docID][:0]
		for _, a := range m.attach[docID] {
			if a.ID != fileID {
				filtered = append(filtered, a)
			}
		}
		m.attach[docID] = filtered
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/latest/_attach_blob/"):
		var id int
		_, _ = fmt.Sscanf(strings.TrimPrefix(path, "/api/latest/_attach_blob/"), "%d", &id)
		if data, ok := m.attachData[id]; ok {
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/latest/documents/"):
		id := m.resolveDocID(path)
		d, ok := m.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(d)

	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/api/latest/documents/"):
		id := m.resolveDocID(path)
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

// resolveDocID — принимает и числовой ID, и UID вида "doc-uid-<n>".
func (m *mockKaiten) resolveDocID(path string) int {
	tail := strings.TrimPrefix(path, "/api/latest/documents/")
	// Отбрасываем возможный хвост /files или другие сегменты.
	if i := strings.Index(tail, "/"); i >= 0 {
		tail = tail[:i]
	}
	if n, err := strconv.Atoi(tail); err == nil {
		return n
	}
	// UID-формат "doc-uid-<n>".
	if strings.HasPrefix(tail, "doc-uid-") {
		var n int
		_, _ = fmt.Sscanf(strings.TrimPrefix(tail, "doc-uid-"), "%d", &n)
		return n
	}
	return 0
}

func parseDocIDFromFilesPath(path string) int {
	// /api/latest/documents/{id}/files
	parts := strings.Split(strings.TrimPrefix(path, "/api/latest/documents/"), "/")
	var id int
	if len(parts) > 0 {
		_, _ = fmt.Sscanf(parts[0], "%d", &id)
	}
	return id
}

func parseDocAndFileID(path string) (int, int) {
	// /api/latest/documents/{docID}/files/{fileID}
	tail := strings.TrimPrefix(path, "/api/latest/documents/")
	parts := strings.Split(tail, "/")
	var docID, fileID int
	if len(parts) >= 3 {
		_, _ = fmt.Sscanf(parts[0], "%d", &docID)
		_, _ = fmt.Sscanf(parts[2], "%d", &fileID)
	}
	return docID, fileID
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

	rep, err := eng.Run(context.Background(), testRootUID)
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

	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Меняем remote.
	mk.updateRemote(1, "v2-from-kaiten", updated.Add(time.Hour))
	// Повторный синк.
	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), testRootUID)
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

	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
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
	rep, err := eng2.Run(context.Background(), testRootUID)
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
	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
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
	rep, err := eng2.Run(context.Background(), testRootUID)
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
	rep, err := eng.Run(context.Background(), testRootUID)
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
	rep, err := eng.Run(context.Background(), testRootUID)
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

	rep, err := eng.Run(context.Background(), testRootUID)
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
	if _, err := eng1.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Второй прогон без изменений — должен быть только Skipped.
	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), testRootUID)
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
	if _, err := eng1.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Удаляем remote.
	mk.deleteRemote(1)

	eng2 := newTestEngine(t, vault, srv)
	rep, err := eng2.Run(context.Background(), testRootUID)
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

	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
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

	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
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

	_, err := eng.Run(ctx, testRootUID)
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
	if _, err := eng1.Run(context.Background(), testRootUID); err != nil {
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
	if _, err := eng2.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Повторный синк без новых правок.
	eng3 := newTestEngine(t, vault, srv)
	rep, err := eng3.Run(context.Background(), testRootUID)
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

// E2E: attachments скачиваются из Kaiten в <vault>/kaiten_files/<docID>/.
func TestE2E_AttachmentsDownload(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now().UTC()
	mk := newMockKaiten(0, newDoc(1, "Doc", "v1\n", updated))
	// Подкладываем remote attachment.
	mk.attach[1] = []*kaiten.Attachment{
		{ID: 5001, Name: "report.pdf", Size: 5, URL: ""},
	}
	mk.attachData[5001] = []byte("HELLO")
	srv := mk.start(t)
	// URL зависит от srv, проставляем после старта.
	mk.attach[1][0].URL = srv.URL + "/api/latest/_attach_blob/5001"

	eng := newTestEngine(t, vault, srv)
	rep, err := eng.Run(context.Background(), testRootUID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AttachmentsDown != 1 {
		t.Errorf("ожидался 1 attach_down, получено: %s", rep)
	}
	dst := filepath.Join(vault, AttachmentsDir, "1", "report.pdf")
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("файл не создан: %v", err)
	}
	if string(data) != "HELLO" {
		t.Errorf("содержимое файла повреждено: %q", data)
	}
}

// E2E: локальный файл в kaiten_files/<docID>/ заливается в Kaiten как attachment.
func TestE2E_AttachmentsUpload(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now().UTC()
	mk := newMockKaiten(0, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)

	// Кладём локальный файл ДО синка.
	docDir := filepath.Join(vault, AttachmentsDir, "1")
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docDir, "notes.txt"), []byte("local content"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := newTestEngine(t, vault, srv)
	rep, err := eng.Run(context.Background(), testRootUID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AttachmentsUp != 1 {
		t.Errorf("ожидался 1 attach_up, получено: %s", rep)
	}
	if mk.uploads[1] != 1 {
		t.Errorf("сервер получил %d upload'ов, ожидался 1", mk.uploads[1])
	}
	if len(mk.attach[1]) != 1 || mk.attach[1][0].Name != "notes.txt" {
		t.Errorf("attachment на сервере: %+v", mk.attach[1])
	}
}

// E2E: при конфликте (одинаковое имя, разный размер) локальный выигрывает —
// в Kaiten остаётся только локальная версия.
func TestE2E_AttachmentLocalWinsConflict(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now().UTC()
	mk := newMockKaiten(0, newDoc(1, "Doc", "v1\n", updated))
	// Remote: file.txt со старым содержимым.
	mk.attach[1] = []*kaiten.Attachment{
		{ID: 5001, Name: "file.txt", Size: 3, URL: ""},
	}
	mk.attachData[5001] = []byte("OLD")
	srv := mk.start(t)
	mk.attach[1][0].URL = srv.URL + "/api/latest/_attach_blob/5001"

	// Local: file.txt с новым содержимым (другой размер).
	docDir := filepath.Join(vault, AttachmentsDir, "1")
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docDir, "file.txt"), []byte("NEW LOCAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng := newTestEngine(t, vault, srv)
	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// На сервере должен остаться один attachment — с новым содержимым (локальный выиграл).
	if len(mk.attach[1]) != 1 {
		t.Fatalf("ожидался 1 attachment, найдено: %+v", mk.attach[1])
	}
	got := mk.attach[1][0]
	if got.Size != int64(len("NEW LOCAL")) {
		t.Errorf("размер на сервере = %d, ожидалось %d", got.Size, len("NEW LOCAL"))
	}
}

// E2E: рекурсивный обход папок (tree-entities) — структура отражается в vault.
func TestE2E_FolderHierarchyMirrored(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now().UTC()

	mk := &mockKaiten{
		docs:         map[int]*kaiten.Document{},
		attach:       map[int][]*kaiten.Attachment{},
		attachData:   map[int][]byte{},
		patchHit:     map[int]int{},
		uploads:      map[int]int{},
		nextAttachID: 1000,
		rootUID:      testRootUID,
	}
	// Документ в подпапке "Subfolder".
	doc := newDoc(1, "Nested Doc", "body\n", updated)
	mk.docs[1] = &doc

	// Кастомный handler: возвращаем дерево вручную.
	subUID := "sub-folder-uid"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mk.mu.Lock()
		defer mk.mu.Unlock()
		switch {
		case r.URL.Path == "/api/latest/tree-entities":
			parent := r.URL.Query().Get("parent_entity_uid")
			rootParent := mk.rootUID
			subParent := subUID
			switch parent {
			case mk.rootUID:
				_ = json.NewEncoder(w).Encode([]kaiten.TreeEntity{
					{UID: subUID, Title: "Subfolder", EntityType: kaiten.EntityTypeDocumentGroup, ParentEntityUID: &rootParent},
				})
			case subUID:
				_ = json.NewEncoder(w).Encode([]kaiten.TreeEntity{
					{UID: "doc-uid-1", ID: 1, Title: "Nested Doc", EntityType: kaiten.EntityTypeDocument, ParentEntityUID: &subParent},
				})
			default:
				_ = json.NewEncoder(w).Encode([]kaiten.TreeEntity{})
			}
		case strings.HasPrefix(r.URL.Path, "/api/latest/documents/") && strings.HasSuffix(r.URL.Path, "/files"):
			_ = json.NewEncoder(w).Encode([]kaiten.Attachment{})
		case strings.HasPrefix(r.URL.Path, "/api/latest/documents/"):
			_ = json.NewEncoder(w).Encode(mk.docs[1])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	eng := newTestEngine(t, vault, srv)
	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Проверяем, что файл лёг в Subfolder/.
	expected := filepath.Join(vault, "Subfolder", "Nested Doc.md")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("файл не создан в правильной папке: %v\nfiles in vault:", err)
		_ = filepath.Walk(vault, func(p string, _ os.FileInfo, _ error) error {
			t.Logf("  %s", p)
			return nil
		})
	}
}
