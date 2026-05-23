package syncengine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

// Report — итог по проходу синхронизации.
type Report struct {
	Synced    int // Kaiten → Obsidian
	Uploaded  int // Obsidian → Kaiten
	Conflicts int
	NewLocal  int
	Errors    int
	Skipped   int
}

func (r Report) String() string {
	return fmt.Sprintf("synced=%d uploaded=%d conflicts=%d new_local=%d errors=%d skipped=%d",
		r.Synced, r.Uploaded, r.Conflicts, r.NewLocal, r.Errors, r.Skipped)
}

// HasErrors возвращает true, если в отчёте есть ошибки.
// Используется для не-нулевого exit code в cron-режиме.
func (r Report) HasErrors() bool { return r.Errors > 0 }

// Engine — координирует один проход синка.
type Engine struct {
	Vault   string
	BaseURL string
	Client  *kaiten.Client
	State   *State
	Logger  *slog.Logger
	DryRun  bool
}

// Run выполняет полный цикл синхронизации для заданного spaceID.
// Поддерживает отмену через ctx между этапами и документами.
//
// TODO(perf): сейчас N+1 запросов (ListDocuments + GetDocument для каждого).
// На больших пространствах (>500 документов) при rate-limit 5 req/sec это
// займёт ~100 секунд. Когда Kaiten добавит bulk-endpoint — переключиться.
func (e *Engine) Run(ctx context.Context, spaceID int) (rep Report, runErr error) {
	defer func() {
		// Записываем итог в state для healthcheck (риск R-07).
		now := time.Now().UTC()
		e.State.LastSync = now
		e.State.SpaceID = spaceID
		switch {
		case runErr != nil:
			e.State.LastError = runErr.Error()
		case rep.HasErrors():
			e.State.LastError = fmt.Sprintf("проход завершён с %d ошибками", rep.Errors)
		default:
			e.State.LastError = ""
			e.State.LastSuccess = now
		}
	}()

	// 1. Получаем документы пространства.
	remotes, err := e.Client.ListDocuments(ctx, spaceID)
	if err != nil {
		return rep, fmt.Errorf("список документов: %w", err)
	}

	// 2. Подтягиваем полный контент для каждого документа.
	full := make([]kaiten.Document, 0, len(remotes))
	for i := range remotes {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		d, derr := e.Client.GetDocument(ctx, remotes[i].ID)
		if derr != nil {
			e.Logger.Warn("не удалось получить документ", "id", remotes[i].ID, "err", derr)
			rep.Errors++
			continue
		}
		full = append(full, *d)
	}

	// 3. Обходим vault.
	locals, err := obsidian.Walk(e.Vault)
	if err != nil {
		return rep, fmt.Errorf("обход vault: %w", err)
	}

	// 4. Строим план.
	decisions := BuildDecisions(full, locals, e.State)

	// 5. Применяем.
	for _, dec := range decisions {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if aerr := e.apply(ctx, dec, &rep); aerr != nil {
			e.Logger.Error("ошибка применения", "doc_id", dec.KaitenID, "dir", dec.Direction, "err", aerr)
			rep.Errors++
		}
	}

	// 6. Сохраняем state (НЕ в DryRun).
	if !e.DryRun {
		// LastSync/LastSuccess проставляются в defer.
		if err := SaveState(e.Vault, e.State); err != nil {
			return rep, fmt.Errorf("сохранение state: %w", err)
		}
	}
	return rep, nil
}

// apply применяет одно решение и обновляет state.
func (e *Engine) apply(ctx context.Context, d Decision, rep *Report) error {
	idKey := strconv.Itoa(d.KaitenID)

	switch d.Direction {
	case Unchanged:
		rep.Skipped++
		return nil

	case RemoteNewer, NewRemote:
		return e.pullRemote(d, idKey, rep)

	case LocalNewer:
		return e.pushLocal(ctx, d, idKey, rep)

	case Conflict:
		return e.handleConflict(d, idKey, rep)

	case NewLocal:
		// MVP: создание новых документов в Kaiten не поддерживаем — только логируем.
		if d.Local != nil {
			e.Logger.Info("локальный документ без удалённого аналога — пропуск",
				"id", d.KaitenID, "path", d.Local.RelPath)
		}
		rep.NewLocal++
		return nil

	case DeletedRemote:
		if d.Local != nil {
			e.Logger.Warn("документ удалён в Kaiten — локальная копия оставлена",
				"id", d.KaitenID, "path", d.Local.RelPath)
		}
		if !e.DryRun {
			delete(e.State.Documents, idKey)
		}
		rep.Skipped++
		return nil
	}
	return nil
}

