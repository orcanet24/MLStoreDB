package db

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestFlushSyncPersistsImmediately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s := openTest(t, path)
	if err := s.Insert("q", Document{"_id": "1", "status": "paid"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	dirty := s.dirty
	s.mu.RUnlock()
	if dirty {
		t.Fatal("expected clean after FlushSync")
	}
	// File must already contain the doc without Close/auto-flush.
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	docs, err := s2.Find("q", Document{"status": "paid"}, nil)
	if err != nil || len(docs) != 1 {
		t.Fatalf("reopen after FlushSync: %v %v", docs, err)
	}
	_ = s.Close()
}

func TestSyncOnWriteAutoPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	opts.SyncOnWrite = true
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("tokens", Document{"_id": "acc1", "access_token": "tok"}); err != nil {
		t.Fatal(err)
	}
	// Mutation returned ⇒ already durable (no Close/Flush/manual call).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < headerSize {
		t.Fatalf("file too small: %d", len(raw))
	}
	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	d, err := s2.Get("tokens", "acc1")
	if err != nil || d["access_token"] != "tok" {
		t.Fatalf("get after SyncOnWrite: %v %v", d, err)
	}
	_ = s.Close()
}

func TestSyncOnWriteFailedMutationDoesNotFlush(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	opts.SyncOnWrite = true
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
}

func TestCorruptMainSnapshotRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	bak := filepath.Join(dir, "bak.mlstore")

	s := openTest(t, path)
	if err := s.Insert("q", Document{"_id": "1", "keep": true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(bak); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[headerSize+10] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, testOpts()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt main: want ErrCorrupt, got %v", err)
	}
	// Snapshot must still open cleanly.
	s2, err := Open(bak, testOpts())
	if err != nil {
		t.Fatalf("snapshot open: %v", err)
	}
	defer s2.Close()
	d, err := s2.Get("q", "1")
	if err != nil || d["keep"] != true {
		t.Fatalf("snapshot content: %v %v", d, err)
	}
	// Repair cannot fix GCM failure.
	if _, err := Repair(path, testOpts()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("repair corrupt payload: want ErrCorrupt, got %v", err)
	}
}

func TestTruncatedFileErrCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s := openTest(t, path)
	_ = s.Insert("q", Document{"_id": "1"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, raw[:20], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, testOpts()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncate open: %v", err)
	}
	if _, err := Repair(path, testOpts()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncate repair: %v", err)
	}
}

func TestRepairInvalidIDType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")

	s := openTest(t, path)
	if err := s.Insert("q", Document{"_id": "good", "n": 1}); err != nil {
		t.Fatal(err)
	}
	// Bypass API: non-string _id in the file.
	s.mu.Lock()
	s.collections["q"].entries["bad"] = newDocEntry(Document{"_id": float64(42), "n": 2})
	s.markDirty()
	s.collections["q"].entries["bad"].dirty.Store(true)
	s.collections["q"].entries["bad"].mutGen = s.dirtyGen
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, testOpts()); !errors.Is(err, ErrNoID) {
		t.Fatalf("open invalid id: want ErrNoID, got %v", err)
	}

	rep, err := Repair(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.DocsDropped != 1 || rep.DocsKept != 1 || !rep.Resaved {
		t.Fatalf("report: %+v", rep)
	}

	s2 := openTest(t, path)
	defer s2.Close()
	if _, err := s2.Get("q", "good"); err != nil {
		t.Fatalf("good doc survived: %v", err)
	}
	if _, err := s2.Get("q", "42"); !errors.Is(err, ErrNotFound) && err == nil {
		t.Fatalf("bad doc should be gone: %v", err)
	}
	all, _ := s2.Find("q", nil, nil)
	if len(all) != 1 {
		t.Fatalf("docs after repair: %d", len(all))
	}
	// Indexes rebuilt: unique on n still works if we add one
	if err := s2.EnsureIndex("q", []string{"n"}, true); err != nil {
		t.Fatal(err)
	}
}

func TestRepairUniqueConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")

	s := openTest(t, path)
	if err := s.Insert("q", Document{"_id": "a", "status": "dup"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex("q", []string{"status"}, true); err != nil {
		t.Fatal(err)
	}
	// Inject second doc with same unique key without index entry.
	s.mu.Lock()
	s.collections["q"].entries["b"] = newDocEntry(Document{"_id": "b", "status": "dup"})
	s.markDirty()
	s.collections["q"].entries["b"].dirty.Store(true)
	s.collections["q"].entries["b"].mutGen = s.dirtyGen
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, testOpts()); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("open unique conflict: want ErrDuplicate, got %v", err)
	}

	rep, err := Repair(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.UniqueConflicts != 1 || rep.DocsDropped != 1 || rep.DocsKept != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.IndexesRebuilt != 1 {
		t.Fatalf("indexes: %+v", rep)
	}

	s2 := openTest(t, path)
	defer s2.Close()
	// Kept lowest _id "a"
	if _, err := s2.Get("q", "a"); err != nil {
		t.Fatalf("kept a: %v", err)
	}
	if _, err := s2.Get("q", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropped b: %v", err)
	}
	// Unique index works after repair
	if err := s2.Insert("q", Document{"_id": "c", "status": "dup"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("unique enforced: %v", err)
	}
}

func TestRepairMissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Repair(filepath.Join(dir, "nope.mlstore"), testOpts()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRepairRequiresMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s := openTest(t, path)
	_ = s.Close()
	if _, err := Repair(path, Options{}); err == nil {
		t.Fatal("want error without MasterKey")
	}
}
