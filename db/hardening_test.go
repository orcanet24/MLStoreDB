package db

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// ── 1. Índice ordenado + range seek ──

func TestRangeSeekIndexedMatchesFullScan(t *testing.T) {
	s := New()
	for i := 0; i < 50; i++ {
		_ = s.Insert("c", Document{"_id": idKey(i), "n": float64(i)})
	}
	_ = s.Insert("c", Document{"_id": "sx", "n": "not-a-number"})
	_ = s.Insert("c", Document{"_id": "miss"})

	filters := []Document{
		{"n": Document{"$gt": 40}},
		{"n": Document{"$gte": 40}},
		{"n": Document{"$lt": 10}},
		{"n": Document{"$lte": 10}},
		{"n": Document{"$gte": 10, "$lt": 20}},
		{"n": Document{"$gt": 10, "$lte": 20}},
	}
	for _, f := range filters {
		want := fullScanIDs(t, s, "c", f)
		// indexed
		if err := s.EnsureIndex("c", []string{"n"}, false); err != nil {
			t.Fatal(err)
		}
		got := fullScanIDs(t, s, "c", f)
		if !sameStringSet(got, want) {
			t.Errorf("range %v: indexed %v != baseline %v", f, got, want)
		}
		if err := s.DropIndex("c", []string{"n"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRangeSeekOrderAscDesc(t *testing.T) {
	s := New()
	for i := 0; i < 20; i++ {
		_ = s.Insert("c", Document{"_id": idKey(i), "n": float64(100 - i*3)})
	}
	_ = s.EnsureIndex("c", []string{"n"}, false)

	docs, err := s.Find("c", Document{"n": Document{"$gte": 0}}, &FindOptions{
		Sort:  map[string]int{"n": 1},
		Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 5 {
		t.Fatalf("asc limit: %d", len(docs))
	}
	prev := -1.0
	for _, d := range docs {
		v, _ := toFloat(d["n"])
		if v < prev {
			t.Fatalf("asc order broken: %v after %v", v, prev)
		}
		prev = v
	}

	docs, _ = s.Find("c", Document{"n": Document{"$gte": 0}}, &FindOptions{
		Sort:  map[string]int{"n": -1},
		Limit: 5,
	})
	if len(docs) != 5 {
		t.Fatalf("desc limit: %d", len(docs))
	}
	prev = 1e18
	for _, d := range docs {
		v, _ := toFloat(d["n"])
		if v > prev {
			t.Fatalf("desc order broken: %v after %v", v, prev)
		}
		prev = v
	}
}

func TestRangeCrossTypeExcluded(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "v": 10})
	_ = s.Insert("c", Document{"_id": "2", "v": "abc"})
	_ = s.Insert("c", Document{"_id": "3"})
	_ = s.EnsureIndex("c", []string{"v"}, false)

	docs, err := s.Find("c", Document{"v": Document{"$gt": 5}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0]["_id"] != "1" {
		t.Errorf("$gt number must exclude string/missing: %v", ids(docs))
	}
}

func TestStringRangeIndexed(t *testing.T) {
	s := New()
	for _, v := range []string{"apple", "banana", "cherry", "date"} {
		_ = s.Insert("c", Document{"_id": v, "s": v})
	}
	_ = s.EnsureIndex("c", []string{"s"}, false)
	docs, err := s.Find("c", Document{"s": Document{"$gte": "banana", "$lt": "date"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Errorf("string range: %v", ids(docs))
	}
}

func TestSortByIndexMatchesSortWithoutIndex(t *testing.T) {
	s := New()
	for i := 0; i < 30; i++ {
		_ = s.Insert("c", Document{"_id": idKey(i), "n": float64((i * 7) % 30)})
	}
	want, _ := s.Find("c", nil, &FindOptions{Sort: map[string]int{"n": 1}})
	_ = s.EnsureIndex("c", []string{"n"}, false)
	got, _ := s.Find("c", nil, &FindOptions{Sort: map[string]int{"n": 1}})
	if len(got) != len(want) {
		t.Fatalf("len %d vs %d", len(got), len(want))
	}
	for i := range got {
		if got[i]["_id"] != want[i]["_id"] {
			t.Fatalf("sort by index mismatch at %d: %v vs %v", i, got[i]["_id"], want[i]["_id"])
		}
	}
}

// ── 2. Planner: prefijo compuesto + range ──

func TestCompoundPrefixEquality(t *testing.T) {
	s := New()
	_ = s.Insert("m", Document{"_id": "1", "user": "u1", "acct": "a1"})
	_ = s.Insert("m", Document{"_id": "2", "user": "u1", "acct": "a2"})
	_ = s.Insert("m", Document{"_id": "3", "user": "u2", "acct": "a1"})
	_ = s.EnsureIndex("m", []string{"user", "acct"}, false)

	docs, err := s.Find("m", Document{"user": "u1"}, nil)
	if err != nil || len(docs) != 2 {
		t.Fatalf("prefix user=u1: %v %v", ids(docs), err)
	}
	docs, err = s.Find("m", Document{"user": "u1", "acct": "a2"}, nil)
	if err != nil || len(docs) != 1 || docs[0]["_id"] != "2" {
		t.Fatalf("full compound: %v %v", ids(docs), err)
	}
	ex := s.Explain("m", Document{"user": "u1", "acct": "a1"})
	if ex.Plan != "IXSCAN" || !ex.Exact {
		t.Errorf("explain compound: %+v", ex)
	}
}

func TestCompoundLeadingRange(t *testing.T) {
	s := New()
	for i := 0; i < 10; i++ {
		_ = s.Insert("m", Document{
			"_id":  idKey(i),
			"user": "u" + idKey(i%3),
			"n":    float64(i),
		})
	}
	_ = s.EnsureIndex("m", []string{"user", "n"}, false)
	docs, err := s.Find("m", Document{"user": "u1", "n": Document{"$gte": 3}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d["user"] != "u1" {
			t.Errorf("residual not applied: %v", d)
		}
		n, _ := toFloat(d["n"])
		if n < 3 {
			t.Errorf("range residual: %v", d)
		}
	}
	ex := s.Explain("m", Document{"user": "u1", "n": Document{"$gte": 3}})
	if ex.Plan != "IXSCAN" || ex.ReMatch != true {
		t.Errorf("range must rematch: %+v", ex)
	}
}

func TestExplainCollScanWhenNoIndex(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "x": 1})
	ex := s.Explain("c", Document{"x": 1})
	if ex.Plan != "COLLSCAN" || !ex.ReMatch {
		t.Errorf("collscan: %+v", ex)
	}
}

// ── 3. Flush no bloqueante + dirtyGen ──

func TestFlushDoesNotLoseConcurrentMutation(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/db.mlstore"
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1", "v": "a"})

	// Snapshot as Flush does, then mutate before clearing dirty (the race window).
	st, err := s.prepareFlush(path, true)
	if err != nil || st == nil {
		t.Fatalf("prepareFlush: %v %v", st, err)
	}
	if err := s.Insert("q", Document{"_id": "2", "v": "b"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(st); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if s.dirtyGen == st.gen {
		s.dirty = false
	}
	stillDirty := s.dirty
	s.mu.Unlock()
	if !stillDirty {
		t.Fatal("mutation after snapshot must keep dirty=true")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, _ := s2.Count("q", nil)
	if n != 2 {
		t.Errorf("docs after final flush: %d", n)
	}
}

func TestFindDuringFlushDoesNotError(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/db.mlstore"
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 200; i++ {
		_ = s.Insert("q", Document{"_id": idKey(i), "n": float64(i)})
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			_ = s.Insert("q", Document{"_id": "x" + idKey(i)})
			if _, err := s.Find("q", Document{"n": Document{"$gte": 0}}, nil); err != nil {
				done <- err
				return
			}
		}
		done <- s.Flush()
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// ── 4. Clone profundo ──

func TestDeepCloneNestedMutation(t *testing.T) {
	s := New()
	src := Document{"_id": "1", "payload": Document{"tags": []any{"a"}, "obj": map[string]any{"k": 1}}}
	_ = s.Insert("c", src)

	// mutate caller nested structures
	p := src["payload"].(Document)
	p["tags"].([]any)[0] = "MUTATED"
	p["obj"].(map[string]any)["k"] = 999

	got, _ := s.Get("c", "1")
	payload := got["payload"].(Document)
	tags := payload["tags"].([]any)
	if tags[0] != "a" {
		t.Errorf("nested array leaked: %v", tags)
	}
	obj := payload["obj"].(map[string]any)
	if obj["k"] != 1 {
		t.Errorf("nested map leaked: %v", obj)
	}
}

func TestUpdatePatchDeepCopy(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "a": 1})
	patchVal := Document{"inner": []any{1}}
	_ = s.Update("c", "1", Document{"p": patchVal})
	patchVal["inner"].([]any)[0] = 42

	got, _ := s.Get("c", "1")
	p := got["p"].(Document)
	if p["inner"].([]any)[0] != 1 {
		t.Errorf("patch not deep-copied: %v", p)
	}
}

func TestProjectionDeepCopy(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "arr": []any{"x"}})
	docs, _ := s.Find("c", nil, &FindOptions{Projection: []string{"arr"}})
	docs[0]["arr"].([]any)[0] = "MUT"
	got, _ := s.Get("c", "1")
	if got["arr"].([]any)[0] != "x" {
		t.Errorf("projection leaked: %v", got["arr"])
	}
}

// ── 6. Regex cache + sensitive por colección ──

func TestRegexCacheRepeated(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "s": "hello"})
	for i := 0; i < 5; i++ {
		docs, err := s.Find("c", Document{"s": Document{"$regex": "^he"}}, nil)
		if err != nil || len(docs) != 1 {
			t.Fatalf("cached regex %d: %v %v", i, docs, err)
		}
	}
	// invalid pattern cached as error → still ErrBadFilter
	_, err := s.Find("c", Document{"s": Document{"$regex": "("}}, nil)
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("bad regex: %v", err)
	}
	_, err = s.Find("c", Document{"s": Document{"$regex": "("}}, nil)
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("bad regex cached: %v", err)
	}
}

func TestSensitiveFieldsPerCollection(t *testing.T) {
	s := New()
	_ = s.Insert("a", Document{"_id": "1", "public": "ok", "internal_note": "hide-me"})
	_ = s.Insert("b", Document{"_id": "1", "internal_note": "keep-me"})
	s.SetSensitiveFields("a", []string{"internal_note"})

	var buf bytes.Buffer
	if err := s.ExportCSV("a", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "hide-me") {
		t.Error("collection-sensitive leaked in coll a")
	}
	buf.Reset()
	if err := s.ExportCSV("b", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "keep-me") {
		t.Error("coll b should still export internal_note")
	}
	if got := s.SensitiveFields("a"); len(got) != 1 || got[0] != "internal_note" {
		t.Errorf("SensitiveFields: %v", got)
	}
}

func TestSensitiveFieldsPersist(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/db.mlstore"
	s, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("a", Document{"_id": "1", "sec": "x", "pub": "y"})
	s.SetSensitiveFields("a", []string{"sec"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.SensitiveFields("a")
	if len(got) != 1 || got[0] != "sec" {
		t.Fatalf("persisted sensitive: %v", got)
	}
	var buf bytes.Buffer
	_ = s2.ExportCSV("a", &buf)
	if strings.Contains(buf.String(), "x") && strings.Contains(buf.String(), "sec") {
		// value x only appears if column exported — check header
		if strings.Contains(strings.SplitN(buf.String(), "\r\n", 2)[0], "sec") {
			t.Error("sec column exported after reload")
		}
	}
}

// helpers

func idKey(i int) string {
	return string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('0'+i%10)) + "_" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func fullScanIDs(t *testing.T, s *Store, coll string, filter Document) []string {
	t.Helper()
	// Force semantic baseline: no index path by cloning filter match via Find on empty-index store behavior.
	// Find always re-matches when not exact; for baseline use Count/Find after ensuring no index.
	docs, err := s.Find(coll, filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ids(docs)
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := map[string]int{}
	for _, x := range a {
		sa[x]++
	}
	for _, x := range b {
		sa[x]--
		if sa[x] < 0 {
			return false
		}
	}
	return true
}
