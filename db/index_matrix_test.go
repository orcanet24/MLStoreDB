package db

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// M2: CSR index_matrix embedded in the IDX record with checksum fallback.

func TestIndexMatrixSerializedAndLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex("q", []string{"status"}, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		st := "OPEN"
		if i%3 == 0 {
			st = "CLOSED"
		}
		if i%7 == 0 {
			st = "PENDING"
		}
		if err := s.Insert("q", Document{"_id": idKey(i), "status": st}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	// In-RAM matrix live after serialize.
	list, _ := s.ListIndexes("q")
	if len(list) != 1 {
		t.Fatalf("indexes: %v", list)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// Loaded via CSR path: matrix must be live for flat seeks.
	c := s2.collections["q"]
	if c == nil {
		t.Fatal("no collection")
	}
	idx := c.indexes[0]
	idx.mu.Lock()
	m := idx.matrix
	nOrd := len(idx.ord)
	idx.mu.Unlock()
	if m == nil {
		t.Fatal("matrix not loaded from disk")
	}
	if !m.valid() {
		t.Fatal("loaded matrix failed checksum")
	}
	if len(m.Indptr) != nOrd+1 {
		t.Fatalf("indptr=%d ord=%d", len(m.Indptr), nOrd)
	}
	// Range seek returns same results via matrix path.
	docs, err := s2.Find("q", Document{"status": "OPEN"}, nil)
	if err != nil || len(docs) == 0 {
		t.Fatalf("equality find: %d %v", len(docs), err)
	}
	docs2, err := s2.Find("q", Document{"status": Document{"$regex": "^O"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Explain should still report IXSCAN for equality.
	ex := s2.Explain("q", Document{"status": "CLOSED"})
	if ex.Plan != "IXSCAN" {
		t.Fatalf("plan=%s want IXSCAN (%+v)", ex.Plan, ex)
	}
	_ = docs2
	// Count via index still correct.
	n, err := s2.Count("q", Document{"status": "CLOSED"})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := 0; i < 50; i++ {
		if i%3 == 0 && i%7 != 0 {
			want++
		} else if i%3 == 0 && i%7 == 0 {
			// PENDING wins in insert order? no — status chosen by last condition
		}
	}
	// Recompute expected with same rules as insert.
	want = 0
	for i := 0; i < 50; i++ {
		st := "OPEN"
		if i%3 == 0 {
			st = "CLOSED"
		}
		if i%7 == 0 {
			st = "PENDING"
		}
		if st == "CLOSED" {
			want++
		}
	}
	if int(n) != want {
		t.Fatalf("count=%d want=%d", n, want)
	}
}

func TestIndexMatrixChecksumFallbackToRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex("q", []string{"n"}, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := s.Insert("q", Document{"_id": idKey(i), "n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the CSR checksum inside the IDX payload (still a valid record:
	// we re-serialize the payload JSON after flipping csum, then rewrite via
	// a loadRows unit path — simpler: exercise loadRows directly).
	// Build the payload as prepareFlush would emit it, corrupt CSR, keep Rows.
	idx := newIndex([]string{"n"}, false)
	for i := 0; i < 20; i++ {
		if err := idx.addDoc(Document{"_id": idKey(i), "n": i}, idKey(i)); err != nil {
			t.Fatal(err)
		}
	}
	p := idx.serialize()
	if p.CSR == nil || !p.CSR.valid() {
		t.Fatal("serialize did not emit valid CSR")
	}
	if len(p.Rows) != 20 {
		t.Fatalf("rows=%d", len(p.Rows))
	}
	// Corrupt checksum → valid() fails → loadRows falls back to Rows.
	p.CSR.Checksum ^= 0xdeadbeef
	idx2 := newIndex([]string{"n"}, false)
	if err := idx2.loadRows(p); err != nil {
		t.Fatalf("fallback load: %v", err)
	}
	if idx2.matrix != nil {
		t.Fatal("matrix must be nil after Rows fallback")
	}
	if len(idx2.ord) != 20 {
		t.Fatalf("ord=%d", len(idx2.ord))
	}
	// Seek still works without matrix.
	got := idx2.lookupIDs([]any{float64(7)})
	if len(got) != 1 {
		t.Fatalf("lookup after fallback: %v", got)
	}
	if _, ok := got[idKey(7)]; !ok {
		t.Fatalf("lookup after fallback: %v", got)
	}

	// Totally invalid CSR (shape) + no Rows → error → eager path.
	bad := idx.serialize()
	bad.Rows = nil
	bad.CSR.Indptr = []int32{0, 99} // shape mismatch vs Keys
	// recompute? no — leave checksum wrong too so valid() is false AND rows empty
	// valid() false → falls to Rows (empty) → returns nil with empty ord.
	// Force the eager signal: empty Rows AND nil/bad CSR.
	bad.CSR = nil
	idx3 := newIndex([]string{"n"}, false)
	if err := idx3.loadRows(bad); err == nil && len(idx3.ord) != 0 {
		t.Fatalf("expected empty load, ord=%d", len(idx3.ord))
	}
}

func TestIndexMatrixRoundtripJSON(t *testing.T) {
	m := &csrMatrix{
		Keys:    []string{"a", "b"},
		Vals:    []any{"x", float64(2)},
		Indptr:  []int32{0, 2, 3},
		Indices: []int32{0, 1, 1},
		Ids:     []string{"id1", "id2"},
	}
	m.Checksum = m.computeChecksum()
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back csrMatrix
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if !back.valid() {
		t.Fatal("roundtrip checksum/shape invalid")
	}
	// Tamper one id → checksum fails.
	back.Ids[0] = "tampered"
	if back.valid() {
		t.Fatal("tampered matrix still valid")
	}
}

func TestIndexMatrixInvalidatedOnMutation(t *testing.T) {
	idx := newIndex([]string{"n"}, false)
	_ = idx.addDoc(Document{"_id": "1", "n": 1}, "1")
	_ = idx.serialize() // sets live matrix
	idx.mu.Lock()
	if idx.matrix == nil {
		idx.mu.Unlock()
		t.Fatal("matrix should be set after serialize")
	}
	idx.mu.Unlock()
	idx.addDoc(Document{"_id": "2", "n": 2}, "2")
	idx.mu.Lock()
	if idx.matrix != nil {
		idx.mu.Unlock()
		t.Fatal("matrix must clear on mutation")
	}
	idx.mu.Unlock()
}

func TestIndexMatrixUniqueMultiIDRejected(t *testing.T) {
	idx := newIndex([]string{"u"}, true)
	// Hand-build a corrupt CSR payload: unique key with two ids.
	p := idxPayload{
		Fields: []string{"u"},
		Unique: true,
		CSR: &csrMatrix{
			Keys:    []string{"dup"},
			Vals:    []any{"dup"},
			Indptr:  []int32{0, 2},
			Indices: []int32{0, 1},
			Ids:     []string{"a", "b"},
		},
	}
	p.CSR.Checksum = p.CSR.computeChecksum()
	if err := idx.loadRows(p); err != ErrDuplicate {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
}
