package syncengine

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/timofeyblog/kaiten-obsidian-sync/internal/kaiten"
	"github.com/timofeyblog/kaiten-obsidian-sync/internal/obsidian"
)

// Регулярки для распознавания ссылок Kaiten в HTML и Markdown.
var (
	// Markdown-ссылка: [text](/document/123) или (/document/uid-abc)
	reKaitenMDDocLink = regexp.MustCompile(`\[([^\]]+)\]\(/?document/([a-zA-Z0-9_-]+)\)`)
	// HTML-ссылка: <a href="/document/123">text</a>
	reKaitenHTMLDocLink = regexp.MustCompile(`<a\s+[^>]*href="/?document/([a-zA-Z0-9_-]+)"[^>]*>([^<]*)</a>`)
	// Markdown-картинка: ![alt](url)
	reMDImage = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	// HTML <img src="...">
	reHTMLImage = regexp.MustCompile(`<img\s+[^>]*src="([^"]+)"[^>]*/?>`)
	// Obsidian wikilink: [[Title]] или [[Title|alias]]
	reObsidianWikilink = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)
	// Obsidian embed: ![[path/to/file]]
	reObsidianEmbed = regexp.MustCompile(`!\[\[([^\]]+)\]\]`)
)

// LinkResolver — двусторонняя карта между Kaiten ID/UID документа и его именем в vault.
// Используется для трансляции ссылок в обе стороны.
type LinkResolver struct {
	// byID: kaiten_id (int → string) → title без расширения (для wikilinks)
	byID map[string]string
	// byTitle: title без расширения → kaiten_id
	byTitle map[string]string
	// vaultPath: kaiten_id → relpath in vault (для embed-ссылок)
	vaultPath map[string]string
}

// NewLinkResolver строит резолвер по списку Kaiten-документов и локальных файлов.
func NewLinkResolver(docs []kaiten.Document, locals []obsidian.File) *LinkResolver {
	r := &LinkResolver{
		byID:      map[string]string{},
		byTitle:   map[string]string{},
		vaultPath: map[string]string{},
	}
	for _, d := range docs {
		id := strconv.Itoa(d.ID)
		r.byID[id] = d.Title
		r.byTitle[d.Title] = id
		if d.UID != "" {
			r.byID[d.UID] = d.Title
			r.byTitle[d.Title] = d.UID
		}
	}
	for _, l := range locals {
		id := strconv.Itoa(l.Frontmatter.KaitenID)
		r.vaultPath[id] = l.RelPath
		// На случай если title в frontmatter совпадает с локальным.
		base := strings.TrimSuffix(filepath.Base(l.RelPath), ".md")
		r.byID[id] = base
		r.byTitle[base] = id
	}
	return r
}

// KaitenToObsidian преобразует ссылки Kaiten в Obsidian-нотацию:
//   - [text](/document/123) → [[Title]] (если title известен) или [text](/document/123) (если нет)
//   - <a href="/document/123">text</a> → [[Title]]
//
// Если документ неизвестен (нет в byID) — оставляем как есть, чтобы не терять ссылку.
func (r *LinkResolver) KaitenToObsidian(body string) string {
	// Markdown-ссылки.
	body = reKaitenMDDocLink.ReplaceAllStringFunc(body, func(m string) string {
		match := reKaitenMDDocLink.FindStringSubmatch(m)
		if len(match) < 3 {
			return m
		}
		text, id := match[1], match[2]
		title, ok := r.byID[id]
		if !ok {
			return m // оставляем оригинал
		}
		if text == title || text == "" {
			return "[[" + title + "]]"
		}
		return "[[" + title + "|" + text + "]]"
	})
	// HTML-ссылки.
	body = reKaitenHTMLDocLink.ReplaceAllStringFunc(body, func(m string) string {
		match := reKaitenHTMLDocLink.FindStringSubmatch(m)
		if len(match) < 3 {
			return m
		}
		id, text := match[1], match[2]
		title, ok := r.byID[id]
		if !ok {
			return m
		}
		if text == title || text == "" {
			return "[[" + title + "]]"
		}
		return "[[" + title + "|" + text + "]]"
	})
	return body
}

// ObsidianToKaiten преобразует [[Title]] и [[Title|alias]] обратно в Markdown-ссылки Kaiten.
// Неизвестные wikilinks оставляются как есть (Obsidian-only документы).
func (r *LinkResolver) ObsidianToKaiten(body string) string {
	return reObsidianWikilink.ReplaceAllStringFunc(body, func(m string) string {
		match := reObsidianWikilink.FindStringSubmatch(m)
		if len(match) < 2 {
			return m
		}
		title := strings.TrimSpace(match[1])
		alias := title
		if len(match) >= 3 && match[2] != "" {
			alias = match[2]
		}
		id, ok := r.byTitle[title]
		if !ok {
			return m
		}
		return "[" + alias + "](/document/" + id + ")"
	})
}

// ---------- Inline images ----------

