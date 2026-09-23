package db

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportCSVBOMAndColumns(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "status": "OPEN", "n": 42, "refresh_token": "SECRET"})
	_ = s.Insert("q", Document{"_id": "2", "status": "CLOSED", "extra": "x"})

	var buf bytes.Buffer
	if err := s.ExportCSV("q", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\xEF\xBB\xBF") {
		t.Error("missing UTF-8 BOM")
	}
	if strings.Contains(out, "SECRET") {
		t.Error("sensitive field leaked")
	}
	if !strings.Contains(out, "_id") || !strings.Contains(out, "status") {
		t.Errorf("headers: %q", out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("number: %q", out)
	}
}

func TestExportCSVNestedJSON(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "payload": Document{"a": 1}})
	var buf bytes.Buffer
	if err := s.ExportCSV("q", &buf); err != nil {
		t.Fatal(err)
	}
	// CSV doubles quotes: "{""a"":1}"
	if !strings.Contains(buf.String(), `""a"":1`) && !strings.Contains(buf.String(), `{"a":1}`) {
		t.Errorf("nested: %q", buf.String())
	}
}

func TestExportCSVMissingColl(t *testing.T) {
	s := New()
	var buf bytes.Buffer
	if err := s.ExportCSV("nope", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "_id") {
		t.Errorf("empty header: %q", buf.String())
	}
}

func TestApplyMigrationsForward(t *testing.T) {
	s := New()
	ran := false
	err := s.ApplyMigrations([]Migration{
		{Version: 1, Name: "init", Up: func(st *Store) error {
			ran = true
			return st.Insert("core", Document{"_id": "v", "n": 1})
		}},
		{Version: 2, Name: "add field", Up: func(st *Store) error {
			return st.Update("core", "v", Document{"extra": true})
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("migration 1 did not run")
	}
	if s.SchemaVersion() != 2 {
		t.Errorf("version: %d", s.SchemaVersion())
	}
	d, _ := s.Get("core", "v")
	if d["extra"] != true {
		t.Errorf("doc: %v", d)
	}

	// re-apply is no-op
	if err := s.ApplyMigrations([]Migration{{Version: 2, Name: "again", Up: func(*Store) error {
		t.Error("should not re-run")
		return nil
	}}}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyMigrationsFutureRejected(t *testing.T) {
	s := New()
	s.schemaVersion = 5
	err := s.ApplyMigrations([]Migration{{Version: 2, Name: "old", Up: func(*Store) error { return nil }}})
	if err == nil || !strings.Contains(err.Error(), "update the program") {
		t.Fatalf("want future reject, got %v", err)
	}
}

func TestApplyMigrationsErrorPropagates(t *testing.T) {
	s := New()
	boom := errors.New("boom")
	err := s.ApplyMigrations([]Migration{
		{Version: 1, Name: "bad", Up: func(*Store) error { return boom }},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if s.SchemaVersion() != 0 {
		t.Errorf("version should stay 0 on failed first migration: %d", s.SchemaVersion())
	}
}

func TestMigrationsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()

	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ApplyMigrations([]Migration{
		{Version: 1, Name: "init", Up: func(st *Store) error {
			return st.EnsureIndex("things", []string{"kind"}, false)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("things", Document{"_id": "1", "kind": "a"})
	_ = s.Close()

	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.SchemaVersion() != 1 {
		t.Errorf("schema after reopen: %d", s2.SchemaVersion())
	}
	list, _ := s2.ListIndexes("things")
	if len(list) != 1 {
		t.Errorf("indexes: %v", list)
	}
}
