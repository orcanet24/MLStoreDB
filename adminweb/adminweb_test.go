package adminweb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"mlstoredb/db"
)

type fakeResponseWriter struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func (w *fakeResponseWriter) Header() http.Header { return w.header }
func (w *fakeResponseWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return w.body.Write(b)
}
func (w *fakeResponseWriter) WriteHeader(code int) { w.code = code }

type testEnv struct {
	t        *testing.T
	store    *db.Store
	srv      *Server
	base     string
	client   *http.Client
	csrf     string
	loggedIn bool
}

func newTestEnv(t *testing.T) *testEnv {
	return newTestEnvWithStore(t, db.New())
}

func newTestEnvWithStore(t *testing.T, store *db.Store) *testEnv {
	t.Helper()
	return newTestEnvWithOptions(t, store, ServerOptions{})
}

// newTestEnvWithOptions permite inyectar opciones del servidor (p. ej. un Logger para
// capturar los diagnósticos en los tests).
func newTestEnvWithOptions(t *testing.T, store *db.Store, opts ServerOptions) *testEnv {
	t.Helper()
	srv := NewServer(store, opts)
	ts := &testEnv{t: t, store: store, srv: srv, base: "http://test"}
	ts.client = &http.Client{
		Transport: &fakeTransport{handler: srv.Handler()},
		Jar:       newJar(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(func() { _ = store.Close() })
	return ts
}

type fakeTransport struct {
	handler http.Handler
}

func (ft *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	w := &fakeResponseWriter{header: http.Header{}, body: &bytes.Buffer{}}
	ft.handler.ServeHTTP(w, req)
	return &http.Response{
		StatusCode: w.code,
		Header:     w.header,
		Body:       io.NopCloser(w.body),
		Request:    req,
	}, nil
}

func newJar() *cookiejar.Jar {
	jar, _ := cookiejar.New(nil)
	return jar
}

func (te *testEnv) do(method, path string, body any, withCSRF bool) (int, map[string]any) {
	te.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, te.base+path, rdr)
	if err != nil {
		te.t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withCSRF && te.csrf != "" {
		req.Header.Set("X-CSRF-Token", te.csrf)
	}
	res, err := te.client.Do(req)
	if err != nil {
		te.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (te *testEnv) login(user, pass string) int {
	te.t.Helper()
	code, out := te.do("POST", "/api/login", map[string]string{"user": user, "pass": pass}, false)
	if v, ok := out["csrf"].(string); ok {
		te.csrf = v
		te.loggedIn = code == 200
	}
	return code
}

func (te *testEnv) setupAndLogin() {
	te.t.Helper()
	code, out := te.do("GET", "/api/status", nil, false)
	if code != 200 {
		te.t.Fatalf("status: %d", code)
	}
	if out["needsSetup"] != true {
		te.t.Fatalf("expected needsSetup")
	}
	code, _ = te.do("POST", "/api/setup", map[string]string{"user": "admin", "pass": "secreto1"}, false)
	if code != 200 {
		te.t.Fatalf("setup: %d", code)
	}
	if !te.store.AuthActive() {
		te.t.Fatalf("RBAC should be active after setup")
	}
	if code := te.login("admin", "secreto1"); code != 200 {
		te.t.Fatalf("login after setup: %d", code)
	}
}

func TestSetupFirstUserAndLogin(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	code, out := te.do("GET", "/api/me", nil, false)
	if code != 200 || out["user"] != "admin" {
		t.Fatalf("me: %d %v", code, out)
	}
	code, _ = te.do("POST", "/api/setup", map[string]string{"user": "x", "pass": "yyyyyy"}, false)
	if code != 409 {
		t.Errorf("second setup should 409, got %d", code)
	}
}

func TestLoginFailAndRateLimit(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/logout", nil, true)
	for i := 0; i < 5; i++ {
		if code := te.login("admin", "wrong"); code != 401 {
			t.Fatalf("attempt %d: expected 401, got %d", i, code)
		}
	}
	if code := te.login("admin", "secreto1"); code != 429 {
		t.Errorf("expected 429 after 5 failures, got %d", code)
	}
}

func TestUnauthorizedAndCSRF(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	if code, _ := te.do("GET", "/api/stats", nil, false); code != 200 {
		t.Errorf("permissive stats without login should 200, got %d", code)
	}
	if code, _ := te.do("POST", "/api/collections", map[string]string{"name": "x"}, false); code != 403 {
		t.Errorf("mutation without CSRF should 403, got %d", code)
	}
	if code, _ := te.do("POST", "/api/collections", map[string]string{"name": "x"}, true); code != 201 {
		t.Errorf("mutation with CSRF should 201, got %d", code)
	}
}

func TestAuthRequiredWhenActive(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	if code, _ := te.do("POST", "/api/logout", nil, true); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := te.do("GET", "/api/stats", nil, false); code != 401 {
		t.Errorf("stats after logout should 401, got %d", code)
	}
}

func TestCollectionsCRUD(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	if code, out := te.do("POST", "/api/collections", map[string]string{"name": "orders"}, true); code != 201 || out["name"] != "orders" {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, _ := te.do("POST", "/api/collections", map[string]string{"name": "orders"}, true); code != 409 {
		t.Errorf("duplicate should 409, got %d", code)
	}
	if code, _ := te.do("POST", "/api/collections", map[string]string{"name": "$bad"}, true); code != 400 {
		t.Errorf("bad name should 400, got %d", code)
	}
	code, out := te.do("GET", "/api/collections", nil, false)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	list, _ := out["collections"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "orders" {
		t.Fatalf("list wrong: %v", list)
	}
	if code, _ := te.do("DELETE", "/api/collections/orders", nil, true); code != 200 {
		t.Errorf("drop: %d", code)
	}
	if code, _ := te.do("DELETE", "/api/collections/orders", nil, true); code != 404 {
		t.Errorf("drop missing should 404, got %d", code)
	}
}

func TestDocsCRUDFlow(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "orders"}, true)

	code, out := te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "o1", "status": "OPEN", "total": 100.0}, true)
	if code != 201 || out["id"] != "o1" {
		t.Fatalf("insert: %d %v", code, out)
	}
	if code, _ := te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "o1"}, true); code != 409 {
		t.Errorf("duplicate _id should 409, got %d", code)
	}
	if code, _ := te.do("POST", "/api/collections/orders/docs", db.Document{"_id": 42}, true); code != 400 {
		t.Errorf("non-string _id should 400, got %d", code)
	}
	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "o2", "status": "PAID", "total": 250.0}, true)
	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "o3", "status": "PAID", "total": 50.0}, true)

	code, out = te.do("GET", "/api/collections/orders/docs?limit=2&sort=total&dir=-1", nil, false)
	if code != 200 {
		t.Fatalf("browse: %d", code)
	}
	if out["total"] != float64(3) {
		t.Errorf("total: %v", out["total"])
	}
	docs := out["docs"].([]any)
	if len(docs) != 2 {
		t.Fatalf("limit: %d", len(docs))
	}
	if docs[0].(map[string]any)["_id"] != "o2" {
		t.Errorf("sort desc wrong: %v", docs[0])
	}

	filter := url.QueryEscape(`{"status":"PAID"}`)
	code, out = te.do("GET", "/api/collections/orders/docs?filter="+filter, nil, false)
	docs = out["docs"].([]any)
	if out["total"] != float64(2) || len(docs) != 2 {
		t.Errorf("filter: %v", out)
	}

	code, out = te.do("GET", "/api/collections/orders/doc?id=o1", nil, false)
	if code != 200 || out["doc"].(map[string]any)["status"] != "OPEN" {
		t.Fatalf("get: %d %v", code, out)
	}

	code, _ = te.do("PUT", "/api/collections/orders/doc?id=o1", db.Document{"_id": "o1", "status": "CLOSED"}, true)
	if code != 200 {
		t.Fatalf("replace: %d", code)
	}
	code, out = te.do("GET", "/api/collections/orders/doc?id=o1", nil, false)
	if code != 200 || out["doc"].(map[string]any)["status"] != "CLOSED" {
		t.Fatalf("replace not applied: %d %v", code, out)
	}

	if code, _ := te.do("PUT", "/api/collections/orders/doc?id=o1", db.Document{"_id": "otro"}, true); code != 400 {
		t.Errorf("_id mismatch should 400, got %d", code)
	}

	if code, _ := te.do("DELETE", "/api/collections/orders/doc?id=o3", nil, true); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if _, err := te.store.Get("orders", "o3"); err == nil {
		t.Errorf("doc survived delete")
	}
	if code, _ := te.do("DELETE", "/api/collections/orders/doc?id=zzz", nil, true); code != 404 {
		t.Errorf("delete missing should 404, got %d", code)
	}
}

