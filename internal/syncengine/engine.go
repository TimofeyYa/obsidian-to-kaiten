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
	Synced             int // Kaiten → Obsidian (документы)
	Uploaded           int // Obsidian → Kaiten (документы)
	Created            int // Новые документы созданы в Kaiten
	DeletedRemote      int // Документы удалены в Kaiten
	DeletedLocal       int // Локальные файлы удалены
	Renamed            int // Переименования/перемещения в vault
	Conflicts          int
	NewLocal           int
	Errors             int
	Skipped            int
	AttachmentsDown    int // Kaiten → Obsidian (вложения)
	AttachmentsUp      int // Obsidian → Kaiten (вложения)
	AttachmentsSkipped int
}

func (r Report) String() string {
	return fmt.Sprintf("synced=%d uploaded=%d created=%d deleted_remote=%d deleted_local=%d renamed=%d conflicts=%d new_local=%d errors=%d skipped=%d attach_down=%d attach_up=%d",
		r.Synced, r.Uploaded, r.Created, r.DeletedRemote, r.DeletedLocal, r.Renamed,
		r.Conflicts, r.NewLocal, r.Errors, r.Skipped, r.AttachmentsDown, r.AttachmentsUp)
}

// HasErrors возвращает true, если в отчёте есть ошибки.
// Используется для не-нулевого exit code в cron-режиме.
func (r Report) HasErrors() bool { return r.Errors > 0 }

// Engine — координирует один проход синка.
type Engine struct {
	Vault   string
	BaseURL string
	RootUID string // используется как parent_entity_uid при создании документов
	Client  *kaiten.Client
	State   *State
	Logger  *slog.Logger
	DryRun  bool

	// CreateRemote — создавать в Kaiten документы, созданные локально в Obsidian (#5).
	CreateRemote bool
	// DeleteOrphans — удалять в Kaiten документы, исчезнувшие из vault (#4).
	DeleteOrphans bool
	// DeleteLocalOrphans — удалять локальные файлы для удалённых в Kaiten (#4).
	DeleteLocalOrphans bool

	// docRelHint — релативная директория в vault для текущего документа
	// (вычисляется из иерархии tree-entities). Передаётся в targetRelPath.
	docRelHint string

	// resolver и imgHandler — контексты для трансляции ссылок и inline-картинок.
	resolver   *LinkResolver
	imgHandler *ImageHandler
}

// AttachmentsDir — имя подпапки в vault, куда скачиваются вложения Kaiten.
const AttachmentsDir = "kaiten_files"