// pullRemote: скачиваем из Kaiten → пишем .md.
// В DryRun не модифицирует state (риск R-13).
func (e *Engine) pullRemote(d Decision, idKey string, rep *Report) error {
	r := d.Remote
	body := r.Content
	if strings.EqualFold(r.Type, "html") {
		m, err := HTMLToMarkdown(r.Content)
		if err != nil {
			return fmt.Errorf("html→md: %w", err)
		}
		body = m
	}

	relPath := e.targetRelPath(r, d.Local)

	// Валидация пути (риск R-04 — path traversal).
	abs, err := SafeJoin(e.Vault, relPath)
	if err != nil {
		return fmt.Errorf("небезопасный путь: %w", err)
	}

	// Защита от zero-time: Kaiten может вернуть updated=0001-01-01 для свежесозданного.
	updatedAt := r.Updated
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}

	fm := obsidian.Frontmatter{
		KaitenID:  r.ID,
		KaitenURL: fmt.Sprintf("%s/document/%d", strings.TrimRight(e.BaseURL, "/"), r.ID),
		Updated:   updatedAt,
		Type:      r.Type,
	}
	if err := obsidian.WriteAtomic(abs, fm, body, e.DryRun); err != nil {
		return err
	}
	if e.DryRun {
		e.Logger.Info("would pull", "id", r.ID, "path", relPath)
		rep.Synced++
		return nil
	}
	_ = os.Chtimes(abs, updatedAt, updatedAt)
	e.State.Documents[idKey] = DocState{
		Path:          relPath,
		KaitenUpdated: updatedAt,
		LocalMtime:    updatedAt,
		ContentHash:   obsidian.HashBody(body),
	}
	rep.Synced++
	e.Logger.Info("pulled", "id", r.ID, "path", relPath)
	return nil
}

// pushLocal: отправляем PATCH в Kaiten.
// После успешного PATCH синхронизируем локальный mtime с временем из ответа Kaiten,
// чтобы следующий синк не увидел расхождение (fix бага #6).
func (e *Engine) pushLocal(ctx context.Context, d Decision, idKey string, rep *Report) error {
	l := d.Local
	payload := kaiten.PatchPayload{
		Title:   strings.TrimSuffix(filepath.Base(l.RelPath), ".md"),
		Content: l.Body,
		Type:    "markdown",
	}
	// Если оригинал был HTML — конвертируем обратно.
	if d.Remote != nil && strings.EqualFold(d.Remote.Type, "html") {
		payload.Content = MarkdownToHTML(l.Body)
		payload.Type = "html"
	}
	if e.DryRun {
		e.Logger.Info("would push", "id", d.KaitenID, "path", l.RelPath)
		rep.Uploaded++
		return nil
	}
	updated, err := e.Client.PatchDocument(ctx, d.KaitenID, payload)
	if err != nil {
		return fmt.Errorf("patch: %w", err)
	}

	// Защита от zero-time в ответе.
	updatedAt := updated.Updated
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}

	// Синхронизируем локальный mtime с ответом — иначе на следующем синке
	// сервер вернёт более новое updated_at, и движок ошибочно решит, что
	// Kaiten новее, и перезапишет файл.
	_ = os.Chtimes(l.AbsPath, updatedAt, updatedAt)

	e.State.Documents[idKey] = DocState{
		Path:          l.RelPath,
		KaitenUpdated: updatedAt,
		LocalMtime:    updatedAt,
		ContentHash:   l.ContentHash(),
	}
	rep.Uploaded++
	e.Logger.Info("pushed", "id", d.KaitenID, "path", l.RelPath)
	return nil
}