// TestBrowseFields cubre el descubrimiento de esquema (GET .../fields) que
// alimenta las columnas dinámicas y los filtros guiados de la consola.
func TestBrowseFields(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	if code, out := te.do("POST", "/api/collections", map[string]string{"name": "products"}, true); code != 201 {
		t.Fatalf("create coll: %d %v", code, out)
	}
	docs := []db.Document{
		{"_id": "p1", "sku": "A-1", "price": 10.0, "item": db.Document{"name": "Caja"}, "tags": []any{"new"}},
		{"_id": "p2", "sku": "A-2", "price": 20.0, "active": true},
	}
	for _, d := range docs {
		if code, out := te.do("POST", "/api/collections/products/docs", d, true); code != 201 {
			t.Fatalf("insert %v: %d %v", d["_id"], code, out)
		}
	}
	code, out := te.do("GET", "/api/collections/products/fields?maxScan=100", nil, false)
	if code != 200 {
		t.Fatalf("fields: %d %v", code, out)
	}
	fields, _ := out["fields"].([]any)
	names := map[string]bool{}
	for _, f := range fields {
		names[fmt.Sprint(f)] = true
	}
	for _, want := range []string{"_id", "sku", "price", "item", "tags", "active"} {
		if !names[want] {
			t.Errorf("fields missing %q: %v", want, fields)
		}
	}
	if len(fields) == 0 || fmt.Sprint(fields[0]) != "_id" {
		t.Errorf("_id should be first: %v", fields)
	}
	paths, _ := out["paths"].([]any)
	pnames := map[string]bool{}
	for _, p := range paths {
		pnames[fmt.Sprint(p)] = true
	}
	if !pnames["item.name"] {
		t.Errorf("paths missing item.name: %v", paths)
	}
	types, _ := out["types"].(map[string]any)
	if types == nil || fmt.Sprint(types["price"]) != "number" || fmt.Sprint(types["sku"]) != "string" {
		t.Errorf("types: %v", types)
	}
	if sc, _ := out["scanned"].(float64); int(sc) != len(docs) {
		t.Errorf("scanned: %v", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); tr {
		t.Errorf("truncated should be false: %v", out)
	}
	// maxScan=1 ⇒ escaneo parcial marcado como truncated
	code, out = te.do("GET", "/api/collections/products/fields?maxScan=1", nil, false)
	if code != 200 {
		t.Fatalf("fields maxScan=1: %d %v", code, out)
	}
	if sc, _ := out["scanned"].(float64); int(sc) != 1 {
		t.Errorf("scanned with maxScan=1: %v", out["scanned"])
	}
	if tr, _ := out["truncated"].(bool); !tr {
		t.Errorf("truncated should be true: %v", out)
	}
	// colección inexistente ⇒ 200 con lista vacía (mismo trato que /docs)
	code, out = te.do("GET", "/api/collections/nofile/fields", nil, false)
	if code != 200 {
		t.Errorf("missing coll: %d", code)
	}
	if fs, _ := out["fields"].([]any); len(fs) != 0 {
		t.Errorf("missing coll fields: %v", fs)
	}
}

func TestReadOnlyUserPermissions(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "pub"}, true)
	te.do("POST", "/api/collections/pub/docs", db.Document{"_id": "1", "secret": "no"}, true)

	te.store.CreateRole("viewer", []db.Permission{{Collection: "pub", Read: true}})
	te.store.CreateUser("viewer", "pass123", []string{"viewer"})

	te2 := newTestEnvWithStore(t, te.store)
	if code := te2.login("viewer", "pass123"); code != 200 {
		t.Fatalf("viewer login: %d", code)
	}
	if code, out := te2.do("GET", "/api/collections/pub/docs", nil, false); code != 200 {
		t.Fatalf("viewer browse: %d %v", code, out)
	}
	if code, _ := te2.do("POST", "/api/collections/pub/docs", db.Document{"_id": "2"}, true); code != 403 {
		t.Errorf("viewer insert should 403, got %d", code)
	}
	if code, _ := te2.do("POST", "/api/collections", map[string]string{"name": "hack"}, true); code != 403 {
		t.Errorf("viewer create coll should 403, got %d", code)
	}
	if code, _ := te2.do("DELETE", "/api/collections/pub", nil, true); code != 403 {
		t.Errorf("viewer drop should 403, got %d", code)
	}
	_, out := te2.do("GET", "/api/collections", nil, false)
	list, _ := out["collections"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "pub" {
		t.Errorf("viewer should only see pub: %v", list)
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	req, _ := http.NewRequest("GET", te.base+"/", nil)
	res, err := te.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("missing CSP")
	}
	if res.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("missing X-Frame-Options")
	}
	if res.StatusCode != 200 {
		t.Fatalf("static index: %d", res.StatusCode)
	}
	buf := make([]byte, 512)
	n, _ := res.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "MLS") {
		t.Errorf("index content unexpected: %q", string(buf[:n]))
	}
	req2, _ := http.NewRequest("GET", te.base+"/app.js", nil)
	res2, err := te.client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != 200 {
		t.Errorf("app.js: %d", res2.StatusCode)
	}
	if ct := res2.Header.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("app.js Content-Type: %q (Windows registry MIME bug regression)", ct)
	}
	// Los assets se sirven con no-cache + ETag: el navegador revalida y nunca
	// se queda con un app.js/css viejo tras recompilar el binario.
	if cc := res2.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("app.js Cache-Control: %q", cc)
	}
	if res2.Header.Get("ETag") == "" {
		t.Errorf("app.js missing ETag")
	}
	req3, _ := http.NewRequest("GET", te.base+"/app.css", nil)
	res3, err := te.client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if ct := res3.Header.Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("app.css Content-Type: %q (Windows registry MIME bug regression)", ct)
	}
}