// Run выполняет полный цикл синхронизации для заданного корневого UID (папки или пространства).
// Рекурсивно обходит дерево через GET /tree-entities, собирает все document'ы
// и синхронизирует их (+ вложения) в vault.
//
// TODO(perf): сейчас N+1 запросов на каждый документ.
func (e *Engine) Run(ctx context.Context, rootUID string) (rep Report, runErr error) {
	defer func() {
		// Записываем итог в state для healthcheck (риск R-07).
		now := time.Now().UTC()
		e.State.LastSync = now
		e.State.RootUID = rootUID
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

	// 1. Рекурсивно обходим дерево от rootUID.
	entities, err := e.Client.WalkTree(ctx, rootUID)
	if err != nil {
		return rep, fmt.Errorf("обход дерева Kaiten: %w", err)
	}
	e.Logger.Info("дерево Kaiten получено", "root", rootUID, "entities", len(entities))

	// 2. Строим карту UID→path для папок и собираем документы.
	folderPath := buildFolderPaths(rootUID, entities)
	var docEntities []kaiten.TreeEntity
	for _, en := range entities {
		if en.IsDocument() {
			docEntities = append(docEntities, en)
		}
	}

	// 3. Подтягиваем полный контент каждого документа + вычисляем relPath в vault.
	full := make([]kaiten.Document, 0, len(docEntities))
	docRelPath := make(map[int]string, len(docEntities))
	for _, en := range docEntities {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		d, derr := e.Client.GetDocument(ctx, en.ID)
		if derr != nil {
			e.Logger.Warn("не удалось получить документ", "id", en.ID, "err", derr)
			rep.Errors++
			continue
		}
		if !d.IsSpaceLevel() {
			// Карточные документы игнорируем (по ТЗ).
			continue
		}
		full = append(full, *d)
		if en.ParentEntityUID != nil {
			docRelPath[d.ID] = folderPath[*en.ParentEntityUID]
		}
	}

	// 4. Обходим vault.
	locals, err := obsidian.Walk(e.Vault)
	if err != nil {
		return rep, fmt.Errorf("обход vault: %w", err)
	}

	// 5. Строим план.
	decisions := BuildDecisions(full, locals, e.State)

	// 5a. Контексты для трансляции ссылок и inline-картинок.
	e.resolver = NewLinkResolver(full, locals)
	e.imgHandler = &ImageHandler{
		Vault:   e.Vault,
		BaseURL: e.BaseURL,
		Client:  e.Client,
		Logger:  e.Logger,
		DryRun:  e.DryRun,
	}

	// 6. Применяем.
	for _, dec := range decisions {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if dec.Remote != nil {
			e.docRelHint = docRelPath[dec.Remote.ID]
		} else {
			e.docRelHint = ""
		}
		if aerr := e.apply(ctx, dec, &rep); aerr != nil {
			e.Logger.Error("ошибка применения", "doc_id", dec.KaitenID, "dir", dec.Direction, "err", aerr)
			rep.Errors++
		}
		// Синхронизируем вложения для этого документа (если есть remote).
		if dec.Remote != nil && dec.Direction != NewLocal && dec.Direction != DeletedRemote {
			if aerr := e.syncAttachments(ctx, dec.Remote.ID, &rep); aerr != nil {
				e.Logger.Warn("ошибка синка вложений", "doc_id", dec.KaitenID, "err", aerr)
				rep.Errors++
			}
		}
	}

	// 6b. DeleteOrphans (#4): для каждой записи state, которой нет ни в remotes,
	// ни в locals — это файл, удалённый пользователем в Obsidian.
	if e.DeleteOrphans {
		remoteIDs := map[int]bool{}
		for _, d := range full {
			remoteIDs[d.ID] = true
		}
		localIDs := map[int]bool{}
		for _, l := range locals {
			localIDs[l.Frontmatter.KaitenID] = true
		}
		for key := range e.State.Documents {
			id, _ := strconv.Atoi(key)
			if localIDs[id] || !remoteIDs[id] {
				continue
			}
			// Существует в Kaiten, но нет в vault — удаляем.
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			if e.DryRun {
				e.Logger.Info("would delete remote orphan", "id", id)
				rep.DeletedRemote++
				continue
			}
			// Ищем UID в remotes для правильного DELETE.
			deleteKey := key
			for _, doc := range full {
				if doc.ID == id && doc.UID != "" {
					deleteKey = doc.UID
					break
				}
			}
			if derr := e.Client.DeleteDocument(ctx, deleteKey); derr != nil {
				e.Logger.Warn("не удалось удалить remote orphan", "id", id, "err", derr)
				rep.Errors++
				continue
			}
			delete(e.State.Documents, key)
			rep.DeletedRemote++
			e.Logger.Warn("удалён из Kaiten (файл удалён локально)", "id", id)
		}
	}

	// 7. Обработка новых «голых» файлов (#5): пользователь создал .md в Obsidian.
	if e.CreateRemote {
		untracked, uerr := obsidian.WalkUntracked(e.Vault)
		if uerr != nil {
			e.Logger.Warn("поиск untracked-файлов", "err", uerr)
		} else {
			for _, abs := range untracked {
				if err := ctx.Err(); err != nil {
					return rep, err
				}
				if err := e.createFromUntracked(ctx, abs, &rep); err != nil {
					e.Logger.Warn("не удалось создать в Kaiten", "path", abs, "err", err)
					rep.Errors++
				}
			}
		}
	}

	// 8. Сохраняем state (НЕ в DryRun).
	if !e.DryRun {
		if err := SaveState(e.Vault, e.State); err != nil {
			return rep, fmt.Errorf("сохранение state: %w", err)
		}
	}
	return rep, nil
}

// createFromUntracked — работает с .md файлом без kaiten_id: читаем сырой контент,
// извлекаем title из имени файла, POST в Kaiten, и перезаписываем файл с frontmatter.
func (e *Engine) createFromUntracked(ctx context.Context, abs string, rep *Report) error {
	data, err := os.ReadFile(abs) //nolint:gosec
	if err != nil {
		return err
	}
	bodyStr := string(data)
	// Проверяем на всякий случай: может быть битый frontmatter, не перезаписываем.
	if strings.HasPrefix(strings.TrimSpace(bodyStr), "---") {
		e.Logger.Debug("файл имеет frontmatter, но без kaiten_id — пропуск", "path", abs)
		return nil
	}
	rel, _ := filepath.Rel(e.Vault, abs)
	title := strings.TrimSuffix(filepath.Base(rel), ".md")
	if e.DryRun {
		e.Logger.Info("would create from untracked", "path", rel, "title", title)
		rep.Created++
		return nil
	}
	body := bodyStr
	if e.resolver != nil {
		body = e.resolver.ObsidianToKaiten(body)
	}
	var parent *string
	if e.RootUID != "" {
		root := e.RootUID
		parent = &root
	}
	created, cerr := e.Client.CreateDocument(ctx, kaiten.CreatePayload{
		Title:           title,
		Content:         body,
		Type:            "markdown",
		ParentEntityUID: parent,
	})
	if cerr != nil {
		return fmt.Errorf("create from untracked: %w", cerr)
	}
	// Пишем frontmatter в локальный файл.
	fm := obsidian.Frontmatter{
		KaitenID:  created.ID,
		KaitenURL: fmt.Sprintf("%s/document/%d", strings.TrimRight(e.BaseURL, "/"), created.ID),
		Updated:   created.Updated,
	}
	if err := obsidian.WriteAtomic(abs, fm, bodyStr, false); err != nil {
		return err
	}
	e.State.Documents[strconv.Itoa(created.ID)] = DocState{
		Path:          filepath.ToSlash(rel),
		KaitenUpdated: created.Updated,
		LocalMtime:    created.Updated,
		ContentHash:   obsidian.HashBody(bodyStr),
	}
	rep.Created++
	e.Logger.Info("created from untracked", "id", created.ID, "path", rel)
	return nil
}

// buildFolderPaths — для каждой папки/пространства вычисляет относительный путь
// от rootUID, чтобы отразить иерархию в файловой системе vault.
// rootUID сам не входит в путь (его содержимое ложится в корень vault).
func buildFolderPaths(rootUID string, entities []kaiten.TreeEntity) map[string]string {
	byUID := make(map[string]kaiten.TreeEntity, len(entities))
	for _, en := range entities {
		byUID[en.UID] = en
	}
	paths := make(map[string]string, len(entities))
	paths[rootUID] = ""
	var resolve func(uid string) string
	resolve = func(uid string) string {
		if p, ok := paths[uid]; ok {
			return p
		}
		en, ok := byUID[uid]
		if !ok {
			return ""
		}
		name := stripLeadingDots(obsidian.Sanitize(en.Title))
		if en.ParentEntityUID == nil {
			paths[uid] = name
			return name
		}
		parent := resolve(*en.ParentEntityUID)
		var p string
		if parent == "" {
			p = name
		} else {
			p = parent + "/" + name
		}
		paths[uid] = p
		return p
	}
	for _, en := range entities {
		if en.IsFolder() || en.IsSpace() {
			resolve(en.UID)
		}
	}
	return paths
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
		return e.handleNewLocal(ctx, d, idKey, rep)

	case DeletedRemote:
		return e.handleDeletedRemote(d, idKey, rep)
	}
	return nil
}

