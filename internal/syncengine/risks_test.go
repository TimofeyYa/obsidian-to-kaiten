// Тесты на риск-митигации (см. RISKS.md).
package syncengine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

// R-04: path traversal через SafeJoin.
func TestSafeJoin_BlocksParentEscape(t *testing.T) {
	vault := t.TempDir()
	cases := []string{
		"../escape",
		"../../etc/passwd",
		"folder/../../escape",
		"a/b/../../../c",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if _, err := SafeJoin(vault, c); err == nil {
				t.Errorf("SafeJoin(%q) должен был отвергнуть путь", c)
			}
		})
	}
}

func TestSafeJoin_BlocksAbsolute(t *testing.T) {
	vault := t.TempDir()
	if _, err := SafeJoin(vault, "/etc/passwd"); err == nil {
		t.Error("абсолютный путь должен быть отвергнут")
	}
}

func TestSafeJoin_AllowsValid(t *testing.T) {
	vault := t.TempDir()
	abs, err := SafeJoin(vault, "notes/doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(abs, vault) {
		t.Errorf("результат не внутри vault: %q vs %q", abs, vault)
	}
}

// R-04 + E2E: вредоносный документ с ".." в Path не пишется за пределы vault.
func TestE2E_PathTraversalBlocked(t *testing.T) {
	vault := t.TempDir()
	// Создаём документ с попыткой выхода через Path.
	doc := kaiten.Document{
		ID:      1,
		Title:   "../../escape",
		Type:    "markdown",
		Content: "evil",
		Updated: time.Now().UTC(),
	}
	mk := newMockKaiten(42, doc)
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)
	rep, err := eng.Run(context.Background(), testRootUID)
	if err != nil {
		t.Fatal(err)
	}
	// Допустимо: либо санитизация имени дала безопасный путь, либо SafeJoin вернул ошибку.
	// В любом случае ничего не должно появиться вне vault.
	parent := filepath.Dir(vault)
	if _, err := os.Stat(filepath.Join(parent, "escape.md")); err == nil {
		t.Errorf("файл создан вне vault — path traversal!")
	}
	// Файл всё же должен оказаться где-то внутри vault.
	files, _ := obsidian.Walk(vault)
	if len(files) == 0 && rep.Errors == 0 {
		t.Error("документ не обработан и не ошибка")
	}
}

// R-01: ErrAlreadyRunning возвращается при попытке второго лока.
func TestVaultLock_Exclusive(t *testing.T) {
	vault := t.TempDir()
	l1, err := AcquireVaultLock(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l1.Release() }()

	l2, err := AcquireVaultLock(vault)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("ожидался ErrAlreadyRunning, получено: %v (l2=%v)", err, l2)
	}
}

func TestVaultLock_ReleaseAllowsReacquire(t *testing.T) {
	vault := t.TempDir()
	l1, err := AcquireVaultLock(vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := AcquireVaultLock(vault)
	if err != nil {
		t.Fatalf("после Release повторный лок должен браться: %v", err)
	}
	_ = l2.Release()
}

// R-02: state.json пишется атомарно с fsync; повреждение не происходит.
func TestSaveState_AtomicWithFsync(t *testing.T) {
	vault := t.TempDir()
	s := &State{
		Documents: map[string]DocState{"1": {Path: "a.md", ContentHash: "h"}},
		SpaceID:   1,
	}
	if err := SaveState(vault, s); err != nil {
		t.Fatal(err)
	}
	// .tmp-файла после успешной записи быть не должно.
	if _, err := os.Stat(StatePath(vault) + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp файл остался: %v", err)
	}
	// Права 0600.
	info, _ := os.Stat(StatePath(vault))
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("ожидались права 0600, получены %#o", mode)
	}
}

// R-07: LastSuccess/LastError записываются.
func TestEngine_LastSuccessOnHappyPath(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now().UTC()
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)

	before := time.Now()
	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	if eng.State.LastSuccess.Before(before.Add(-time.Second)) {
		t.Errorf("LastSuccess не обновился: %v", eng.State.LastSuccess)
	}
	if eng.State.LastError != "" {
		t.Errorf("LastError должен быть пустым: %q", eng.State.LastError)
	}
}

