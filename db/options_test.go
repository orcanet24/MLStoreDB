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
	// Cero y negativo recurren al valor por defecto.
	neg := Options{AutoFlush: -time.Second}
	if got := neg.autoFlushInterval(); got != 2*time.Second {
		t.Fatalf("negative auto-flush should fall back to 2s, got %v", got)
	}
	// Los valores personalizados pasan tal cual.
	custom := Options{AutoFlush: 250 * time.Millisecond, LockFile: "app.lock"}
	if got := custom.autoFlushInterval(); got != 250*time.Millisecond {
		t.Fatalf("custom auto-flush = %v", got)
	}
	if got := custom.lockFileName(); got != "app.lock" {
		t.Fatalf("custom lock file = %q", got)
	}
	// FindWorkers: 0 → GOMAXPROCS, negativo → 1 (serie), positivo → n.
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
	// MaxPendingWrites: 0 = sin límite, los positivos pasan tal cual.
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

	// Existe el archivo de bloqueo personalizado; el de por defecto no.
	if _, err := os.Stat(filepath.Join(dir, "myapp-a.lock")); err != nil {
		t.Fatalf("custom lock file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mlstoredb.lock")); !os.IsNotExist(err) {
		t.Fatalf("default lock file must not exist when LockFile is set (err=%v)", err)
	}

	// Una segunda apertura con el MISMO bloqueo personalizado → bloqueada.
	if _, err := OpenWithLock(path, custom); err != ErrAlreadyOpen {
		t.Fatalf("second open with same LockFile: err=%v, want ErrAlreadyOpen", err)
	}

	// Abrir el mismo archivo con el nombre de bloqueo POR DEFECTO debe funcionar: los
	// bloqueos son independientes, es el caso de uso de varias bases por directorio.
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

	// Crear un archivo limpio y válido.
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

	// Repair funciona con el nombre de bloqueo personalizado.
	rep, err := Repair(path, opts)
	if err != nil {
		t.Fatalf("repair with custom lock: %v", err)
	}
	if rep.DocsKept != 1 {
		t.Fatalf("repair kept %d docs, want 1", rep.DocsKept)
	}

	// Repair queda bloqueado mientras otro proceso tiene ese bloqueo personalizado.
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
	// Sin Flush/Close — solo el ticker rápido puede persistir esto.
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