// handleNewLocal: локальный файл без удалённого аналога.
// Если CreateRemote=true — создаём новый документ в Kaiten.
// Примечание: файл в vault уже имеет во фронтматтере kaiten_id, но в Kaiten
// такого ID нет. Это бывает, когда ранее созданный док был удалён, но state
// не в курсе. Для полноценного создания из пустого файла нужен файл без kaiten_id —
// такой в obsidian.Walk не попадает (там фильтр по kaiten_id). Поэтому отдельная
// процедура CreateNewDocuments работает с файлами «без frontmatter» отдельно (см. Run).
func (e *Engine) handleNewLocal(ctx context.Context, d Decision, idKey string, rep *Report) error {
	if d.Local == nil {
		rep.NewLocal++
		return nil
	}
	if !e.CreateRemote {
		e.Logger.Info("локальный файл без удалённого аналога (без --create-remote)",
			"id", d.KaitenID, "path", d.Local.RelPath)
		rep.NewLocal++
		return nil
	}
	// Для этого case'а в frontmatter уже есть kaiten_id (иначе файл не попал бы в Walk).
	// Предполагаем, что документ был удалён в Kaiten, но файл жив. Пересоздаём.
	return e.createInKaiten(ctx, d.Local, idKey, rep)
}

// createInKaiten — POST /documents и обновление frontmatter локального файла с новым kaiten_id.
func (e *Engine) createInKaiten(ctx context.Context, l *obsidian.File, oldIDKey string, rep *Report) error {
	title := strings.TrimSuffix(filepath.Base(l.RelPath), ".md")
	body := l.Body
	if e.resolver != nil {
		body = e.resolver.ObsidianToKaiten(body)
	}
	if e.DryRun {
		e.Logger.Info("would create", "title", title, "path", l.RelPath)
		rep.Created++
		return nil
	}
	var parent *string
	if e.RootUID != "" {
		root := e.RootUID
		parent = &root
	}
	created, err := e.Client.CreateDocument(ctx, kaiten.CreatePayload{
		Title:           title,
		Content:         body,
		Type:            "markdown",
		ParentEntityUID: parent,
	})
	if err != nil {
		return fmt.Errorf("create document: %w", err)
	}
	// Обновляем frontmatter локального файла с новым ID.
	l.Frontmatter.KaitenID = created.ID
	l.Frontmatter.KaitenURL = fmt.Sprintf("%s/document/%d", strings.TrimRight(e.BaseURL, "/"), created.ID)
	l.Frontmatter.Updated = created.Updated
	if err := obsidian.WriteAtomic(l.AbsPath, l.Frontmatter, l.Body, false); err != nil {
		return err
	}
	if oldIDKey != "0" {
		delete(e.State.Documents, oldIDKey)
	}
	e.State.Documents[strconv.Itoa(created.ID)] = DocState{
		Path:          l.RelPath,
		KaitenUpdated: created.Updated,
		LocalMtime:    created.Updated,
		ContentHash:   obsidian.HashBody(l.Body),
	}
	rep.Created++
	e.Logger.Info("created", "id", created.ID, "path", l.RelPath)
	return nil
}