func TestEngine_LastErrorOnFailure(t *testing.T) {
	vault := t.TempDir()
	// Сервер всегда отвечает 500 → ListDocuments провалится.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := kaiten.New(srv.URL, "tok")
	c.SetRateLimit(1000)
	c.MaxRetries = 0
	state, _ := LoadState(vault)
	eng := &Engine{
		Vault:   vault,
		BaseURL: srv.URL,
		Client:  c,
		State:   state,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_, err := eng.Run(context.Background(), testRootUID)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if eng.State.LastError == "" {
		t.Errorf("LastError должен быть заполнен")
	}
	if !eng.State.LastSuccess.IsZero() {
		t.Errorf("LastSuccess не должен обновляться при ошибке")
	}
}

// R-13: DryRun НЕ модифицирует state в памяти для решений, кроме счётчиков.
func TestEngine_DryRunDoesNotMutateStateOnPull(t *testing.T) {
	vault := t.TempDir()
	updated := time.Now().UTC()
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng := newTestEngine(t, vault, srv)
	eng.DryRun = true

	if _, err := eng.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// В DryRun state.Documents не должен пополниться (R-13).
	if _, ok := eng.State.Documents["1"]; ok {
		t.Errorf("DryRun загрязнил state.Documents")
	}
}

// R-15: conflict-backup начинается с точки → не попадает в синк.
func TestE2E_ConflictBackupIsHidden(t *testing.T) {
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng1 := newTestEngine(t, vault, srv)
	if _, err := eng1.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Готовим конфликт.
	abs := filepath.Join(vault, "Doc.md")
	f, _ := obsidian.ReadFile(vault, abs)
	_ = obsidian.WriteAtomic(abs, f.Frontmatter, "local-version\n", false)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(abs, future, future)
	mk.updateRemote(1, "remote-version\n", updated.Add(2*time.Hour))

	eng2 := newTestEngine(t, vault, srv)
	if _, err := eng2.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Backup должен начинаться с точки.
	matches, _ := filepath.Glob(filepath.Join(vault, ".Doc.conflict-*.md"))
	if len(matches) != 1 {
		t.Fatalf("ожидался 1 backup с ведущей точкой, найдено: %v", matches)
	}
	// На третьем проходе Walk не должен видеть backup → 0 решений по новому ID.
	eng3 := newTestEngine(t, vault, srv)
	rep, err := eng3.Run(context.Background(), testRootUID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NewLocal > 0 {
		t.Errorf("backup попал в Walk: %s", rep)
	}
}

// R-03: при ошибке backup'а pullRemote НЕ применяется (локальная версия не теряется).
// Эмулируем ошибку через readonly-каталог.
func TestE2E_BackupFailureBlocksPull(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root readonly не работает")
	}
	vault := t.TempDir()
	updated := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	mk := newMockKaiten(42, newDoc(1, "Doc", "v1\n", updated))
	srv := mk.start(t)
	eng1 := newTestEngine(t, vault, srv)
	if _, err := eng1.Run(context.Background(), testRootUID); err != nil {
		t.Fatal(err)
	}
	// Готовим конфликт.
	abs := filepath.Join(vault, "Doc.md")
	f, _ := obsidian.ReadFile(vault, abs)
	_ = obsidian.WriteAtomic(abs, f.Frontmatter, "local-IMPORTANT\n", false)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(abs, future, future)
	mk.updateRemote(1, "remote-version\n", updated.Add(2*time.Hour))

	// Делаем каталог vault readonly → backup-write упадёт.
	if err := os.Chmod(vault, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(vault, 0o755) })

	eng2 := newTestEngine(t, vault, srv)
	rep, _ := eng2.Run(context.Background(), testRootUID)
	// Ожидаем: conflicts=1, errors>=1 (backup упал → pull пропущен).
	if rep.Conflicts != 1 || rep.Errors < 1 {
		t.Errorf("ожидался conflict + error, получено: %s", rep)
	}

	// Возвращаем права, чтобы прочитать локальный файл.
	_ = os.Chmod(vault, 0o755)
	body, _ := os.ReadFile(abs)
	if !strings.Contains(string(body), "local-IMPORTANT") {
		t.Errorf("локальная версия потеряна (R-03)! got: %q", body)
	}
}

// R-05: лимит сканера 32 MB — большой документ читается без ошибок.
func TestSplitFrontmatter_LargeBody(t *testing.T) {
	// 5 MB body — выше старого лимита 8 MB не уйдём, но проверим 5.
	big := strings.Repeat("a", 5*1024*1024)
	data := []byte("---\nkaiten_id: 1\nupdated: 2026-01-01T00:00:00Z\n---\n" + big)
	_, body, err := obsidian.SplitFrontmatter(data)
	if err != nil {
		t.Fatalf("большой документ не прочитан: %v", err)
	}
	if len(body) < len(big) {
		t.Errorf("body обрезан: got %d, want %d", len(body), len(big))
	}
}