func TestStats(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "a"}, true)
	te.do("POST", "/api/collections/a/docs", db.Document{"_id": "1"}, true)
	code, out := te.do("GET", "/api/stats", nil, false)
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if out["totalDocs"] != float64(1) {
		t.Errorf("totalDocs: %v", out["totalDocs"])
	}
	if out["authActive"] != true {
		t.Errorf("authActive: %v", out["authActive"])
	}
}

func TestIndexesAdmin(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "users"}, true)
	code, _ := te.do("POST", "/api/collections/users/indexes", map[string]any{"fields": []string{"email"}, "unique": true}, true)
	if code != 201 {
		t.Fatalf("create index: %d", code)
	}
	code, out := te.do("GET", "/api/collections/users/indexes", nil, false)
	if code != 200 {
		t.Fatalf("list indexes: %d", code)
	}
	idxs := out["indexes"].([]any)
	if len(idxs) != 1 {
		t.Fatalf("expected 1 index: %v", idxs)
	}
	ix := idxs[0].(map[string]any)
	if ix["unique"] != true {
		t.Errorf("unique flag lost: %v", ix)
	}
	code, _ = te.do("POST", "/api/collections/users/docs", db.Document{"_id": "1", "email": "a@x"}, true)
	if code != 201 {
		t.Fatalf("insert with index: %d", code)
	}
	code, _ = te.do("POST", "/api/collections/users/docs", db.Document{"_id": "2", "email": "a@x"}, true)
	if code != 409 {
		t.Errorf("unique violation should 409, got %d", code)
	}
	if code, _ := te.do("DELETE", "/api/collections/users/indexes?fields=email", nil, true); code != 200 {
		t.Errorf("drop index: %d", code)
	}
	code, _ = te.do("POST", "/api/collections/users/docs", db.Document{"_id": "2", "email": "a@x"}, true)
	if code != 201 {
		t.Errorf("insert after drop should pass, got %d", code)
	}
}

