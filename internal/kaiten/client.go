// Package kaiten — клиент REST API Kaiten (https://<domain>.kaiten.ru/api/latest).
// Поддерживает Bearer-auth, retry с экспоненциальным бэкоффом и rate-limit ≤5 req/sec.
package kaiten

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// MaxResponseSize — верхний потолок на размер ответа Kaiten (риск R-18).
// Защищает от выборки 1 GB JSON в RAM.
const MaxResponseSize int64 = 64 << 20 // 64 MB

// Client — обёртка над net/http с авторизацией и ограничением скорости.
type Client struct {
	BaseURL string
	Token   string

	HTTPClient *http.Client // экспортирован — можно подменить транспорт в тестах
	limiter    *rate.Limiter

	// MaxRetries — число повторов на 5xx и 429.
	MaxRetries int
}

// redact маскирует Bearer-токен в строке ошибки (риск R-06).
// Применяется к любым строкам, которые могут попасть в логи.
func (c *Client) redact(s string) string {
	if c.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Token, "***REDACTED***")
}

// New создаёт клиент. baseURL без завершающего слэша (или с ним — нормализуется).
// rate-limit Kaiten = 5 req/sec.
func New(baseURL, token string) *Client {
	hc := &http.Client{
		Timeout: 30 * time.Second,
		// PATCH/PUT/POST не должны автоматически следовать за 3xx —
		// иначе тело запроса теряется (превращается в GET).
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: hc,
		limiter:    rate.NewLimiter(rate.Limit(5), 5),
		MaxRetries: 4,
	}
}

// SetRateLimit подменяет лимитер (для тестов и инстансов с другим лимитом).
func (c *Client) SetRateLimit(rps float64) {
	c.limiter = rate.NewLimiter(rate.Limit(rps), int(rps)+1)
}

// do выполняет HTTP-запрос с rate-limit и ретраями. Тело ответа возвращается прочитанным.
// Возвращает (body, error). HTTP status code включается в текст ошибки.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}

	url := c.BaseURL + "/api/latest" + path

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		// Проверяем отмену контекста перед каждой попыткой.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}

		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			backoff(attempt)
			continue
		}
		// LimitReader против риска R-18 (ответ 1 GB).
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
		_ = resp.Body.Close()

		// 429: уважаем X-RateLimit-Reset либо ждём бэкоффом.
		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("kaiten %s %s: 429 rate-limited: %s", method, path, c.redact(string(respBody)))
			if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
				if secs, perr := strconv.Atoi(reset); perr == nil && secs > 0 && secs < 60 {
					time.Sleep(time.Duration(secs) * time.Second)
					continue
				}
			}
			backoff(attempt)
			continue
		}
		// 5xx — ретраим.
		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			lastErr = fmt.Errorf("kaiten %s %s: %d %s", method, path, resp.StatusCode, c.redact(string(respBody)))
			backoff(attempt)
			continue
		}
		// 3xx — после CheckRedirect → ErrUseLastResponse сюда попадёт сам редирект.
		// Считаем это ошибкой — Kaiten не должен редиректить.
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return respBody, fmt.Errorf("kaiten %s %s: неожиданный редирект %d → %s",
				method, path, resp.StatusCode, resp.Header.Get("Location"))
		}
		// 4xx (кроме 429) — фатально, без ретраев.
		// Для 401/403 специально НЕ логируем тело в вернувшейся ошибке — там может быть
		// echo токена через прокси (риск R-06).
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return respBody, fmt.Errorf("kaiten %s %s: %d (проверьте Bearer-токен)", method, path, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return respBody, fmt.Errorf("kaiten %s %s: %d %s", method, path, resp.StatusCode, c.redact(string(respBody)))
		}
		return respBody, nil
	}
	if lastErr == nil {
		lastErr = errors.New("исчерпаны попытки запроса")
	}
	return nil, lastErr
}