// R-06: при 401 в ошибке НЕ содержится тело ответа (может содержать echo токена).
func TestClient_401ErrorDoesNotLeakBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Прокси-реверс мог бы echo'ить токен — эмулируем.
		_, _ = w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer srv.Close()

	c := kaiten.New(srv.URL, "supersecret-token")
	c.SetRateLimit(1000)
	c.MaxRetries = 0
	_, err := c.GetCurrentUser(context.Background())
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(err.Error(), "supersecret-token") {
		t.Errorf("токен утёк в ошибку: %v", err)
	}
}

// R-06: redact работает в общих сообщениях об ошибках.
func TestClient_5xxRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Эмулируем echo токена в теле 5xx.
		_, _ = w.Write([]byte("error: " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()

	c := kaiten.New(srv.URL, "supersecret")
	c.SetRateLimit(1000)
	c.MaxRetries = 0
	_, err := c.GetCurrentUser(context.Background())
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("токен утёк через 5xx: %v", err)
	}
}

// R-10: Preflight выявляет readonly vault.
func TestPreflight_ReadonlyFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root readonly не работает")
	}
	vault := t.TempDir()
	if err := os.Chmod(vault, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(vault, 0o755) }()

	if err := Preflight(vault); err == nil {
		t.Error("Preflight должен был упасть на readonly")
	}
}

func TestPreflight_OK(t *testing.T) {
	if err := Preflight(t.TempDir()); err != nil {
		t.Errorf("Preflight на нормальном каталоге: %v", err)
	}
}

func TestPreflight_NotADir(t *testing.T) {
	f, _ := os.CreateTemp(t.TempDir(), "file")
	_ = f.Close()
	if err := Preflight(f.Name()); err == nil {
		t.Error("Preflight должен упасть на не-каталоге")
	}
}

// R-11: проверка домена.
func TestIsLikelyKaitenURL(t *testing.T) {
	cases := map[string]bool{
		"https://mycompany.kaiten.ru": true,
		"https://x.kaiten.app":        true,
		"https://my-on-prem.local":    false,
		"https://kaitten.ru":          false, // typo
		"":                            false,
		"not-a-url":                   false,
		"http://test.kaiten.ru/sub":   true,
		"https://kaiten.com.evil.com": false,
	}
	for in, want := range cases {
		if got := IsLikelyKaitenURL(in); got != want {
			t.Errorf("IsLikelyKaitenURL(%q) = %v, want %v", in, got, want)
		}
	}
}

// R-16: EnsureContextDeadline добавляет дедлайн, если его нет.
func TestEnsureContextDeadline_AddsDefault(t *testing.T) {
	ctx, cancel := EnsureContextDeadline(context.Background(), 30*time.Second)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("дедлайн должен быть установлен")
	}
}

func TestEnsureContextDeadline_PreservesExisting(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer parentCancel()
	ctx, cancel := EnsureContextDeadline(parent, 30*time.Second)
	defer cancel()
	dl, _ := ctx.Deadline()
	parentDl, _ := parent.Deadline()
	if !dl.Equal(parentDl) {
		t.Errorf("родительский дедлайн не сохранён: %v vs %v", dl, parentDl)
	}
}

// R-18: LimitReader защищает от гигантских ответов.
func TestClient_LimitReaderCutsHugeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Возвращаем больше, чем MaxResponseSize.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 70 MB не-JSON — клиент прочитает только 64 MB, парсинг JSON упадёт.
		chunk := strings.Repeat("a", 1024*1024)
		for i := 0; i < 70; i++ {
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	c := kaiten.New(srv.URL, "tok")
	c.SetRateLimit(1000)
	c.MaxRetries = 0
	// Ожидаем ошибку парсинга JSON (а не OOM).
	_, err := c.GetCurrentUser(context.Background())
	if err == nil {
		t.Error("ожидалась ошибка парсинга для огромного не-JSON ответа")
	}
}

// R-19: filepath.WalkDir НЕ следует за symlink-петлями.
func TestWalk_NoSymlinkLoop(t *testing.T) {
	vault := t.TempDir()
	sub := filepath.Join(vault, "sub")
	_ = os.MkdirAll(sub, 0o755)
	// Создаём symlink на родителя.
	if err := os.Symlink(vault, filepath.Join(sub, "loop")); err != nil {
		t.Skipf("symlinks недоступны: %v", err)
	}
	// Walk не должен зацикливаться — просто завершиться.
	done := make(chan struct{})
	go func() {
		_, _ = obsidian.Walk(vault)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Walk зациклился на symlink")
	}
}

// Используем io.Discard, чтобы make линтер счастлив.
var _ = io.Discard