func TestExportFormats(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "ex"}, true)
	te.do("POST", "/api/collections/ex/docs", db.Document{"_id": "1", "n": 10.5, "s": "hola"}, true)
	te.do("POST", "/api/collections/ex/docs", db.Document{"_id": "2", "n": 20.0, "s": "mundo"}, true)

	get := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", te.base+path, nil)
		res, err := te.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	code, body := get("/api/collections/ex/export?format=json")
	if code != 200 || !strings.Contains(body, `"hola"`) || !strings.HasPrefix(body, "[") {
		t.Errorf("json export: %d %q", code, body)
	}
	code, body = get("/api/collections/ex/export?format=ndjson")
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if code != 200 || len(lines) != 2 {
		t.Errorf("ndjson export: %d lines=%d", code, len(lines))
	}
	code, body = get("/api/collections/ex/export?format=csv")
	if code != 200 {
		t.Fatalf("csv export: %d", code)
	}
	if !strings.Contains(body, "\xEF\xBB\xBF") {
		t.Errorf("csv should have BOM")
	}
	if !strings.Contains(body, "hola") || !strings.Contains(body, "10.5") {
		t.Errorf("csv content: %q", body)
	}
	filter := url.QueryEscape(`{"_id":"2"}`)
	code, body = get("/api/collections/ex/export?format=json&filter=" + filter)
	if code != 200 || strings.Contains(body, "hola") || !strings.Contains(body, "mundo") {
		t.Errorf("filtered export: %d %q", code, body)
	}
}