// ImageHandler отвечает за скачивание inline-картинок Kaiten и перепись ссылок.
type ImageHandler struct {
	Vault   string
	BaseURL string
	Client  *kaiten.Client
	Logger  Loggable
	DryRun  bool
}

// Loggable — небольшой интерфейс, чтобы не зависеть от *slog.Logger напрямую в тестах.
type Loggable interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// RewriteForObsidian:
//   - находит все <img src="..."> и ![](url) с URL внутри Kaiten;
//   - скачивает картинку в <vault>/kaiten_files/<docID>/inline/<name>;
//   - переписывает ссылку на относительный путь.
//
// Внешние ссылки (https://example.com/pic.png без kaiten-домена) остаются как есть.
func (h *ImageHandler) RewriteForObsidian(ctx context.Context, docID int, body string) string {
	rewriter := func(rawURL string) string {
		if !h.isKaitenInternalURL(rawURL) {
			return rawURL
		}
		localPath, err := h.downloadInline(ctx, docID, rawURL)
		if err != nil {
			if h.Logger != nil {
				h.Logger.Warn("не удалось скачать inline-картинку", "url", rawURL, "err", err)
			}
			return rawURL
		}
		return localPath
	}

	// Markdown ![alt](url)
	body = reMDImage.ReplaceAllStringFunc(body, func(m string) string {
		match := reMDImage.FindStringSubmatch(m)
		if len(match) < 3 {
			return m
		}
		alt, urlStr := match[1], match[2]
		newURL := rewriter(urlStr)
		if newURL == urlStr {
			return m
		}
		return "![" + alt + "](" + newURL + ")"
	})
	// HTML <img src="...">
	body = reHTMLImage.ReplaceAllStringFunc(body, func(m string) string {
		match := reHTMLImage.FindStringSubmatch(m)
		if len(match) < 2 {
			return m
		}
		urlStr := match[1]
		newURL := rewriter(urlStr)
		if newURL == urlStr {
			return m
		}
		return `<img src="` + newURL + `"/>`
	})
	return body
}

// RewriteForKaiten: ![[kaiten_files/<docID>/inline/file.png]] → загрузить и заменить
// на абсолютный URL Kaiten в attachment.
// Возвращает обновлённый body. Загрузки выполняются как side-effect через клиент.
func (h *ImageHandler) RewriteForKaiten(ctx context.Context, docID int, body string) string {
	return reObsidianEmbed.ReplaceAllStringFunc(body, func(m string) string {
		match := reObsidianEmbed.FindStringSubmatch(m)
		if len(match) < 2 {
			return m
		}
		rel := match[1]
		abs := filepath.Join(h.Vault, rel)
		// Только если файл есть на диске.
		f, err := os.Open(abs) //nolint:gosec
		if err != nil {
			return m
		}
		defer func() { _ = f.Close() }()
		if h.DryRun {
			return m
		}
		name := filepath.Base(rel)
		att, uerr := h.Client.UploadDocumentAttachment(ctx, docID, name, f)
		if uerr != nil {
			if h.Logger != nil {
				h.Logger.Warn("не удалось загрузить inline-картинку", "path", rel, "err", uerr)
			}
			return m
		}
		return "![" + name + "](" + att.URL + ")"
	})
}

// downloadInline качает картинку из Kaiten по URL в <vault>/kaiten_files/<docID>/inline/<name>.
// Возвращает относительный путь (от корня vault) для подстановки в md.
func (h *ImageHandler) downloadInline(ctx context.Context, docID int, rawURL string) (string, error) {
	name := guessFileName(rawURL)
	rel := filepath.Join(AttachmentsDir, strconv.Itoa(docID), "inline", name)
	abs := filepath.Join(h.Vault, rel)
	if h.DryRun {
		return rel, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	// Если файл уже есть — не качаем повторно.
	if _, err := os.Stat(abs); err == nil {
		return rel, nil
	}
	tmp := abs + ".part"
	f, err := os.Create(tmp) //nolint:gosec
	if err != nil {
		return "", err
	}
	if _, err := h.Client.DownloadAttachment(ctx, rawURL, f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	_ = f.Close()
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return rel, nil
}

// isKaitenInternalURL — true, если URL указывает на тот же Kaiten-инстанс.
func (h *ImageHandler) isKaitenInternalURL(rawURL string) bool {
	if h.BaseURL == "" {
		return false
	}
	if strings.HasPrefix(rawURL, "/") {
		return true // относительный путь — точно Kaiten
	}
	return strings.HasPrefix(rawURL, h.BaseURL)
}

// guessFileName извлекает имя файла из URL.
// При коллизиях добавляет хеш-суффикс на стороне вызывающего (мы используем sha256 от URL).
func guessFileName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		// fallback: detsterministic hash from full URL
		return fmt.Sprintf("inline-%x", simpleHash(rawURL))
	}
	name := filepath.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		return fmt.Sprintf("inline-%x", simpleHash(rawURL))
	}
	return obsidian.Sanitize(name)
}

// simpleHash — короткий хеш строки (для имён файлов).
func simpleHash(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Чтобы линтер не ругался на unused io.
var _ = io.Discard
