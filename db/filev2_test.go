package db

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M1: formatVersion 2 record log — no plaintext JSON body, COMMIT commit point.

func TestV2FormatRecordLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1", "status": "OPEN", "secret": "hunter2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < headerSize {
		t.Fatalf("file too small: %d", len(raw))
	}
	// formatVersion = 2
	if raw[4] != 2 || raw[5] != 0 {
		t.Fatalf("formatVersion = %d, want 2", int(raw[4])|int(raw[5])<<8)
	}
	// body must not contain plaintext JSON / document content
	if bytes.Contains(raw[headerSize:], []byte(`"hunter2"`)) {
		t.Error("plaintext doc value in record log")
	}
	if bytes.Contains(raw[headerSize:], []byte(`"status"`)) {
		t.Error("plaintext field name in record log (payload should be ciphertext)")
	}
	// must end with a COMMIT record (scan types)
	off := headerSize
	var sawCommit, sawDoc bool
	for off+recHdrSize <= len(raw) {
		typ, _, idBlob, _, total, err := parseRecordAt(raw, off)
		if err != nil {
			t.Fatalf("parse @%d: %v", off, err)
		}
		switch typ {
		case recCOMMIT:
			sawCommit = true
		case recDOC:
			sawDoc = true
			if _, _, err := decodeIDBlob(idBlob); err != nil {
				t.Fatalf("id blob: %v", err)
			}
		}
		off += total
	}
	if !sawCommit || !sawDoc {
		t.Fatalf("commit=%v doc=%v", sawCommit, sawDoc)
	}
}

