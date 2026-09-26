package db

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func authStore(t *testing.T) *Store {
	t.Helper()
	s := New()
	s.opts.LightKDF = true // argon2 rápido para los tests
	return s
}

// bootstrap crea el rol admin (rw sobre *), el rol reader (r sobre q con fieldDeny)
// y los usuarios admin/reader. Devuelve el store.
func bootstrap(t *testing.T) *Store {
	t.Helper()
	s := authStore(t)
	if err := s.CreateRole("admin", []Permission{
		{Collection: "*", Read: true, Write: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("reader", []Permission{
		{Collection: "q", Read: true, FieldDeny: []string{"salary"}},
		{Collection: "other", Read: true, Write: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("bob", "secret-reader-1", []string{"reader"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthPermissiveDefault(t *testing.T) {
	s := authStore(t)
	if s.AuthActive() {
		t.Fatal("auth must be off with no users")
	}
	if err := s.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("q", "1"); err != nil {
		t.Fatal(err)
	}
	names := s.Collections()
	if len(names) != 1 || names[0] != "q" {
		t.Errorf("collections = %v (system colls must not appear)", names)
	}
}

func TestCreateUserActivatesRBAC(t *testing.T) {
	s := authStore(t)
	if err := s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatal(err)
	}
	if s.AuthActive() {
		t.Fatal("role alone must not activate auth")
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if !s.AuthActive() {
		t.Fatal("first user must activate RBAC")
	}
	// la API cruda del Store ahora está denegada
	if _, err := s.Get("q", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Get = %v, want ErrUnauthorized", err)
	}
	if err := s.Insert("q", Document{"_id": "1"}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Insert = %v, want ErrUnauthorized", err)
	}
	if _, err := s.Find("q", nil, nil); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Find = %v, want ErrUnauthorized", err)
	}
	if _, err := s.Count("q", nil); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Count = %v, want ErrUnauthorized", err)
	}
	if err := s.Upsert("q", "1", Document{"_id": "1"}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Upsert = %v, want ErrUnauthorized", err)
	}
	if err := s.Update("q", "1", Document{"a": 1}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Update = %v, want ErrUnauthorized", err)
	}
	if err := s.Delete("q", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Delete = %v, want ErrUnauthorized", err)
	}
	var buf bytes.Buffer
	if err := s.ExportCSV("q", &buf); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("ExportCSV = %v, want ErrUnauthorized", err)
	}
	// colecciones del sistema ocultas
	for _, n := range s.Collections() {
		if isSystemColl(n) {
			t.Errorf("Collections leaked system coll %q", n)
		}
	}
}

func TestAuthenticateAndSessionCRUD(t *testing.T) {
	s := bootstrap(t)
	if _, err := s.Authenticate("root", "wrong-password"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad password = %v, want ErrUnauthorized", err)
	}
	if _, err := s.Authenticate("nobody", "x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("unknown user = %v, want ErrUnauthorized", err)
	}
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.User() != "root" {
		t.Errorf("user = %q", sess.User())
	}
	if got := sess.Roles(); len(got) != 1 || got[0] != "admin" {
		t.Errorf("roles = %v", got)
	}
	if err := sess.Insert("q", Document{"_id": "1", "x": 42}); err != nil {
		t.Fatal(err)
	}
	doc, err := sess.Get("q", "1")
	if err != nil {
		t.Fatal(err)
	}
	if x, ok := toFloat(doc["x"]); !ok || x != 42 {
		t.Errorf("Get x = %v", doc["x"])
	}
	if err := sess.Update("q", "1", Document{"x": 43}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Upsert("q", "2", Document{"_id": "2"}); err != nil {
		t.Fatal(err)
	}
	n, err := sess.Count("q", nil)
	if err != nil || n != 2 {
		t.Errorf("Count = %d, %v", n, err)
	}
	docs, err := sess.Find("q", nil, nil)
	if err != nil || len(docs) != 2 {
		t.Errorf("Find = %v, %v", docs, err)
	}
	if err := sess.Delete("q", "2"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionReadWithoutWrite(t *testing.T) {
	s := bootstrap(t)
	// sembrar a través de la sesión de admin
	admin, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Insert("q", Document{"_id": "1", "salary": 100}); err != nil {
		t.Fatal(err)
	}
	bob, err := s.Authenticate("bob", "secret-reader-1")
	if err != nil {
		t.Fatal(err)
	}
	// reader tiene Read sobre q → Get/Find funcionan
	if _, err := bob.Get("q", "1"); err != nil {
		t.Errorf("reader Get = %v", err)
	}
	if _, err := bob.Find("q", nil, nil); err != nil {
		t.Errorf("reader Find = %v", err)
	}
	// pero no Write
	if err := bob.Insert("q", Document{"_id": "9"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("reader Insert = %v, want ErrForbidden", err)
	}
	if err := bob.Update("q", "1", Document{"a": 1}); !errors.Is(err, ErrForbidden) {
		t.Errorf("reader Update = %v, want ErrForbidden", err)
	}
	if err := bob.Delete("q", "1"); !errors.Is(err, ErrForbidden) {
		t.Errorf("reader Delete = %v, want ErrForbidden", err)
	}
	// colección sin permiso de lectura ("zzz" no está en la lista de reader)
	if _, err := bob.Get("zzz", "1"); !errors.Is(err, ErrForbidden) {
		t.Errorf("reader Get zzz = %v, want ErrForbidden", err)
	}
	// reader tiene escritura sobre "other"
	if err := bob.Insert("other", Document{"_id": "1"}); err != nil {
		t.Errorf("reader other Insert = %v", err)
	}
}

func TestSessionFieldDeny(t *testing.T) {
	s := bootstrap(t)
	admin, _ := s.Authenticate("root", "secret-admin-1")
	if err := admin.Insert("q", Document{"_id": "1", "name": "Ana", "salary": 99999}); err != nil {
		t.Fatal(err)
	}
	bob, _ := s.Authenticate("bob", "secret-reader-1")
	doc, err := bob.Get("q", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := doc["salary"]; leaked {
		t.Error("fieldDeny leaked in Get")
	}
	if doc["name"] != "Ana" {
		t.Errorf("visible field missing: %v", doc)
	}
	docs, err := bob.Find("q", nil, nil)
	if err != nil || len(docs) != 1 {
		t.Fatalf("Find = %v, %v", docs, err)
	}
	if _, leaked := docs[0]["salary"]; leaked {
		t.Error("fieldDeny leaked in Find")
	}
	// los filtros que sondean un campo denegado → ErrForbidden
	if _, err := bob.Find("q", Document{"salary": Document{"$gt": 1}}, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("Find salary probe = %v, want ErrForbidden", err)
	}
	if _, err := bob.Count("q", Document{"salary": 1}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Count salary probe = %v, want ErrForbidden", err)
	}
	// sondeo lógico anidado
	f := Document{"$or": []any{Document{"salary": 1}, Document{"name": "Ana"}}}
	if _, err := bob.Find("q", f, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("Find $or salary probe = %v, want ErrForbidden", err)
	}
	// ExportCSV oculta el campo denegado
	var buf bytes.Buffer
	if err := bob.ExportCSV("q", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "99999") || strings.Contains(buf.String(), "salary") {
		t.Error("fieldDeny leaked in ExportCSV")
	}
	if !strings.Contains(buf.String(), "Ana") {
		t.Error("visible field missing in ExportCSV")
	}
	// admin (comodín, sin fieldDeny) ve salary
	adoc, err := admin.Get("q", "1")
	if err != nil || adoc["salary"] == nil {
		t.Errorf("admin should see salary: %v, %v", adoc, err)
	}
}

func TestSessionRevoke(t *testing.T) {
	s := bootstrap(t)
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	s.Revoke(sess.Token())
	if _, err := sess.Get("q", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked session Get = %v, want ErrUnauthorized", err)
	}
	if err := sess.Insert("q", Document{"_id": "2"}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked session Insert = %v, want ErrUnauthorized", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	s := authStore(t)
	s.opts.SessionTTL = 20 * time.Millisecond
	if err := s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := sess.Get("q", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("expired session Get = %v, want ErrUnauthorized", err)
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	s := bootstrap(t)
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword("root", "secret-admin-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("root", "secret-admin-1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("old password must fail: %v", err)
	}
	if _, err := s.Authenticate("root", "secret-admin-2"); err != nil {
		t.Errorf("new password must work: %v", err)
	}
	if _, err := sess.Get("q", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("session after ChangePassword = %v, want ErrUnauthorized", err)
	}
}

func TestSystemCollProtected(t *testing.T) {
	s := authStore(t)
	// incluso con la autenticación desactivada, el CRUD genérico no puede escribir en colecciones del sistema
	if err := s.Insert("_users", Document{"_id": "evil"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Insert _users = %v, want ErrForbidden", err)
	}
	if err := s.Insert("_roles", Document{"_id": "evil"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Insert _roles = %v, want ErrForbidden", err)
	}
	// con la autenticación activa, las sesiones tampoco pueden tocar colecciones del sistema
	if err := s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Get("_users", "root"); !errors.Is(err, ErrForbidden) {
		t.Errorf("session Get _users = %v, want ErrForbidden", err)
	}
	if err := sess.Insert("_users", Document{"_id": "x"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("session Insert _users = %v, want ErrForbidden", err)
	}
}

func TestCreateRoleValidation(t *testing.T) {
	s := authStore(t)
	if err := s.CreateUser("u", "password-123", []string{"ghost"}); err == nil {
		t.Error("CreateUser with unknown role must fail")
	}
	if err := s.CreateUser("", "password-123", nil); err == nil {
		t.Error("empty username must fail")
	}
	if err := s.CreateUser("u", "", nil); err == nil {
		t.Error("empty password must fail")
	}
	if err := s.CreateRole("", nil); err == nil {
		t.Error("empty role name must fail")
	}
	if err := s.CreateRole("r1", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("r1", nil); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate role = %v", err)
	}
}

func TestAuthPersistsReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()

	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("admin", []Permission{
		{Collection: "q", Read: true, Write: true, FieldDeny: []string{"secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Insert("q", Document{"_id": "1", "secret": "hide", "ok": "show"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.AuthActive() {
		t.Fatal("auth must stay active after reopen")
	}
	if _, err := s2.Get("q", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("raw Get after reopen = %v", err)
	}
	sess2, err := s2.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := sess2.Get("q", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := doc["secret"]; leaked {
		t.Error("fieldDeny lost after reopen")
	}
	if doc["ok"] != "show" {
		t.Errorf("doc = %v", doc)
	}
	// Collections sigue ocultando las colecciones del sistema
	for _, n := range s2.Collections() {
		if isSystemColl(n) {
			t.Errorf("leaked system coll %q", n)
		}
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}
