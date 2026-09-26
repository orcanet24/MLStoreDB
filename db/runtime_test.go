package db

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotCreatesIndependentBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	bak := filepath.Join(dir, "bak.mlstore")

	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1", "v": "before"})
	if err := s.Snapshot(bak); err != nil {
		t.Fatal(err)
	}
	// mutar tras la instantánea
	_ = s.Insert("q", Document{"_id": "2", "v": "after"})
	_ = s.Close()

	// el respaldo solo tiene el documento 1
	b, err := Open(bak, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	docs, _ := b.Find("q", nil, nil)
	if len(docs) != 1 || docs[0]["_id"] != "1" {
		t.Errorf("backup docs: %v", ids(docs))
	}

	// el principal tiene ambos
	m, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	docs, _ = m.Find("q", nil, nil)
	if len(docs) != 2 {
		t.Errorf("main docs: %v", ids(docs))
	}
}

func TestSnapshotRequiresMasterKey(t *testing.T) {
	s := New()
	if err := s.Snapshot(filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("want error without master key")
	}
}

func TestFileLockBlocksSecondOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")

	s1, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	_, err = OpenWithLock(path, testOpts())
	if !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("want ErrAlreadyOpen, got %v", err)
	}

	// tras cerrar, el bloqueo se libera
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatalf("re-open after release: %v", err)
	}
	_ = s2.Close()
}

func TestAutoFlushPersistsWithinInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")

	s, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1", "x": 1})
	// NO llamar a Flush/Close — esperar al ticker
	time.Sleep(defaultAutoFlush + 500*time.Millisecond)

	// abrir sin bloqueo (lado lector): el bloqueo sigue en manos de s — usar Open, no OpenWithLock.
	// En realidad s tiene el bloqueo; leer con Open crudo es correcto (Open no comprueba el bloqueo)
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	docs, _ := s2.Find("q", nil, nil)
	if len(docs) != 1 {
		t.Errorf("auto-flush did not persist: %d docs", len(docs))
	}
	_ = s2.Close()
	_ = s.Close()
}

func TestCloseReleasesLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// el archivo de bloqueo puede seguir existiendo, pero debe estar desbloqueado
	s2, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
}

func TestAutoFlushStopsOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// tras cerrar, mutar y esperar NO debería escribir automáticamente con el ticker
	// (el almacén está cerrado; markDirty sigue poniendo el flag, pero el ticker está parado)
	_ = s.Insert("q", Document{"_id": "late"})
	time.Sleep(defaultAutoFlush + 300*time.Millisecond)
	// reabrir: no debería haber "late" salvo que Close hiciera flush (no lo hizo — insert tras cerrar)
	// Nota: Insert tras Close sigue funcionando en memoria; el ticker está parado, así que no hay flush.
	// El estado final en disco = lo que Close dejó grabado (vacío).
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	docs, _ := s2.Find("q", nil, nil)
	if len(docs) != 0 {
		t.Errorf("ticker wrote after close: %v", ids(docs))
	}
	_ = s2.Close()
}

func TestSnapshotEmptyDest(t *testing.T) {
	s := New()
	if err := s.Snapshot(""); err == nil {
		t.Fatal("want error")
	}
}

func TestLockFileCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := OpenWithLock(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, testOpts().lockFileName())
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock file missing: %v", err)
	}
	_ = s.Close()
}
