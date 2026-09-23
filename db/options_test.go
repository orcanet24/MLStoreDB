package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOptionsResolveDefaults(t *testing.T) {
	var o Options
	if got := o.autoFlushInterval(); got != 2*time.Second {
		t.Fatalf("default auto-flush = %v, want 2s", got)
	}
	if got := o.lockFileName(); got != "mlstoredb.lock" {
		t.Fatalf("default lock file = %q", got)
	}
	// Zero and negative fall back to the default.
	neg := Options{AutoFlush: -time.Second}
	if got := neg.autoFlushInterval(); got != 2*time.Second {
		t.Fatalf("negative auto-flush should fall back to 2s, got %v", got)
	}
	// Custom values pass through.
	custom := Options{AutoFlush: 250 * time.Millisecond, LockFile: "app.lock"}
	if got := custom.autoFlushInterval(); got != 250*time.Millisecond {
		t.Fatalf("custom auto-flush = %v", got)
	}
	if got := custom.lockFileName(); got != "app.lock" {
		t.Fatalf("custom lock file = %q", got)
	}
	// FindWorkers: 0 → GOMAXPROCS, negative → 1 (serial), positive → n.
	var fw Options
	if got := fw.findWorkers(); got < 1 {
		t.Fatalf("default findWorkers = %d, want >= 1", got)
	}
	if got := (Options{FindWorkers: -1}).findWorkers(); got != 1 {
		t.Fatalf("negative findWorkers = %d, want 1", got)
	}
	if got := (Options{FindWorkers: 7}).findWorkers(); got != 7 {
		t.Fatalf("custom findWorkers = %d, want 7", got)
	}
	// MaxPendingWrites: 0 = unlimited, positive passes through.
	if got := (Options{}).maxPendingWrites(); got != 0 {
		t.Fatalf("default maxPendingWrites = %d, want 0", got)
	}
	if got := (Options{MaxPendingWrites: 8}).maxPendingWrites(); got != 8 {
		t.Fatalf("custom maxPendingWrites = %d, want 8", got)
	}
}

func TestOpenWithLockCustomLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.mlstore")
	custom := Options{
		MasterKey: []byte("master-key-32-bytes-long!!!!!!!"),
		MachineID: "m", LightKDF: true,
		LockFile: "myapp-a.lock",
	}

	s, err := OpenWithLock(path, custom)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Custom lock file exists; the default one does not.
	if _, err := os.Stat(filepath.Join(dir, "myapp-a.lock")); err != nil {
		t.Fatalf("custom lock file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mlstoredb.lock")); !os.IsNotExist(err) {
		t.Fatalf("default lock file must not exist when LockFile is set (err=%v)", err)
	}

	// Second open with the SAME custom lock → blocked.
	if _, err := OpenWithLock(path, custom); err != ErrAlreadyOpen {
		t.Fatalf("second open with same LockFile: err=%v, want ErrAlreadyOpen", err)
	}

	// Opening the same file with the DEFAULT lock name must succeed: locks
	// are independent, this is the multi-db-per-directory use case.
	s2, err := OpenWithLock(path, Options{MasterKey: custom.MasterKey, MachineID: "m", LightKDF: true})
	if err != nil {
		t.Fatalf("open with default lock name while custom lock held: %v", err)
	}
	s2.Close()
}

func TestRepairHonorsCustomLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.mlstore")
	opts := Options{
		MasterKey: []byte("master-key-32-bytes-long!!!!!!!"),
		MachineID: "m", LightKDF: true,
		LockFile: "myapp-r.lock",
	}

	// Create a valid clean file.
	s, err := OpenWithLock(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("c", Document{"_id": "1", "v": "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Repair works with the custom lock name.
	rep, err := Repair(path, opts)
	if err != nil {
		t.Fatalf("repair with custom lock: %v", err)
	}
	if rep.DocsKept != 1 {
		t.Fatalf("repair kept %d docs, want 1", rep.DocsKept)
	}

	// Repair is blocked while another process holds that custom lock.
	s2, err := OpenWithLock(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := Repair(path, opts); err != ErrAlreadyOpen {
		t.Fatalf("repair while locked: err=%v, want ErrAlreadyOpen", err)
	}
}

func TestAutoFlushCustomIntervalPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fast.mlstore")
	opts := Options{
		MasterKey: []byte("master-key-32-bytes-long!!!!!!!"),
		MachineID: "m", LightKDF: true,
		AutoFlush: 150 * time.Millisecond,
	}

	s, err := OpenWithLock(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1", "x": 1}); err != nil {
		t.Fatal(err)
	}
	// No Flush/Close — only the fast ticker can persist this.
	deadline := time.Now().Add(3 * time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		s2, err := Open(path, opts)
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
		docs, _ := s2.Find("q", nil, nil)
		s2.Close()
		if len(docs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			s.Close()
			t.Fatal("custom auto-flush never persisted the doc")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
