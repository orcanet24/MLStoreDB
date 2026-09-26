package db

import (
	"path/filepath"
	"testing"
)

func TestResetAuthPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reset.mlstore")

	s, err := OpenWithLock(path, Options{MasterKey: []byte("k"), LightKDF: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatalf("createRole: %v", err)
	}
	if err := s.CreateUser("admin", "secret1", []string{"admin"}); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	sess, err := s.Authenticate("admin", "secret1")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := sess.Insert("orders", Document{"_id": "o1", "total": 100}); err != nil {
		t.Fatal(err)
	}
	if !s.AuthActive() {
		t.Fatal("should be auth active")
	}
	if err := s.ResetAuth(); err != nil {
		t.Fatal("reset:", err)
	}
	if s.AuthActive() {
		t.Fatal("should NOT be auth active after reset")
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal("flush:", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// reabrir y verificar
	s2, err := OpenWithLock(path, Options{MasterKey: []byte("k"), LightKDF: true})
	if err != nil {
		t.Fatal("reopen:", err)
	}
	defer s2.Close()
	if s2.AuthActive() {
		t.Fatal("FAIL: auth still active after reopen — reset did not persist")
	}
	// los datos deben sobrevivir
	doc, err := s2.Get("orders", "o1")
	if err != nil {
		t.Fatal("data lost:", err)
	}
	if doc["total"] != float64(100) {
		t.Fatalf("data wrong: %v", doc)
	}
	// la API cruda funciona sin autenticación (modo permisivo)
	if err := s2.Insert("orders", Document{"_id": "o2", "total": 200}); err != nil {
		t.Fatal("permissive insert failed:", err)
	}
	n, _ := s2.Count("orders", nil)
	if n != 2 {
		t.Fatalf("expected 2 orders, got %d", n)
	}
}