// handleDeletedRemote: документ пропал в Kaiten.
// Если DeleteLocalOrphans=true — удаляем локальный файл.
// Иначе оставляем (поведение по умолчанию из ТЗ: «локальная версия оставлена»).
func (e *Engine) handleDeletedRemote(d Decision, idKey string, rep *Report) error {
	if d.Local == nil {
		delete(e.State.Documents, idKey)
		rep.Skipped++
		return nil
	}
	if !e.DeleteLocalOrphans {
		e.Logger.Warn("документ удалён в Kaiten — локальная копия оставлена",
			"id", d.KaitenID, "path", d.Local.RelPath)
		if !e.DryRun {
			delete(e.State.Documents, idKey)
		}
		rep.Skipped++
		return nil
	}
	if e.DryRun {
		e.Logger.Info("would delete local orphan", "path", d.Local.RelPath)
		rep.DeletedLocal++
		return nil
	}
	if err := os.Remove(d.Local.AbsPath); err != nil {
		return fmt.Errorf("delete local orphan: %w", err)
	}
	delete(e.State.Documents, idKey)
	rep.DeletedLocal++
	e.Logger.Warn("локальный осиротевший файл удалён", "path", d.Local.RelPath)
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
	// Скачиваем inline-картинки и переписываем ссылки.
	if e.imgHandler != nil {
		body = e.imgHandler.RewriteForObsidian(context.Background(), r.ID, body)
	}
	// Переводим ссылки Kaiten в wikilinks Obsidian.
	if e.resolver != nil {
		body = e.resolver.KaitenToObsidian(body)
	}

	// Вычисляем желаемый путь НЕЗАВИСИМО от текущего расположения локального файла,
	// чтобы поддержать переименования/перемещения в Kaiten (#7, #8).
	desiredRel := e.targetRelPath(r, nil)
	relPath := desiredRel

	// Если в vault уже есть файл с этим kaiten_id, но по другому пути — переименование.
	if d.Local != nil && d.Local.RelPath != desiredRel && !e.DryRun {
		oldAbs := d.Local.AbsPath
		newAbs, sjErr := SafeJoin(e.Vault, desiredRel)
		if sjErr != nil {
			return fmt.Errorf("небезопасный путь при переименовании: %w", sjErr)
		}
		if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
			return fmt.Errorf("mkdir для переименования: %w", err)
		}
		if err := os.Rename(oldAbs, newAbs); err != nil {
			e.Logger.Warn("не удалось переименовать", "old", oldAbs, "new", newAbs, "err", err)
		} else {
			rep.Renamed++
			e.Logger.Info("renamed", "from", d.Local.RelPath, "to", desiredRel)
			// Обновляем ссылку на File (для последующего WriteAtomic).
			d.Local.AbsPath = newAbs
			d.Local.RelPath = desiredRel
		}
	}

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
	body := l.Body
	// Конвертируем wikilinks [[Title]] обратно в /document/<id>.
	if e.resolver != nil {
		body = e.resolver.ObsidianToKaiten(body)
	}
	// Загружаем inline-картинки из ![[...]] и переписываем на Kaiten URL.
	if e.imgHandler != nil {
		body = e.imgHandler.RewriteForKaiten(ctx, d.KaitenID, body)
	}
	payload := kaiten.PatchPayload{
		Title:   strings.TrimSuffix(filepath.Base(l.RelPath), ".md"),
		Content: body,
		Type:    "markdown",
	}
	// Если оригинал был HTML — рендерим в HTML через goldmark.
	if d.Remote != nil && strings.EqualFold(d.Remote.Type, "html") {
		payload.Content = MarkdownToHTML(body)
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
// Приоритет источников для директории:
//  1. e.docRelHint (из рекурсивного обхода tree-entities) — основной источник;
//  2. r.Path (legacy, на случай ручных вызовов без обхода).
//
// Защита от:
//   - R-04 (path traversal): любые `..` и абсолютные пути игнорируются;
//   - баг #1: ведущие точки в любом сегменте пути экранируются (.archive → archive).
func (e *Engine) targetRelPath(r *kaiten.Document, local *obsidian.File) string {
	if local != nil {
		return local.RelPath
	}
	dir := e.docRelHint
	if dir == "" && r.Path != "" {
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

// syncAttachments — синхронизирует вложения одного документа.
// Правила (по ТЗ):
//   - файлы Kaiten скачиваются в <vault>/kaiten_files/<docID>/<name>;
//   - все локальные файлы в этой папке заливаются в Kaiten как attachments документа;
//   - при конфликте локальный выигрывает (заливаем в Kaiten поверх).
//
// В DryRun только логируем план.
func (e *Engine) syncAttachments(ctx context.Context, docID int, rep *Report) error {
	remotes, err := e.Client.ListDocumentAttachments(ctx, docID)
	if err != nil {
		// Эндпоинт может отсутствовать в старых инстансах (404). Не фатально — пропускаем.
		if strings.Contains(err.Error(), " 404 ") {
			e.Logger.Debug("вложения недоступны для этого документа (404)", "doc_id", docID)
			return nil
		}
		return fmt.Errorf("list attachments: %w", err)
	}

	docDir := filepath.Join(e.Vault, AttachmentsDir, strconv.Itoa(docID))
	if !e.DryRun {
		if err := os.MkdirAll(docDir, 0o755); err != nil {
			return fmt.Errorf("mkdir attachments: %w", err)
		}
	}

	// Remote-файлы: имя → attachment.
	remoteByName := make(map[string]kaiten.Attachment, len(remotes))
	for _, a := range remotes {
		remoteByName[obsidian.Sanitize(a.Name)] = a
	}

	// Local-файлы: имя → abspath.
	localByName := map[string]string{}
	if entries, err := os.ReadDir(docDir); err == nil {
		for _, en := range entries {
			if en.IsDir() || strings.HasPrefix(en.Name(), ".") {
				continue
			}
			localByName[en.Name()] = filepath.Join(docDir, en.Name())
		}
	}

	// 1) Локальные выигрывают (ТЗ). Для каждого локального — заливаем, если:
	//    - его нет в Kaiten;
	//    - или размер локального отличается от размера в Kaiten (были правки).
	for name, absPath := range localByName {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, _ := os.Stat(absPath)
		remote, hasRemote := remoteByName[name]
		if hasRemote && info != nil && info.Size() == remote.Size {
			rep.AttachmentsSkipped++
			continue
		}
		if e.DryRun {
			e.Logger.Info("would upload attachment", "doc_id", docID, "name", name)
			rep.AttachmentsUp++
			continue
		}
		f, oerr := os.Open(absPath) //nolint:gosec
		if oerr != nil {
			e.Logger.Warn("ошибка open attachment", "path", absPath, "err", oerr)
			rep.Errors++
			continue
		}
		// Если в Kaiten уже есть файл с тем же именем — сначала удаляем (локальный побеждает).
		if hasRemote {
			if derr := e.Client.DeleteDocumentAttachment(ctx, docID, remote.ID); derr != nil {
				e.Logger.Warn("не удалось удалить старый attachment", "id", remote.ID, "err", derr)
			}
		}
		_, uerr := e.Client.UploadDocumentAttachment(ctx, docID, name, f)
		_ = f.Close()
		if uerr != nil {
			e.Logger.Warn("ошибка upload attachment", "doc_id", docID, "name", name, "err", uerr)
			rep.Errors++
			continue
		}
		rep.AttachmentsUp++
		e.Logger.Info("attachment uploaded", "doc_id", docID, "name", name)
	}

	// 2) Скачиваем из Kaiten все вложения, которых нет локально.
	for name, a := range remoteByName {
		if _, ok := localByName[name]; ok {
			continue // локальный выиграл выше или совпал
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(docDir, name)
		if e.DryRun {
			e.Logger.Info("would download attachment", "doc_id", docID, "name", name)
			rep.AttachmentsDown++
			continue
		}
		if a.URL == "" {
			e.Logger.Warn("attachment без URL", "doc_id", docID, "name", name)
			continue
		}
		tmp := dst + ".part"
		f, ferr := os.Create(tmp) //nolint:gosec
		if ferr != nil {
			e.Logger.Warn("create attachment file", "path", tmp, "err", ferr)
			rep.Errors++
			continue
		}
		_, derr := e.Client.DownloadAttachment(ctx, a.URL, f)
		_ = f.Close()
		if derr != nil {
			_ = os.Remove(tmp)
			e.Logger.Warn("download attachment", "doc_id", docID, "name", name, "err", derr)
			rep.Errors++
			continue
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			rep.Errors++
			continue
		}
		rep.AttachmentsDown++
		e.Logger.Info("attachment downloaded", "doc_id", docID, "name", name)
	}

	return nil
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