func TestImportFormats(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "imp"}, true)

	csvData := "_id,nombre,activo,nota,n\n1,Ana,true,hola mundo,1\n2,Bo,false,xyz,2\n"
	previewBody := strings.NewReader(csvData)
	code, out := postImport(te, "imp", "csv", "insert", previewBody, true)
	if code != 200 || out["total"] != float64(2) {
		t.Fatalf("csv preview: %d %v", code, out)
	}

	code, out = postImport(te, "imp", "csv", "insert", strings.NewReader(csvData), false)
	if code != 202 {
		t.Fatalf("csv import: %d %v", code, out)
	}
	jobID, _ := out["jobId"].(string)
	if out["status"] != "running" || jobID == "" {
		t.Fatalf("expected jobId+running: %v", out)
	}
	for i := 0; i < 50; i++ {
		code, st := te.do("GET", "/api/import/"+jobID, nil, false)
		if code != 200 {
			t.Fatalf("status: %d", code)
		}
		if st["status"] == "done" || st["status"] == "error" {
			if st["inserted"] != float64(2) {
				t.Fatalf("import inserted: %v", st)
			}
			goto doneCSV
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("csv import job did not finish")
doneCSV:
	code, out = te.do("GET", "/api/collections/imp/doc?id=1", nil, false)
	doc := out["doc"].(map[string]any)
	if doc["nombre"] != "Ana" || doc["activo"] != true || doc["nota"] != "hola mundo" {
		t.Fatalf("csv values: %v", doc)
	}
	if doc["n"].(float64) != 1 {
		t.Errorf("csv number inference: %v", doc["n"])
	}

	jsonData := `[{"_id":"3","nombre":"Carla"},{"_id":"1","nombre":"Ana2"}]`
	code, out = postImport(te, "imp", "json", "insert", strings.NewReader(jsonData), false)
	if code != 202 {
		t.Fatalf("json import: %d", code)
	}
	j2, _ := out["jobId"].(string)
	for i := 0; i < 50; i++ {
		_, st := te.do("GET", "/api/import/"+j2, nil, false)
		if st["status"] == "done" || st["status"] == "error" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	code, out = postImport(te, "imp", "json", "upsert", strings.NewReader(jsonData), false)
	if code != 202 {
		t.Fatalf("json upsert: %d", code)
	}
	j3, _ := out["jobId"].(string)
	for i := 0; i < 50; i++ {
		_, st := te.do("GET", "/api/import/"+j3, nil, false)
		if st["status"] == "done" || st["status"] == "error" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	code, out = te.do("GET", "/api/collections/imp/doc?id=1", nil, false)
	if out["doc"].(map[string]any)["nombre"] != "Ana2" {
		t.Errorf("upsert not applied")
	}

	ndData := "{\"_id\":\"4\",\"nombre\":\"Dave\"}\n{\"_id\":\"5\",\"nombre\":\"Eve\"}\n"
	code, out = postImport(te, "imp", "ndjson", "insert", strings.NewReader(ndData), false)
	if code != 202 {
		t.Fatalf("ndjson import: %d", code)
	}
	code, out = postImport(te, "imp", "json", "insert", strings.NewReader("{bad json"), false)
	if code != 202 {
		t.Fatalf("bad json: %d", code)
	}
	jErr, _ := out["jobId"].(string)
	for i := 0; i < 50; i++ {
		_, st := te.do("GET", "/api/import/"+jErr, nil, false)
		if st["status"] == "done" || st["status"] == "error" {
			if st["errors"] == float64(0) && st["error"] == "" {
				t.Errorf("bad json should report errors in job: %v", st)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func postImport(te *testEnv, coll, format, mode string, body io.Reader, preview bool) (int, map[string]any) {
	params := newURLParams(format, mode, preview)
	url := fmt.Sprintf("http://test/api/collections/%s/import?%s", coll, params.Encode())
	req, _ := http.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", te.csrf)
	res, err := te.client.Do(req)
	if err != nil {
		te.t.Fatalf("post import: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func newURLParams(format, mode string, preview bool) url.Values {
	p := url.Values{}
	p.Set("format", format)
	p.Set("mode", mode)
	if preview {
		p.Set("preview", "1")
	}
	return p
}

func TestTriggersAdmin(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "orders"}, true)
	te.do("POST", "/api/collections", map[string]string{"name": "audit"}, true)

	code, out := te.do("PUT", "/api/triggers", map[string]any{
		"_id":        "audit_paid",
		"event":      "after_insert",
		"collection": "orders",
		"enabled":    true,
		"filter":     map[string]any{"status": "PAID"},
		"actions":    []any{map[string]any{"type": "insert", "collection": "audit", "doc": map[string]any{"ref": map[string]any{"$get": "_id"}}}},
	}, true)
	if code != 200 || out["id"] != "audit_paid" {
		t.Fatalf("create trigger: %d %v", code, out)
	}
	code, out = te.do("GET", "/api/triggers", nil, false)
	triggers := out["triggers"].([]any)
	if len(triggers) != 1 {
		t.Fatalf("list triggers: %v", triggers)
	}

	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "o1", "status": "PAID"}, true)
	te.store.WaitAsyncHooks()
	code, out = te.do("GET", "/api/collections/audit/docs", nil, false)
	docs := out["docs"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["ref"] != "o1" {
		t.Fatalf("trigger did not fire: %v", docs)
	}

	code, _ = te.do("PUT", "/api/triggers", map[string]any{
		"_id": "audit_paid", "event": "after_insert", "collection": "orders",
		"enabled": false,
		"actions": []any{map[string]any{"type": "insert", "collection": "audit", "doc": map[string]any{"ref": map[string]any{"$get": "_id"}}}},
	}, true)
	if code != 200 {
		t.Fatalf("disable trigger: %d", code)
	}
	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "o2", "status": "PAID"}, true)
	te.store.WaitAsyncHooks()
	code, out = te.do("GET", "/api/collections/audit/docs", nil, false)
	if len(out["docs"].([]any)) != 1 {
		t.Errorf("disabled trigger should not fire: %v", out)
	}

	if code, _ := te.do("DELETE", "/api/triggers/audit_paid", nil, true); code != 200 {
		t.Fatalf("delete trigger: %d", code)
	}
	code, out = te.do("GET", "/api/triggers", nil, false)
	if len(out["triggers"].([]any)) != 0 {
		t.Errorf("trigger survived delete")
	}
}

func TestTriggerPermissions(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "pub"}, true)
	te.do("POST", "/api/collections", map[string]string{"name": "priv"}, true)
	te.store.CreateRole("pubonly", []db.Permission{{Collection: "pub", Read: true, Write: true}})
	te.store.CreateUser("puser", "pass123", []string{"pubonly"})

	te2 := newTestEnvWithStore(t, te.store)
	te2.login("puser", "pass123")
	code, _ := te2.do("PUT", "/api/triggers", map[string]any{
		"event": "after_insert", "collection": "priv", "enabled": true,
		"actions": []any{map[string]any{"type": "set", "fields": map[string]any{"x": 1}}},
	}, true)
	if code != 403 {
		t.Errorf("trigger on forbidden coll should 403, got %d", code)
	}
	code, _ = te2.do("PUT", "/api/triggers", map[string]any{
		"event": "after_insert", "collection": "pub", "enabled": true,
		"actions": []any{map[string]any{"type": "set", "fields": map[string]any{"x": 1}}},
	}, true)
	if code != 200 {
		t.Errorf("trigger on allowed coll should 200, got %d", code)
	}
}

// Una base sin usuarios todavía no tiene credenciales que validar: la consola debe decirlo
// (409 + "no users") en lugar de responder "invalid credentials".
func TestLoginOnDatabaseWithoutUsersAsksForSetup(t *testing.T) {
	te := newTestEnv(t) // store vacío: AuthActive() == false
	code, out := te.do("POST", "/api/login", map[string]string{"user": "admin", "pass": "secreto1"}, false)
	if code != http.StatusConflict {
		t.Fatalf("login sin usuarios debería dar 409, dio %d (%v)", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "no users") {
		t.Fatalf("error debería pedir el setup inicial: %v", out)
	}
}

// El motivo real del fallo de login (usuario inexistente vs contraseña incorrecta) debe
// quedar en el log del servidor: antes se descartaba y sólo se veía "invalid credentials".
func TestLoginFailureLogsRealReason(t *testing.T) {
	var logBuf bytes.Buffer
	te := newTestEnvWithOptions(t, db.New(), ServerOptions{Logger: log.New(&logBuf, "", 0)})
	if code, _ := te.do("POST", "/api/setup", map[string]string{"user": "admin", "pass": "secreto1"}, false); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	logBuf.Reset()

	if code, _ := te.do("POST", "/api/login", map[string]string{"user": "admin", "pass": "malaClave"}, false); code != http.StatusUnauthorized {
		t.Fatalf("contraseña incorrecta debería dar 401, dio %d", code)
	}
	if !strings.Contains(logBuf.String(), "wrong password") {
		t.Fatalf("log sin el motivo (usuario existe): %q", logBuf.String())
	}

	logBuf.Reset()
	if code, _ := te.do("POST", "/api/login", map[string]string{"user": "nadie", "pass": "malaClave"}, false); code != http.StatusUnauthorized {
		t.Fatalf("usuario inexistente debería dar 401, dio %d", code)
	}
	if !strings.Contains(logBuf.String(), "does not exist in this database _users") {
		t.Fatalf("log sin el motivo (usuario inexistente): %q", logBuf.String())
	}
}