func backoff(attempt int) {
	d := time.Duration(1<<attempt) * 250 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	time.Sleep(d)
}

// ---------- Модели ----------

// User — упрощённая модель пользователя.
type User struct {
	ID       int    `json:"id"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

// Space — пространство Kaiten.
//
// Поле ParentEntityUID в API Kaiten возвращается строкой (UUID-подобный идентификатор),
// а не числом. Неправильный тип раньше приводил к ошибке парсинга:
// "json: cannot unmarshal string into Go struct field Space.parent_entity_uid of type int".
type Space struct {
	ID              int     `json:"id"`
	Title           string  `json:"title"`
	UID             string  `json:"uid,omitempty"`
	ParentEntityUID *string `json:"parent_entity_uid,omitempty"`
	Path            string  `json:"path,omitempty"`
	Entities        []Space `json:"entities,omitempty"`
}

// Document — документ уровня пространства.
// Поле Type принимает значения "html" или "markdown".
//
// ОСОБЕННОСТЬ: по доке Kaiten поле id для документов — string, но
// на практике приходит и integer (например в ListTreeEntities). Поэтому ID храним
// как int, но принимаем оба варианта через UnmarshalJSON.
type Document struct {
	ID       int       `json:"-"`
	UID      string    `json:"uid,omitempty"`
	Title    string    `json:"title"`
	Type     string    `json:"type"`
	Content  string    `json:"content"`
	Path     string    `json:"path,omitempty"`
	SpaceID  int       `json:"space_id,omitempty"`
	ParentID *int      `json:"parent_id,omitempty"`
	Updated  time.Time `json:"updated"`
	CardID   *int      `json:"card_id,omitempty"`
}

// docAlias — технический тип для UnmarshalJSON без бесконечной рекурсии.
type docAlias struct {
	ID       json.RawMessage `json:"id"`
	UID      string          `json:"uid,omitempty"`
	Title    string          `json:"title"`
	Type     string          `json:"type"`
	Content  string          `json:"content"`
	Path     string          `json:"path,omitempty"`
	SpaceID  int             `json:"space_id,omitempty"`
	ParentID *int            `json:"parent_id,omitempty"`
	Updated  time.Time       `json:"updated"`
	CardID   *int            `json:"card_id,omitempty"`
}

// MarshalJSON — выводит ID как number в JSON (нужно только в тестах и для отладки).
func (d Document) MarshalJSON() ([]byte, error) {
	type out struct {
		ID       int       `json:"id"`
		UID      string    `json:"uid,omitempty"`
		Title    string    `json:"title"`
		Type     string    `json:"type"`
		Content  string    `json:"content"`
		Path     string    `json:"path,omitempty"`
		SpaceID  int       `json:"space_id,omitempty"`
		ParentID *int      `json:"parent_id,omitempty"`
		Updated  time.Time `json:"updated"`
		CardID   *int      `json:"card_id,omitempty"`
	}
	return json.Marshal(out{
		ID: d.ID, UID: d.UID, Title: d.Title, Type: d.Type,
		Content: d.Content, Path: d.Path, SpaceID: d.SpaceID,
		ParentID: d.ParentID, Updated: d.Updated, CardID: d.CardID,
	})
}

// UnmarshalJSON принимает id как string или как number.
// Это фикс ошибки "cannot unmarshal string into Go struct field Document.id of type int":
// POST /documents возвращает id строкой (согласно доке), GET /documents — числом.
func (d *Document) UnmarshalJSON(data []byte) error {
	var raw docAlias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.UID = raw.UID
	d.Title = raw.Title
	d.Type = raw.Type
	d.Content = raw.Content
	d.Path = raw.Path
	d.SpaceID = raw.SpaceID
	d.ParentID = raw.ParentID
	d.Updated = raw.Updated
	d.CardID = raw.CardID
	if len(raw.ID) == 0 {
		return nil
	}
	// Строковый вариант "123".
	if raw.ID[0] == '"' {
		var s string
		if err := json.Unmarshal(raw.ID, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			// Не числовая строка — используем как UID, ID оставляем 0.
			if d.UID == "" {
				d.UID = s
			}
			return nil
		}
		d.ID = n
		return nil
	}
	// Числовой вариант 123.
	return json.Unmarshal(raw.ID, &d.ID)
}

// Типы сущностей в tree-entities.
const (
	EntityTypeSpace         = "space"
	EntityTypeDocument      = "document"
	EntityTypeDocumentGroup = "document_group" // это и есть «папка»
	EntityTypeStoryMap      = "story_map"
)

// TreeEntity — элемент из GET /tree-entities.
// EntityType определяет природу: space / document / document_group (папка) / story_map.
//
// ID может приходить как int, string или отсутствовать вообще (для документов в некоторых
// инстансах возвращается только UID). UID — всегда присутствует и является основным
// идентификатором.
type TreeEntity struct {
	UID             string  `json:"uid"`
	ID              int     `json:"-"`
	Title           string  `json:"title"`
	EntityType      string  `json:"entity_type"`
	ParentEntityUID *string `json:"parent_entity_uid,omitempty"`
	Path            string  `json:"path,omitempty"`
	SortOrder       float64 `json:"sort_order,omitempty"`
	Archived        bool    `json:"archived,omitempty"`
}

// treeEntityAlias — технический тип для UnmarshalJSON без рекурсии.
type treeEntityAlias struct {
	UID             string          `json:"uid"`
	ID              json.RawMessage `json:"id"`
	Title           string          `json:"title"`
	EntityType      string          `json:"entity_type"`
	ParentEntityUID *string         `json:"parent_entity_uid,omitempty"`
	Path            string          `json:"path,omitempty"`
	SortOrder       float64         `json:"sort_order,omitempty"`
	Archived        bool            `json:"archived,omitempty"`
}

// MarshalJSON — выводит id как number (для тестов).
func (t TreeEntity) MarshalJSON() ([]byte, error) {
	type out struct {
		UID             string  `json:"uid"`
		ID              int     `json:"id,omitempty"`
		Title           string  `json:"title"`
		EntityType      string  `json:"entity_type"`
		ParentEntityUID *string `json:"parent_entity_uid,omitempty"`
		Path            string  `json:"path,omitempty"`
		SortOrder       float64 `json:"sort_order,omitempty"`
		Archived        bool    `json:"archived,omitempty"`
	}
	return json.Marshal(out{
		UID: t.UID, ID: t.ID, Title: t.Title, EntityType: t.EntityType,
		ParentEntityUID: t.ParentEntityUID, Path: t.Path,
		SortOrder: t.SortOrder, Archived: t.Archived,
	})
}

// UnmarshalJSON принимает id как string / number / отсутствующее.
// Аналогично Document.UnmarshalJSON, но без fallback в UID (UID уже есть).
func (t *TreeEntity) UnmarshalJSON(data []byte) error {
	var raw treeEntityAlias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.UID = raw.UID
	t.Title = raw.Title
	t.EntityType = raw.EntityType
	t.ParentEntityUID = raw.ParentEntityUID
	t.Path = raw.Path
	t.SortOrder = raw.SortOrder
	t.Archived = raw.Archived
	if len(raw.ID) == 0 || string(raw.ID) == "null" {
		return nil
	}
	if raw.ID[0] == '"' {
		var s string
		if err := json.Unmarshal(raw.ID, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		if n, err := strconv.Atoi(s); err == nil {
			t.ID = n
		}
		return nil
	}
	return json.Unmarshal(raw.ID, &t.ID)
}

// IsFolder — true для document_group.
func (t TreeEntity) IsFolder() bool { return t.EntityType == EntityTypeDocumentGroup }

// IsDocument — true для document.
func (t TreeEntity) IsDocument() bool { return t.EntityType == EntityTypeDocument }

// IsSpace — true для space.
func (t TreeEntity) IsSpace() bool { return t.EntityType == EntityTypeSpace }

// Attachment — вложение документа (или карточки; схема общая).
type Attachment struct {
	ID        int       `json:"id"`
	UID       string    `json:"uid,omitempty"`
	Name      string    `json:"name"`
	Size      int64     `json:"size,omitempty"`
	Type      int       `json:"type,omitempty"` // 1=attachment, см. док
	URL       string    `json:"url"`
	SortOrder float64   `json:"sort_order,omitempty"`
	Created   time.Time `json:"created,omitempty"`
	Updated   time.Time `json:"updated,omitempty"`
}

// IsSpaceLevel — true, если документ привязан к пространству, а не к карточке.
func (d Document) IsSpaceLevel() bool { return d.CardID == nil }

// ---------- Endpoints ----------
//
// ВАЖНО: точные пути для документов в публичной документации Kaiten не описаны
// (https://developers.kaiten.ru/). Ниже использованы наиболее распространённые
// варианты, согласующиеся с REST-конвенцией Kaiten. При необходимости
// скорректируйте константы DocsListPath / DocPath под конкретный инстанс.

const (
	pathCurrentUser = "/users/current"
	pathSpaces      = "/spaces"
	// DocsListPath — список документов пространства (GET).
	DocsListPath = "/spaces/%d/documents"
	// DocPath — операции с конкретным документом (GET/PATCH).
	DocPath = "/documents/%d"
)

// GetCurrentUser — проверка авторизации.
func (c *Client) GetCurrentUser(ctx context.Context) (*User, error) {
	body, err := c.do(ctx, http.MethodGet, pathCurrentUser, nil)
	if err != nil {
		return nil, err
	}
	var u User
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListSpaces — все доступные пространства верхнего уровня.
func (c *Client) ListSpaces(ctx context.Context) ([]Space, error) {
	body, err := c.do(ctx, http.MethodGet, pathSpaces, nil)
	if err != nil {
		return nil, err
	}
	var s []Space
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	return s, nil
}

// ListDocuments возвращает все документы пространства, включая вложенные.
// Игнорирует документы-карточки (Document.IsSpaceLevel()==false).
func (c *Client) ListDocuments(ctx context.Context, spaceID int) ([]Document, error) {
	body, err := c.do(ctx, http.MethodGet, fmt.Sprintf(DocsListPath, spaceID), nil)
	if err != nil {
		return nil, err
	}
	var docs []Document
	if err := json.Unmarshal(body, &docs); err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(docs))
	for _, d := range docs {
		if d.IsSpaceLevel() {
			out = append(out, d)
		}
	}
	return out, nil
}

// GetDocument — полный документ с контентом по числовому ID.
func (c *Client) GetDocument(ctx context.Context, id int) (*Document, error) {
	body, err := c.do(ctx, http.MethodGet, fmt.Sprintf(DocPath, id), nil)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// GetDocumentByUID — полный документ по UID.
// По доке Kaiten это основной идентификатор (DELETE /documents/{document_uid}).
func (c *Client) GetDocumentByUID(ctx context.Context, uid string) (*Document, error) {
	body, err := c.do(ctx, http.MethodGet, "/documents/"+uid, nil)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// PatchPayload — то, что отправляем при обновлении.
type PatchPayload struct {
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Type    string `json:"type,omitempty"`
}

// CreatePayload — полезная нагрузка для POST /documents.
// Согласно доке Kaiten, sort_order обязателен.
type CreatePayload struct {
	Title           string  `json:"title"`
	Content         string  `json:"content,omitempty"`
	Type            string  `json:"type,omitempty"` // markdown / html
	ParentEntityUID *string `json:"parent_entity_uid,omitempty"`
	SortOrder       float64 `json:"sort_order"`
}

// CreateGroupPayload — создание вложенной папки (document_group).
// Согласно доке Kaiten POST /document-groups (не /tree-entities!).
// В ответ возвращается объект document group, у которого есть uid —
// его и нужно использовать как parent_entity_uid для дочерних сущностей.
type CreateGroupPayload struct {
	Title           string  `json:"title"`
	ParentEntityUID *string `json:"parent_entity_uid,omitempty"`
	SortOrder       float64 `json:"sort_order,omitempty"`
}

// pathDocumentGroups — POST /api/latest/document-groups (см. доку Kaiten).
const pathDocumentGroups = "/document-groups"

// CreateDocumentGroup — создаёт новую папку (document_group) внутри родителя.
// Используется, чтобы отразить файловую иерархию Obsidian в Kaiten.
//
// ВАЖНО: ранее ошибочно дёргали POST /tree-entities — у этого эндпоинта в
// публичном API Kaiten нет операции создания, он только GET. Папки
// создаются через POST /document-groups, после чего автоматически
// регистрируются в дереве (tree-entities).
func (c *Client) CreateDocumentGroup(ctx context.Context, p CreateGroupPayload) (*TreeEntity, error) {
	if p.SortOrder == 0 {
		p.SortOrder = float64(time.Now().UnixNano()) / 1e9
	}
	body, err := c.do(ctx, http.MethodPost, pathDocumentGroups, p)
	if err != nil {
		return nil, err
	}
	// Ответ — document_group object. Маппим в TreeEntity (поля uid, title,
	// parent_entity_uid совпадают; entity_type принудительно проставим).
	var t TreeEntity
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	if t.EntityType == "" {
		t.EntityType = EntityTypeDocumentGroup
	}
	return &t, nil
}

// CreateDocument — создаёт новый документ в указанной папке/пространстве.
// На ряде инстансах Kaiten POST /documents игнорирует поле content в теле запроса
// (сохраняет только title); реальное тело нужно отправлять отдельным PATCH'ом.
func (c *Client) CreateDocument(ctx context.Context, p CreatePayload) (*Document, error) {
	if p.SortOrder == 0 {
		p.SortOrder = float64(time.Now().Unix()) // иначе Kaiten отклонит (exclusiveMinimum: 0)
	}
	body, err := c.do(ctx, http.MethodPost, "/documents", p)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// DeleteDocument — удаляет документ по UID (согласно доке Kaiten:
// DELETE /documents/{document_uid}). Если UID пуст — fallback на int ID.
func (c *Client) DeleteDocument(ctx context.Context, idOrUID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/documents/"+idOrUID, nil)
	return err
}

// PatchDocument — обновить документ по числовому ID. Возвращает обновлённую версию.
func (c *Client) PatchDocument(ctx context.Context, id int, p PatchPayload) (*Document, error) {
	body, err := c.do(ctx, http.MethodPatch, fmt.Sprintf(DocPath, id), p)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// PatchDocumentByUID — обновить документ по UID. Основной путь в Kaiten API.
func (c *Client) PatchDocumentByUID(ctx context.Context, uid string, p PatchPayload) (*Document, error) {
	body, err := c.do(ctx, http.MethodPatch, "/documents/"+uid, p)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ---------- Tree entities (папки и документы) ----------

// pathTreeEntities — GET /api/latest/tree-entities (сверяется с докой).
const pathTreeEntities = "/tree-entities"

// ListTreeEntities — одна страница сущностей. Если parentUID == "" — верхний уровень.
// limit max 500 (ограничение Kaiten API).
func (c *Client) ListTreeEntities(ctx context.Context, parentUID string, limit, offset int) ([]TreeEntity, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	u := fmt.Sprintf("%s?limit=%d&offset=%d", pathTreeEntities, limit, offset)
	if parentUID != "" {
		u += "&parent_entity_uid=" + parentUID
	}
	body, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var list []TreeEntity
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// ListTreeChildrenAll — все прямые потомки parentUID (все страницы).
func (c *Client) ListTreeChildrenAll(ctx context.Context, parentUID string) ([]TreeEntity, error) {
	var out []TreeEntity
	const pageSize = 500
	for offset := 0; ; offset += pageSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := c.ListTreeEntities(ctx, parentUID, pageSize, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < pageSize {
			break
		}
	}
	return out, nil
}

// FolderEntry — элемент плоского списка папок с полным путём.
// FullPath — «Parent / Sub / Target» (разделитель — " / ").
type FolderEntry struct {
	UID      string
	Title    string
	FullPath string
}

// ListAllDocumentGroups — плоский список всех папок (document_group) в инстансе.
//
// Подход:
//  1. Один вызов GET /tree-entities без параметров — возвращает
//     верхний уровень дерева (все сущности без parent_entity_uid).
//  2. Добор вложенных папок: рекурсивно входим внутрь каждой
//     space/document_group, но СОБИРАЕМ ТОЛЬКО document_group.
//
// archived папки пропускаются. Путь формируется «Parent / Sub / Target».
func (c *Client) ListAllDocumentGroups(ctx context.Context) ([]FolderEntry, error) {
	// 1) Верхний уровень: явный вызов без parent_entity_uid.
	top, err := c.listTreeTopLevel(ctx)
	if err != nil {
		return nil, err
	}

	var out []FolderEntry
	visited := map[string]bool{}

	var walk func(items []TreeEntity, parentPath string) error
	walk = func(items []TreeEntity, parentPath string) error {
		for _, ch := range items {
			if ch.Archived || visited[ch.UID] {
				continue
			}
			visited[ch.UID] = true
			fullPath := ch.Title
			if parentPath != "" {
				fullPath = parentPath + " / " + ch.Title
			}
			if ch.IsFolder() {
				out = append(out, FolderEntry{
					UID:      ch.UID,
					Title:    ch.Title,
					FullPath: fullPath,
				})
			}
			// Рекурсия в контейнеры (space и document_group) с parent_entity_uid=текущий UID.
			if ch.IsSpace() || ch.IsFolder() {
				children, err := c.ListTreeChildrenAll(ctx, ch.UID)
				if err != nil {
					return err
				}
				if err := walk(children, fullPath); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(top, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// listTreeTopLevel — GET /tree-entities без параметра parent_entity_uid.
// Именно такой вызов возвращает плоский список верхнего уровня, включая
// папки (document_group) с parent_entity_uid: null.
func (c *Client) listTreeTopLevel(ctx context.Context) ([]TreeEntity, error) {
	var all []TreeEntity
	const pageSize = 500
	for offset := 0; ; offset += pageSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u := fmt.Sprintf("%s?limit=%d&offset=%d", pathTreeEntities, pageSize, offset)
		body, err := c.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		var page []TreeEntity
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
	}
	return all, nil
}

// WalkTree — рекурсивный DFS-обход дерева от rootUID.
// Собирает все document_group (папки) и document, включая саму root-сущность в visited.
// archived элементы пропускаются.
//
// FALLBACK: если tree-entities возвращает пусто (на некоторых инстансах вложенные
// документы не попадают в tree-entities), пробуем
func (c *Client) WalkTree(ctx context.Context, rootUID string) ([]TreeEntity, error) {
	visited := map[string]bool{}
	var out []TreeEntity
	var walk func(parentUID string) error
	walk = func(parentUID string) error {
		children, err := c.ListTreeChildrenAll(ctx, parentUID)
		if err != nil {
			return err
		}
		// FALLBACK: если tree-entities пуст для папки, пробуем получить документы
		// через GET /documents?parent_entity_uid=… (реальный эндпоинт Kaiten).
		if len(children) == 0 && parentUID != "" {
			docs, derr := c.ListDocumentsByParent(ctx, parentUID)
			if derr == nil {
				for _, d := range docs {
					p := parentUID
					children = append(children, TreeEntity{
						UID:             d.UID,
						ID:              d.ID,
						Title:           d.Title,
						EntityType:      EntityTypeDocument,
						ParentEntityUID: &p,
					})
				}
			}
		}
		for _, ch := range children {
			if ch.Archived {
				continue
			}
			if visited[ch.UID] {
				continue // защита от циклов
			}
			visited[ch.UID] = true
			out = append(out, ch)
			// Рекурсия только в контейнеры: space и document_group.
			if ch.IsFolder() || ch.IsSpace() {
				if err := walk(ch.UID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(rootUID); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDocumentsByParent — GET /documents?parent_entity_uid=<UID>.
// Этот эндпоинт явно документирован Kaiten (см. /documents/retrieve-list-of-documents)
// и возвращает все документы внутри папки/пространства по UID. Используется
// как fallback, когда tree-entities не показывает документы внутри document_group.
func (c *Client) ListDocumentsByParent(ctx context.Context, parentUID string) ([]Document, error) {
	var all []Document
	const pageSize = 500
	for offset := 0; ; offset += pageSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u := fmt.Sprintf("/documents?limit=%d&offset=%d&parent_entity_uid=%s", pageSize, offset, parentUID)
		body, err := c.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		var page []Document
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
	}
	return all, nil
}

// ---------- Attachments ----------
//
// В публичной документации Kaiten явно расписаны вложения для карточек
// (PUT /cards/{id}/files). Для документов схема аналогичная:
// GET /documents/{id}/files — список, PUT /documents/{id}/files — загрузка.
// Если в вашем инстансе эндпоинты иные — скорректируйте константы ниже.

const (
	DocAttachmentsList  = "/documents/%d/files"
	DocAttachmentUpload = "/documents/%d/files"
	DocAttachmentDelete = "/documents/%d/files/%d"
)

// ListDocumentAttachments — все вложения документа.
func (c *Client) ListDocumentAttachments(ctx context.Context, docID int) ([]Attachment, error) {
	body, err := c.do(ctx, http.MethodGet, fmt.Sprintf(DocAttachmentsList, docID), nil)
	if err != nil {
		return nil, err
	}
	var list []Attachment
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// DownloadAttachment — скачивает файл по Attachment.URL в io.Writer.
// Использует тот же HTTP-клиент и Bearer-токен (для private-file URLs).
// Rate-limit тоже уважаем.
func (c *Client) DownloadAttachment(ctx context.Context, urlStr string, w io.Writer) (int64, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("download %s: %d", c.redact(urlStr), resp.StatusCode)
	}
	// Против OOM: лимит размера файла.
	n, err := io.Copy(w, io.LimitReader(resp.Body, MaxAttachmentSize))
	return n, err
}

// MaxAttachmentSize — потолок на размер вложения (защита от больших файлов).
const MaxAttachmentSize int64 = 256 << 20 // 256 MB

// UploadDocumentAttachment — заливает файл в документ через multipart/form-data.
// Имя поля формы — "file" (совпадает с attach-to-card).
func (c *Client) UploadDocumentAttachment(ctx context.Context, docID int, name string, content io.Reader) (*Attachment, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, content); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	url := c.BaseURL + "/api/latest" + fmt.Sprintf(DocAttachmentUpload, docID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upload %s: %d %s", fmt.Sprintf(DocAttachmentUpload, docID), resp.StatusCode, c.redact(string(respBody)))
	}
	var a Attachment
	if err := json.Unmarshal(respBody, &a); err != nil {
		return nil, fmt.Errorf("parse upload response: %w", err)
	}
	return &a, nil
}

// DeleteDocumentAttachment — удаляет вложение документа.
func (c *Client) DeleteDocumentAttachment(ctx context.Context, docID, fileID int) error {
	_, err := c.do(ctx, http.MethodDelete, fmt.Sprintf(DocAttachmentDelete, docID, fileID), nil)
	return err
}
