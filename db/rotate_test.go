package db

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	rotMasterA = []byte("master-A-32-bytes-long!!!!!!!!!!")
	rotMasterB = []byte("master-B-32-bytes-long!!!!!!!!!!")
)

// rotateSetup creates a clean file-backed store with two docs and closes it.
func rotateSetup(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rot.mlstore")
	s, err := OpenWithLock(path, Options{MasterKey: rotMasterA, MachineID: "machine-A", LightKDF: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("docs", Document{"_id": "d1", "v": "uno"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("docs", Document{"_id": "d2", "v": "dos"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func rotOpts(master []byte, machine string) Options {
	return Options{MasterKey: master, MachineID: machine, LightKDF: true}
}

func TestRotateKeysMasterOnly(t *testing.T) {
	_, path := rotateSetup(t)

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(rotMasterB, ""); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// New key opens and data is intact.
	s2, err := Open(path, rotOpts(rotMasterB, "machine-A"))
	if err != nil {
		t.Fatalf("reopen with new master: %v", err)
	}
	docs, err := s2.Find("docs", nil, nil)
	if err != nil || len(docs) != 2 {
		t.Fatalf("post-rotate data: %d docs err=%v", len(docs), err)
	}
	s2.Close()

	// Old master must no longer decrypt.
	if _, err := Open(path, rotOpts(rotMasterA, "machine-A")); err == nil {
		t.Fatal("old master still opens the file after rotation")
	}
}

func TestRotateKeysMachineOnly(t *testing.T) {
	_, path := rotateSetup(t)

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(nil, "machine-B"); err != nil {
		t.Fatalf("rotate machine: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, rotOpts(rotMasterA, "machine-B"))
	if err != nil {
		t.Fatalf("reopen with new machine id: %v", err)
	}
	if docs, _ := s2.Find("docs", nil, nil); len(docs) != 2 {
		t.Fatalf("post-rotate data: %d docs", len(docs))
	}
	s2.Close()
	if _, err := Open(path, rotOpts(rotMasterA, "machine-A")); err == nil {
		t.Fatal("old machine id still opens the file after rotation")
	}
}

func TestRotateKeysNoOpRewrap(t *testing.T) {
	_, path := rotateSetup(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	// Empty args = re-wrap with same credentials (fresh salt) — must be safe.
	if err := s.RotateKeys(nil, ""); err != nil {
		t.Fatalf("noop rotate: %v", err)
	}
	s.Close()

	s2, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatalf("reopen after noop rotate: %v", err)
	}
	if docs, _ := s2.Find("docs", nil, nil); len(docs) != 2 {
		t.Fatalf("noop rotate lost data: %d docs", len(docs))
	}
	s2.Close()

	after, _ := os.ReadFile(path)
	if bytes.Equal(before[32:48], after[32:48]) {
		t.Fatal("salt was not rotated on re-wrap")
	}
}

func TestRotateKeysPayloadBytesUntouched(t *testing.T) {
	_, path := rotateSetup(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(rotMasterB, "machine-B"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	after, _ := os.ReadFile(path)
	if !bytes.Equal(before[headerSize:], after[headerSize:]) {
		t.Fatal("payload ciphertext changed — rotation must be header-only")
	}
	if bytes.Equal(before[32:48], after[32:48]) {
		t.Fatal("salt (wrap nonce) was not refreshed")
	}
	if bytes.Equal(before[120:152], after[120:152]) {
		t.Fatal("HMAC was not re-computed")
	}
}

func TestRotateKeysTamperedHeaderRejected(t *testing.T) {
	_, path := rotateSetup(t)

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the on-disk header (schema_version byte) behind the store's back.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), data...)
	tampered[9] ^= 0xFF
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.RotateKeys(rotMasterB, ""); err == nil {
		t.Fatal("rotate accepted a file whose header HMAC does not verify")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}

	// Failed rotation must leave the file exactly as it was.
	after, _ := os.ReadFile(path)
	if !bytes.Equal(tampered, after) {
		t.Fatal("failed rotation modified the file")
	}
}

func TestRotateKeysRefusesDirtyStore(t *testing.T) {
	_, path := rotateSetup(t)

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Insert("docs", Document{"_id": "d3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(rotMasterB, ""); err == nil {
		t.Fatal("rotate accepted a dirty store (on-disk file would go stale)")
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(rotMasterB, ""); err != nil {
		t.Fatalf("rotate after flush: %v", err)
	}
}

// The critical adoption test: credentials rotated in RAM must stick, so
// post-rotation mutations flush with the NEW KEK and reopen agrees.
func TestRotateKeysRAMAdoptsNewCredentials(t *testing.T) {
	_, path := rotateSetup(t)

	s, err := Open(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(rotMasterB, "machine-B"); err != nil {
		t.Fatal(err)
	}
	// Mutate AFTER rotation; the next flush re-wraps with the new KEK.
	if err := s.Insert("docs", Document{"_id": "d3", "v": "tres"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, rotOpts(rotMasterB, "machine-B"))
	if err != nil {
		t.Fatalf("reopen with rotated credentials: %v", err)
	}
	docs, _ := s2.Find("docs", nil, nil)
	if len(docs) != 3 {
		t.Fatalf("post-rotate mutations lost: %d docs, want 3", len(docs))
	}
	s2.Close()

	for _, bad := range []Options{
		rotOpts(rotMasterA, "machine-A"),
		rotOpts(rotMasterB, "machine-A"),
		rotOpts(rotMasterA, "machine-B"),
	} {
		if _, err := Open(path, bad); err == nil {
			t.Fatal("pre-rotation credentials still open the file")
		}
	}
}

func TestRotateKeysConcurrentReadersUnaffected(t *testing.T) {
	_, path := rotateSetup(t)

	s, err := OpenWithLock(path, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var readErrs int
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.Find("docs", nil, nil); err != nil {
					mu.Lock()
					readErrs++
					mu.Unlock()
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	err = s.RotateKeys(rotMasterB, "")
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("rotate under readers: %v", err)
	}
	if readErrs != 0 {
		t.Fatalf("%d reader errors during rotation", readErrs)
	}
}

func TestRotateKeysRequiresFileBacked(t *testing.T) {
	s := New()
	if err := s.RotateKeys(rotMasterB, "machine-B"); err == nil {
		t.Fatal("rotate must refuse a RAM-only store")
	}
}

func TestRotateKeysThenMoveToOtherMachine(t *testing.T) {
	dirA, pathA := rotateSetup(t)

	// Provider moves the db to another machine: rotate the machine binding
	// while the file is still on the source machine.
	s, err := Open(pathA, rotOpts(rotMasterA, "machine-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RotateKeys(nil, "machine-B"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Copy the file to the destination machine's data directory.
	dirB := t.TempDir()
	pathB := filepath.Join(dirB, "moved.mlstore")
	if err := copyFile(pathA, pathB); err != nil {
		t.Fatal(err)
	}
	_ = dirA

	// It opens at the destination with the new binding (and only with it).
	s2, err := Open(pathB, rotOpts(rotMasterA, "machine-B"))
	if err != nil {
		t.Fatalf("destination open: %v", err)
	}
	if docs, _ := s2.Find("docs", nil, nil); len(docs) != 2 {
		t.Fatalf("moved db data: %d docs", len(docs))
	}
	s2.Close()
	if _, err := Open(pathB, rotOpts(rotMasterA, "machine-A")); err == nil {
		t.Fatal("moved db opened with the source machine binding")
	}
}
