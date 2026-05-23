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
type Document struct {
	ID       int       `json:"id"`
	Title    string    `json:"title"`
	Type     string    `json:"type"`
	Content  string    `json:"content"`
	Path     string    `json:"path,omitempty"`
	SpaceID  int       `json:"space_id,omitempty"`
	ParentID *int      `json:"parent_id,omitempty"`
	Updated  time.Time `json:"updated"`
	CardID   *int      `json:"card_id,omitempty"`
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

// GetDocument — полный документ с контентом.
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

// PatchPayload — то, что отправляем при обновлении.
type PatchPayload struct {
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Type    string `json:"type,omitempty"`
}

// PatchDocument — обновить документ. Возвращает обновлённую версию.
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