// handleConflict: сохраняем локальную как .conflict-<ts>.md, накатываем Kaiten как основную.
//
// Фиксы:
//   - R-03: если backup не создан, НЕ применяем pullRemote (иначе локальная версия теряется).
//   - R-15: имя backup'а начинается с точки → попадает под IsHiddenPath и
//     не считается синхронизируемым файлом на следующем проходе.
func (e *Engine) handleConflict(d Decision, idKey string, rep *Report) error {
	rep.Conflicts++

	if d.Local == nil {
		// Конфликт без локального файла — невозможен, но защитимся.
		return e.pullRemote(d, idKey, rep)
	}

	if e.DryRun {
		e.Logger.Warn("conflict (dry-run) — backup не создан", "id", d.KaitenID)
		return e.pullRemote(d, idKey, rep)
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Dir(d.Local.AbsPath)
	base := strings.TrimSuffix(filepath.Base(d.Local.AbsPath), ".md")
	// Ведущая точка → файл не попадёт ни в Walk, ни в синхронизацию (R-15).
	conflictPath := filepath.Join(dir, "."+base+".conflict-"+ts+".md")

	data, rerr := os.ReadFile(d.Local.AbsPath) //nolint:gosec
	if rerr != nil {
		e.Logger.Error("не удалось прочитать локальный файл для backup",
			"err", rerr, "id", d.KaitenID, "path", d.Local.RelPath)
		return fmt.Errorf("backup read: %w", rerr)
	}
	if werr := writeFileSync(conflictPath, data, 0o600); werr != nil {
		// R-03: без backup'а ПУЛЛ НЕ ДЕЛАЕМ.
		e.Logger.Error("не удалось создать backup конфликта — pull пропущен",
			"err", werr, "id", d.KaitenID, "path", d.Local.RelPath)
		return fmt.Errorf("backup write: %w", werr)
	}
	e.Logger.Warn("conflict — локальная сохранена", "id", d.KaitenID, "backup", conflictPath)

	return e.pullRemote(d, idKey, rep)
}

// writeFileSync — атомарная запись с fsync (используется для conflict-backup'ов).
func writeFileSync(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
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
	return os.Rename(tmp, path)
}

// targetRelPath вычисляет относительный путь .md в vault для документа Kaiten.
// Если локальный файл уже есть — переиспользуем его путь, чтобы не дублировать.
//
// Защита от:
//   - R-04 (path traversal): любые `..` и абсолютные пути в r.Path игнорируются;
//   - баг #1: ведущие точки в любом сегменте пути экранируются (.archive → archive).
func (e *Engine) targetRelPath(r *kaiten.Document, local *obsidian.File) string {
	if local != nil {
		return local.RelPath
	}
	dir := ""
	if r.Path != "" {
		parts := strings.Split(r.Path, "/")
		clean := make([]string, 0, len(parts))
		for _, p := range parts {
			if p == "" || p == "." || p == ".." {
				continue
			}
			clean = append(clean, stripLeadingDots(obsidian.Sanitize(p)))
		}
		dir = filepath.Join(clean...)
	}
	name := stripLeadingDots(obsidian.Sanitize(r.Title)) + ".md"
	return filepath.ToSlash(filepath.Join(dir, name))
}

// stripLeadingDots убирает ведущие точки и пробелы.
// Используется, чтобы созданные нами файлы/папки не попадали под IsHiddenPath.
func stripLeadingDots(s string) string {
	out := strings.TrimLeft(s, ". ")
	if out == "" {
		return "untitled"
	}
	return out
}

// Preflight проверяет, что vault доступен на чтение и запись (риск R-10).
// Создаёт .kaiten-sync/.write-test, удаляет. Возвращает понятную ошибку,
// если что-то не так.
func Preflight(vault string) error {
	info, err := os.Stat(vault)
	if err != nil {
		return fmt.Errorf("vault %q недоступен: %w", vault, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("vault %q не каталог", vault)
	}
	dir := filepath.Join(vault, ".kaiten-sync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("не удалось создать %s: %w", dir, err)
	}
	testFile := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("vault не доступен для записи: %w", err)
	}
	return os.Remove(testFile)
}

// IsLikelyKaitenURL — мягкая проверка домена (риск R-11).
// Возвращает false для подозрительных доменов; вызывающий выводит warning,
// но не блокирует (есть on-prem инстансы).
func IsLikelyKaitenURL(baseURL string) bool {
	u := strings.ToLower(strings.TrimSpace(baseURL))
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return false
	}
	host := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	return strings.HasSuffix(host, ".kaiten.ru") || strings.HasSuffix(host, ".kaiten.app")
}

// EnsureContextDeadline — гарантирует, что у контекста есть дедлайн.
// Если deadline уже задан вызывающим — возвращает как есть.
// Иначе — добавляет default-таймаут (риск R-16).
func EnsureContextDeadline(ctx context.Context, defaultTimeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultTimeout)
}
