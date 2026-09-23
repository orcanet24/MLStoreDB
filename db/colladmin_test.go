package db

import (
	"path/filepath"
	"testing"
)

// M8 wire support: UpdateFields, CreateCollection, DropCollection (+Session
// admin variants) and the META-gated scan that prevents dropped collections
// from resurrecting from stale DOC/IDX records.

func TestUpdateFieldsPatchAndRemove(t *testing.T) {
	s := New()
	defer s.Close()
	if err := s.Insert("c", Document{"_id": "1", "a": 1, "b": 2, "c": 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFields("c", "1", Document{"a": 10, "d": 4}, []string{"b", "c"}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Get("c", "1")
	if err != nil {
		t.Fatal(err)
	}
	if doc["a"] != 10 || doc["d"] != 4 {
		t.Errorf("patch not applied: %v", doc)
	}
	if _, ok := doc["b"]; ok {
		t.Errorf("field b not removed")
	}
	if _, ok := doc["c"]; ok {
		t.Errorf("field c not removed")
	}
}

func TestUpdateFieldsHookSeesFinalShape(t *testing.T) {
	s := New()
	defer s.Close()
	seen := Document{}
	_, err := s.On("before_update", "c", func(hc *HookContext) error {
		seen = clone(hc.Doc)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("c", Document{"_id": "1", "keep": "y", "drop": "n"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFields("c", "1", Document{"keep": "z"}, []string{"drop"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen["drop"]; ok {
		t.Errorf("before hook should not see removed field: %v", seen)
	}
	if seen["keep"] != "z" {
		t.Errorf("before hook should see patched field: %v", seen)
	}
	doc, _ := s.Get("c", "1")
	if _, ok := doc["drop"]; ok {
		t.Errorf("drop field should be removed after hooks")
	}
}

func TestCreateCollection(t *testing.T) {
	s := New()
	defer s.Close()
	if err := s.CreateCollection("alpha"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range s.Collections() {
		if c == "alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("created collection not listed")
	}
	if err := s.CreateCollection("alpha"); err != ErrExists {
		t.Errorf("duplicate create: %v", err)
	}
	if err := s.CreateCollection("_users"); err != ErrForbidden {
		t.Errorf("system coll create: %v", err)
	}
	if err := s.CreateCollection(""); err != ErrForbidden {
		t.Errorf("empty name: %v", err)
	}
	if err := s.CreateCollection("$bad"); err != ErrForbidden {
		t.Errorf("$-prefixed name: %v", err)
	}
}

func TestDropCollection(t *testing.T) {
	s := New()
	defer s.Close()
	for _, id := range []string{"1", "2", "3"} {
		if err := s.Insert("c", Document{"_id": id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnsureIndex("c", []string{"x"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DropCollection("c"); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Collections()); n != 0 {
		t.Errorf("collection still listed: %v", s.Collections())
	}
	if _, err := s.Get("c", "1"); err == nil {
		t.Errorf("docs survived drop")
	}
	if err := s.DropCollection("c"); err != ErrNotFound {
		t.Errorf("drop missing: %v", err)
	}
	if err := s.DropCollection("_users"); err != ErrForbidden {
		t.Errorf("drop system coll: %v", err)
	}
}

func TestDropCollectionPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drop.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if err := s.Insert("gone", Document{"_id": id, "v": id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Insert("stay", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex("gone", []string{"v"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.DropCollection("gone"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	for _, c := range s3.Collections() {
		if c == "gone" {
			t.Errorf("dropped collection resurrected on reopen")
		}
	}
	if _, err := s3.Get("gone", "1"); err == nil {
		t.Errorf("dropped docs resurrected on reopen")
	}
	if _, err := s3.Get("stay", "1"); err != nil {
		t.Errorf("surviving collection damaged: %v", err)
	}
	if idxs, err := s3.ListIndexes("gone"); err == nil && len(idxs) > 0 {
		t.Errorf("dropped collection indexes resurrected: %v", idxs)
	}
}

func TestSessionAdminMethods(t *testing.T) {
	s := New()
	defer s.Close()
	if err := s.CreateRole("rw", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("ro", []Permission{{Collection: "pub", Read: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("writer", "pw", []string{"rw"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("reader", "pw", []string{"ro"}); err != nil {
		t.Fatal(err)
	}
	w, err := s.Authenticate("writer", "pw")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Authenticate("reader", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Insert("pub", Document{"_id": "1", "x": 1}); err != nil {
		t.Fatal(err)
	}
	if err := w.UpdateFields("pub", "1", Document{"y": 2}, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if err := w.EnsureIndex("pub", []string{"y"}, false); err != nil {
		t.Fatal(err)
	}
	idxs, err := w.ListIndexes("pub")
	if err != nil || len(idxs) != 1 {
		t.Fatalf("session ListIndexes: %v %v", idxs, err)
	}
	if err := w.CreateCollection("wcoll"); err != nil {
		t.Fatal(err)
	}
	if err := w.DropCollection("wcoll"); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateFields("pub", "1", nil, []string{"y"}); err != ErrForbidden {
		t.Errorf("reader updateFields: %v", err)
	}
	if err := r.EnsureIndex("pub", []string{"z"}, false); err != ErrForbidden {
		t.Errorf("reader ensureIndex: %v", err)
	}
	if err := r.CreateCollection("rcoll"); err != ErrForbidden {
		t.Errorf("reader createCollection: %v", err)
	}
	if _, err := r.ListIndexes("other"); err != ErrForbidden {
		t.Errorf("reader listIndexes (coll not readable): %v", err)
	}
	seen := map[string]bool{}
	for _, c := range r.Collections() {
		seen[c] = true
	}
	if !seen["pub"] {
		t.Errorf("reader should see pub")
	}
	if seen["wcoll"] {
		t.Errorf("reader should not see dropped coll")
	}
}