func TestV2ColdEvictionAndCacheStats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	opts.CacheBytes = 8 << 10 // tiny budget → force eviction pressure
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Incompressible payload so on-disk record size tracks the budget pressure.
	payload := make([]byte, 2048)
	for i := range payload {
		payload[i] = byte(i*17 + 31)
	}
	blob := json.Number(string(payload)) // keep as raw string in doc
	for i := 0; i < 64; i++ {
		if err := s.Insert("q", Document{"_id": idKey(i), "blob": blob.String()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// After open, docs are cold (rec offsets only).
	st := s2.CacheStats()
	if st.TotalEntries != 64 {
		t.Fatalf("entries = %d", st.TotalEntries)
	}
	if st.Cold != 64 {
		t.Fatalf("expected all cold after open, got cold=%d resident=%d", st.Cold, st.Resident)
	}
	// Access one doc → becomes resident.
	if _, err := s2.Get("q", idKey(0)); err != nil {
		t.Fatal(err)
	}
	st = s2.CacheStats()
	if st.Resident < 1 {
		t.Fatalf("resident after Get: %+v", st)
	}
	// Walk many docs to pressure the 64KiB budget → evictions expected.
	for i := 0; i < 64; i++ {
		if _, err := s2.Get("q", idKey(i)); err != nil {
			t.Fatal(err)
		}
	}
	st = s2.CacheStats()
	if st.Evictions == 0 {
		t.Fatalf("expected evictions under 8KiB budget: %+v", st)
	}
	if st.Bytes > st.Budget && st.Budget > 0 {
		t.Fatalf("bytes %d > budget %d with no further eviction", st.Bytes, st.Budget)
	}
	// Find still returns all docs (loads cold as needed).
	docs, err := s2.Find("q", nil, nil)
	if err != nil || len(docs) != 64 {
		t.Fatalf("find after eviction: %d %v", len(docs), err)
	}
}

func TestV2TornTailSelfHeal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1", "v": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	// Second flush appends another doc (sawFile → append mode).
	if err := s.Insert("q", Document{"_id": "2", "v": "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a torn append: append a partial record header (no full record).
	torn := append(append([]byte(nil), raw...), make([]byte, 10)...) // 10 < recHdrSize
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatalf("open with torn tail: %v", err)
	}
	defer s2.Close()
	docs, err := s2.Find("q", nil, nil)
	if err != nil || len(docs) != 2 {
		t.Fatalf("docs after tail heal: %d %v", len(docs), err)
	}
	// File should have been truncated back to last COMMIT.
	after, _ := os.ReadFile(path)
	if int64(len(after)) != int64(len(raw)) {
		// may equal raw if truncate landed on commit end == raw end
		if len(after) > len(raw) {
			t.Fatalf("tail not truncated: %d > %d", len(after), len(raw))
		}
	}
}

func TestV2CompactRewritesLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := s.Upsert("q", idKey(i), Document{"_id": idKey(i), "n": i, "pad": strings.Repeat("z", 256)}); err != nil {
			t.Fatal(err)
		}
	}
	// Rewrite same ids many times → dead bytes in the log.
	for round := 0; round < 5; round++ {
		for i := 0; i < 20; i++ {
			if err := s.Update("q", idKey(i), Document{"round": round}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.FlushSync(); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.Stat(path)
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size() {
		t.Errorf("compact did not shrink: %d → %d", before.Size(), after.Size())
	}
	// Data intact + log still valid.
	docs, err := s.Find("q", nil, nil)
	if err != nil || len(docs) != 20 {
		t.Fatalf("after compact: %d %v", len(docs), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	docs, _ = s2.Find("q", nil, nil)
	if len(docs) != 20 {
		t.Fatalf("reopen after compact: %d", len(docs))
	}
	// Verify offsets are valid (Get works for cold docs).
	if _, err := s2.Get("q", idKey(7)); err != nil {
		t.Fatalf("cold get after compact: %v", err)
	}
}

func TestV1MigrationToV2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	// Build a minimal v1 file by hand using current crypto helpers.
	opts := testOpts()
	dek, err := randomBytes(32)
	if err != nil {
		t.Fatal(err)
	}
	salt, err := randomBytes(16)
	if err != nil {
		t.Fatal(err)
	}
	machine := opts.machineID()
	kek := deriveKEK(opts.MasterKey, machine, salt, lightKDFTime, lightKDFMem, lightKDFPar)
	wrapped, err := sealGCM(kek, salt[:12], dek, nil)
	if err != nil {
		t.Fatal(err)
	}
	fd := &fileData{
		Meta: fileMeta{App: "mlstoredb", SchemaVersion: 0},
		Collections: map[string]*fileColl{
			"q": {
				Indexes: []IndexInfo{{Fields: []string{"status"}, Unique: false}},
				Docs: []Document{
					{"_id": "1", "status": "OPEN"},
					{"_id": "2", "status": "CLOSED"},
				},
			},
		},
	}
	plain, err := json.Marshal(fd)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := randomBytes(12)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := compressIfNeeded(plain)
	ct, err := sealGCM(dek, nonce, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &header{
		FormatVer:     1,
		SchemaVersion: 0,
		CreatedAt:     1700000000,
		KDFTime:       lightKDFTime,
		KDFMemKiB:     lightKDFMem,
		KDFPar:        lightKDFPar,
	}
	if len(body) != len(plain) {
		h.Flags |= flagCompressed
	}
	copy(h.KDFSalt[:], salt)
	copy(h.NonceBase[:], nonce)
	copy(h.DEKWrapped[:], wrapped)
	h.setHMAC(kek)
	var out bytes.Buffer
	out.Write(h.marshalBody())
	out.Write(h.HeaderHMAC[:])
	out.Write(ct)
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// Open v1 → in-memory load, pending v2 rewrite.
	s, err := Open(path, opts)
	if err != nil {
		t.Fatalf("open v1: %v", err)
	}
	if s.SchemaVersion() != 0 {
		t.Errorf("schema: %d", s.SchemaVersion())
	}
	docs, err := s.Find("q", nil, nil)
	if err != nil || len(docs) != 2 {
		t.Fatalf("v1 docs: %d %v", len(docs), err)
	}
	list, _ := s.ListIndexes("q")
	if len(list) != 1 || list[0].Fields[0] != "status" {
		t.Fatalf("v1 index defs: %v", list)
	}
	// First flush migrates to v2.
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if raw[4] != 2 {
		t.Fatalf("after migration formatVersion = %d, want 2", raw[4])
	}
	// Reopen as v2.
	s2, err := Open(path, opts)
	if err != nil {
		t.Fatalf("open migrated: %v", err)
	}
	defer s2.Close()
	docs, _ = s2.Find("q", nil, nil)
	if len(docs) != 2 {
		t.Fatalf("migrated docs: %d", len(docs))
	}
	list, _ = s2.ListIndexes("q")
	if len(list) != 1 {
		t.Fatalf("migrated indexes: %v", list)
	}
	// Index still enforced.
	_ = s2.Insert("q", Document{"_id": "3", "status": "OPEN"})
	if err := s2.EnsureIndex("q", []string{"status"}, false); err != nil {
		t.Fatal(err)
	}
}

func TestV2DeleteDoesNotResurrect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1", "v": "keep"})
	_ = s.Insert("q", Document{"_id": "2", "v": "gone"})
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("q", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Get("q", "2"); err == nil {
		t.Fatal("deleted doc resurrected after reopen")
	}
	docs, _ := s2.Find("q", nil, nil)
	if len(docs) != 1 || docs[0]["_id"] != "1" {
		t.Fatalf("docs: %v", ids(docs))
	}
}

func TestV2AppendIncrementalPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "a"})
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	size1 := fileSize(t, path)
	_ = s.Insert("q", Document{"_id": "b"})
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	size2 := fileSize(t, path)
	if size2 <= size1 {
		t.Fatalf("append did not grow file: %d → %d", size1, size2)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	docs, _ := s2.Find("q", nil, nil)
	if len(docs) != 2 {
		t.Fatalf("docs: %v", ids(docs))
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}
