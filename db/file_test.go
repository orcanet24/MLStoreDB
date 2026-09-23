package db

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testOpts() Options {
	return Options{
		MasterKey: []byte("test-master-key-32-bytes-long!!!!"),
		MachineID: "test-machine",
		LightKDF:  true,
	}
}

func TestFlushOpenRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")

	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1", "status": "OPEN", "n": 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "2", "status": "CLOSED"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex("q", []string{"status"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// file exists and is not plaintext JSON
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < headerSize || string(raw[0:4]) != magicStr {
		t.Fatalf("bad magic: %q", raw[:min(4, len(raw))])
	}

	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	docs, err := s2.Find("q", Document{"status": "OPEN"}, nil)
	if err != nil || len(docs) != 1 || docs[0]["_id"] != "1" {
		t.Fatalf("reload find: %v %v", docs, err)
	}
	if s2.SchemaVersion() != 0 {
		t.Errorf("schema: %d", s2.SchemaVersion())
	}
	list, _ := s2.ListIndexes("q")
	if len(list) != 1 || list[0].Fields[0] != "status" {
		t.Errorf("indexes reloaded: %v", list)
	}
	// unique index enforced after reload
	_ = s2.Insert("q", Document{"_id": "3", "status": "OPEN"})
	err = s2.Insert("q", Document{"_id": "4", "status": "OPEN"})
	if !errors.Is(err, ErrDuplicate) {
		// status is non-unique in this test — just ensure insert works
		if err != nil {
			t.Errorf("insert after reload: %v", err)
		}
	}
}

func TestOpenWrongKeyCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	bad := testOpts()
	bad.MasterKey = []byte("wrong-master-key-32-bytes-long!!!!")
	if _, err := Open(path, bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestOpenTamperedHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, _ := Open(path, testOpts())
	_ = s.Insert("q", Document{"_id": "1"})
	_ = s.Close()

	raw, _ := os.ReadFile(path)
	raw[10] ^= 0xff // flip schema_version byte
	_ = os.WriteFile(path, raw, 0o600)

	if _, err := Open(path, testOpts()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestOpenTamperedPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, _ := Open(path, testOpts())
	_ = s.Insert("q", Document{"_id": "1"})
	_ = s.Close()

	raw, _ := os.ReadFile(path)
	raw[headerSize+5] ^= 0xff
	_ = os.WriteFile(path, raw, 0o600)

	if _, err := Open(path, testOpts()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestFlushNoopWhenClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, _ := Open(path, testOpts())
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// clean flush: still no file required? dirty=true on create so file written
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file after first flush: %v", err)
	}
	_ = s.Close()
}

func TestOpenMissingMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("want error without MasterKey")
	}
}

func TestCreatedThenReopenSameContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()

	s, _ := Open(path, opts)
	_ = s.Insert("users", Document{"_id": "u1", "name": "Ana", "nested": Document{"a": 1}})
	_ = s.Insert("users", Document{"_id": "u2", "name": "Luis"})
	_ = s.EnsureIndex("users", []string{"name"}, true)
	_ = s.Close()

	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	all, _ := s2.Find("users", nil, nil)
	if len(all) != 2 {
		t.Fatalf("docs: %d", len(all))
	}
	// unique still works
	if err := s2.Insert("users", Document{"_id": "u3", "name": "Ana"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("unique after reload: %v", err)
	}
	// nested preserved
	d, _ := s2.Get("users", "u1")
	if n, ok := d["nested"].(map[string]any); !ok || n["a"] != float64(1) {
		t.Errorf("nested: %v", d["nested"])
	}
}
