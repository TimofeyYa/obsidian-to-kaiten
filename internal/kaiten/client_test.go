package kaiten

import (
	"context"
	"encoding/json"
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
//
// Обратите внимание: верхний уровень получается запросом БЕЗ parent_entity_uid
// (реальный Kaiten отвечает верхний уровень дерева), и эта папка может иметь
// parent_entity_uid: null.
func TestListAllDocumentGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/latest/tree-entities" {
			http.NotFound(w, r)
			return
		}
		// Реальный Kaiten: без параметра — верхний уровень дерева.
		params := r.URL.Query()
		hasParent := params.Has("parent_entity_uid")
		parent := params.Get("parent_entity_uid")

		switch {
		case !hasParent:
			// Верхний уровень: space + топ-левел document_group с null parent
			// (как в реальном инстансе пользователя).
			_, _ = w.Write([]byte(`[
				{"uid":"space-1","title":"Marketing","entity_type":"space"},
				{"uid":"folder-root","title":"Personal Notes","entity_type":"document_group","parent_entity_uid":null}
			]`))
		case parent == "space-1":
			// Внутри space: одна папка и один документ.
			_, _ = w.Write([]byte(`[
				{"uid":"folder-top","title":"Docs","entity_type":"document_group","parent_entity_uid":"space-1"},
				{"uid":"doc-1","id":1,"title":"X","entity_type":"document","parent_entity_uid":"space-1"}
			]`))
		case parent == "folder-top":
			// Внутри папки: вложенная + archived (пропускается).
			_, _ = w.Write([]byte(`[
				{"uid":"folder-sub","title":"Drafts","entity_type":"document_group","parent_entity_uid":"folder-top"},
				{"uid":"folder-arch","title":"Old","entity_type":"document_group","parent_entity_uid":"folder-top","archived":true}
			]`))
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
	// Ожидаем 3 папки: Personal Notes (топ-левел), Docs, Drafts.
	if len(folders) != 3 {
		t.Fatalf("ожидалось 3 папки, получено %d: %+v", len(folders), folders)
	}
	var topLevel, drafts *FolderEntry
	for i := range folders {
		switch folders[i].UID {
		case "folder-root":
			topLevel = &folders[i]
		case "folder-sub":
			drafts = &folders[i]
		}
	}
	if topLevel == nil || topLevel.FullPath != "Personal Notes" {
		t.Errorf("top-level папка не найдена или путь неверный: %+v", topLevel)
	}
	if drafts == nil || !strings.Contains(drafts.FullPath, "Marketing / Docs / Drafts") {
		t.Errorf("вложенный path неверен: %+v", drafts)
	}
}

// Регрессия: POST /documents возвращает id строкой "123", а GET — числом 123.
// Document.UnmarshalJSON должен переварить оба варианта.
func TestDocument_UnmarshalJSON_AcceptsStringID(t *testing.T) {
	// id строкой (POST /documents response style).
	var d1 Document
	if err := json.Unmarshal([]byte(`{"id":"42","title":"X","uid":"u42"}`), &d1); err != nil {
		t.Fatalf("string id не распарсился: %v", err)
	}
	if d1.ID != 42 {
		t.Errorf("string id → %d, ожидалось 42", d1.ID)
	}
	if d1.UID != "u42" {
		t.Errorf("uid потерян: %q", d1.UID)
	}

	// id числом (GET /documents).
	var d2 Document
	if err := json.Unmarshal([]byte(`{"id":42,"title":"X"}`), &d2); err != nil {
		t.Fatalf("int id не распарсился: %v", err)
	}
	if d2.ID != 42 {
		t.Errorf("int id → %d, ожидалось 42", d2.ID)
	}

	// id отсутствует — не падаем.
	var d3 Document
	if err := json.Unmarshal([]byte(`{"title":"X"}`), &d3); err != nil {
		t.Fatalf("отсутствующий id ломает парсинг: %v", err)
	}

	// id строкой, но не число → используем как UID.
	var d4 Document
	if err := json.Unmarshal([]byte(`{"id":"abc-uuid","title":"X"}`), &d4); err != nil {
		t.Fatalf("не-числовая строка id ломает парсинг: %v", err)
	}
	if d4.UID != "abc-uuid" {
		t.Errorf("не-числовой id не сохранён как UID: %+v", d4)
	}
}

// Регрессия: создание document_group идёт на POST /document-groups,
// а не на POST /tree-entities. Раньше эндпоинт был неверным и Kaiten
// отвечал 400 "Treeentity should have required property 'source_entity_uid'".
func TestCreateDocumentGroup_PostsToDocumentGroups(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uid":"grp-uid-1","title":"Notes","entity_type":"document_group","parent_entity_uid":"root-uid"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "t")

	parent := "root-uid"
	got, err := c.CreateDocumentGroup(context.Background(), CreateGroupPayload{
		Title:           "Notes",
		ParentEntityUID: &parent,
	})
	if err != nil {
		t.Fatalf("CreateDocumentGroup: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method: %s, want POST", gotMethod)
	}
	if gotPath != "/api/latest/document-groups" {
		t.Errorf("path: %s, want /api/latest/document-groups", gotPath)
	}
	if gotBody["title"] != "Notes" {
		t.Errorf("title в теле: %v", gotBody["title"])
	}
	if gotBody["parent_entity_uid"] != "root-uid" {
		t.Errorf("parent_entity_uid в теле: %v", gotBody["parent_entity_uid"])
	}
	// entity_type больше НЕ передаём (это поле document-groups не принимает).
	if _, ok := gotBody["entity_type"]; ok {
		t.Errorf("entity_type не должен передаваться в body: %v", gotBody["entity_type"])
	}
	if got.UID != "grp-uid-1" {
		t.Errorf("UID из ответа потерян: %+v", got)
	}
	if got.EntityType != EntityTypeDocumentGroup {
		t.Errorf("EntityType должен быть document_group, получили %q", got.EntityType)
	}
}
