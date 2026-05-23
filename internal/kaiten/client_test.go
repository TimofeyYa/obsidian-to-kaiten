package kaiten

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetCurrentUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/latest/users/current" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("нет Bearer auth")
		}
		_, _ = w.Write([]byte(`{"id":1,"full_name":"Tim","email":"t@e"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	u, err := c.GetCurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "t@e" {
		t.Errorf("email = %s", u.Email)
	}
}

func TestRetryOn5xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	c.MaxRetries = 5
	if _, err := c.GetCurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("ожидалось 3 запроса, было %d", hits)
	}
}

func TestListDocumentsFiltersCardLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"title":"A"},{"id":2,"title":"B","card_id":99}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	docs, err := c.ListDocuments(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].ID != 1 {
		t.Errorf("ожидался только space-level документ, получено: %+v", docs)
	}
}

// Фикс бага #7: 429 с X-RateLimit-Reset — клиент уважает заголовок.
func TestRetryOn429WithResetHeader(t *testing.T) {
	var hits int32
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("X-RateLimit-Reset", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	c.MaxRetries = 3
	if _, err := c.GetCurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < time.Second {
		t.Errorf("клиент не подождал X-RateLimit-Reset: elapsed=%v", elapsed)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("ожидалось 2 запроса, было %d", hits)
	}
}

// 4xx (кроме 429) — фатально, без ретраев.
func TestNoRetryOn4xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	c.MaxRetries = 5
	_, err := c.GetCurrentUser(context.Background())
	if err == nil {
		t.Fatal("ожидалась ошибка 401")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("4xx должен быть без ретраев, было %d попыток", hits)
	}
}

// Фикс бага #8: PATCH не должен следовать за 3xx — иначе тело теряется.
func TestPatchDoesNotFollowRedirect(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method == http.MethodPatch {
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		// Второй хит был бы GET после редиректа — но не должен случиться.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	c.MaxRetries = 0
	_, err := c.PatchDocument(context.Background(), 1, PatchPayload{Title: "x"})
	if err == nil {
		t.Fatal("ожидалась ошибка из-за редиректа")
	}
	if !strings.Contains(err.Error(), "редирект") {
		t.Errorf("ошибка должна упоминать редирект: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("клиент НЕ должен следовать за редиректом, было %d запросов", hits)
	}
}

func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetCurrentUser(ctx); err == nil {
		t.Fatal("ожидалась ошибка отмены")
	}
}

func TestPatchDocument_SendsBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"id":1,"title":"x","content":"y","type":"markdown","updated":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	if _, err := c.PatchDocument(context.Background(), 1, PatchPayload{Title: "x", Content: "y", Type: "markdown"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"title":"x"`) || !strings.Contains(gotBody, `"content":"y"`) {
		t.Errorf("тело запроса некорректно: %s", gotBody)
	}
}

// Регрессия: parent_entity_uid в реальном API Kaiten — строка, не число.
// Старый тип *int падал с "cannot unmarshal string into Go struct field ... of type int".
func TestListSpaces_ParsesStringParentEntityUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":1,"title":"Root","uid":"abc-123","parent_entity_uid":null},
			{"id":2,"title":"Child","uid":"def-456","parent_entity_uid":"abc-123"}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	spaces, err := c.ListSpaces(context.Background())
	if err != nil {
		t.Fatalf("парсинг упал: %v", err)
	}
	if len(spaces) != 2 {
		t.Fatalf("ожидалось 2 пространства, получено %d", len(spaces))
	}
	if spaces[1].ParentEntityUID == nil || *spaces[1].ParentEntityUID != "abc-123" {
		t.Errorf("parent_entity_uid распарсен некорректно: %+v", spaces[1].ParentEntityUID)
	}
	if spaces[0].ParentEntityUID != nil {
		t.Errorf("null должен дать nil-указатель, получено %v", spaces[0].ParentEntityUID)
	}
}

// ListAllDocumentGroups собирает только document_group, рекурсивно,
// игнорируя spaces, documents и archived.
func TestListAllDocumentGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/latest/tree-entities" {
			http.NotFound(w, r)
			return
		}
		parent := r.URL.Query().Get("parent_entity_uid")
		root := ""
		spaceUID := "space-1"
		folderTop := "folder-top"
		folderSub := "folder-sub"
		switch parent {
		case root:
			// Верхний уровень: один space.
			_, _ = w.Write([]byte(`[
				{"uid":"space-1","title":"Marketing","entity_type":"space"}
			]`))
		case spaceUID:
			// Внутри space: одна папка и один документ.
			_, _ = w.Write([]byte(`[
				{"uid":"folder-top","title":"Docs","entity_type":"document_group","parent_entity_uid":"space-1"},
				{"uid":"doc-1","id":1,"title":"X","entity_type":"document","parent_entity_uid":"space-1"}
			]`))
		case folderTop:
			// Внутри папки: вложенная папка + archived папка (должна быть пропущена).
			_, _ = w.Write([]byte(`[
				{"uid":"folder-sub","title":"Drafts","entity_type":"document_group","parent_entity_uid":"folder-top"},
				{"uid":"folder-arch","title":"Old","entity_type":"document_group","parent_entity_uid":"folder-top","archived":true}
			]`))
		case folderSub:
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	c.SetRateLimit(1000)
	folders, err := c.ListAllDocumentGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("ожидалось 2 папки (Docs, Drafts), получено %d: %+v", len(folders), folders)
	}
	// Проверяем full path: Drafts должен быть "Marketing / Docs / Drafts".
	var drafts *FolderEntry
	for i := range folders {
		if folders[i].UID == "folder-sub" {
			drafts = &folders[i]
		}
	}
	if drafts == nil {
		t.Fatal("папка Drafts не найдена")
	}
	if !strings.Contains(drafts.FullPath, "Marketing / Docs / Drafts") {
		t.Errorf("неверный full path: %q", drafts.FullPath)
	}
}
