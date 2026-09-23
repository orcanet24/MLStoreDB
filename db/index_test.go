package db

import (
	"errors"
	"testing"
)

func TestEnsureIndexAndList(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "status": "OPEN"})
	if err := s.EnsureIndex("q", []string{"status"}, false); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := s.EnsureIndex("q", []string{"status"}, false); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListIndexes("q")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListIndexes: %v %v", list, err)
	}
	if list[0].Fields[0] != "status" || list[0].Unique {
		t.Errorf("info: %+v", list[0])
	}
}

func TestUniqueIndexBlocksDuplicate(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "code": "A"})
	if err := s.EnsureIndex("q", []string{"code"}, true); err != nil {
		t.Fatal(err)
	}
	err := s.Insert("q", Document{"_id": "2", "code": "A"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	// same _id re-insert still ErrDuplicate (already exists)
	err = s.Insert("q", Document{"_id": "1", "code": "B"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("_id dup: %v", err)
	}
	// different code ok
	if err := s.Insert("q", Document{"_id": "3", "code": "B"}); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueIndexOnExistingDocs(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "code": "A"})
	_ = s.Insert("q", Document{"_id": "2", "code": "A"})
	err := s.EnsureIndex("q", []string{"code"}, true)
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("build unique on conflict: %v", err)
	}
	// no index left half-applied
	list, _ := s.ListIndexes("q")
	if len(list) != 0 {
		t.Errorf("indexes after failed build: %v", list)
	}
}

func TestUniqueIndexOnUpdate(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "code": "A"})
	_ = s.Insert("q", Document{"_id": "2", "code": "B"})
	_ = s.EnsureIndex("q", []string{"code"}, true)

	err := s.Update("q", "2", Document{"code": "A"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("update clash: %v", err)
	}
	// doc 2 unchanged
	d, _ := s.Get("q", "2")
	if d["code"] != "B" {
		t.Errorf("rollback failed: %v", d)
	}
	// update to free value ok
	if err := s.Update("q", "2", Document{"code": "C"}); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueIndexOnUpsert(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "code": "A"})
	_ = s.EnsureIndex("q", []string{"code"}, true)
	err := s.Upsert("q", "2", Document{"code": "A"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("upsert clash: %v", err)
	}
	// upsert same doc id with same code is fine
	if err := s.Upsert("q", "1", Document{"code": "A"}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexMaintainedOnDelete(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "code": "A"})
	_ = s.EnsureIndex("q", []string{"code"}, true)
	if err := s.Delete("q", "1"); err != nil {
		t.Fatal(err)
	}
	// code A free again
	if err := s.Insert("q", Document{"_id": "2", "code": "A"}); err != nil {
		t.Fatal(err)
	}
}

func TestDropIndex(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "code": "A"})
	if err := s.EnsureIndex("q", []string{"code"}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "2", "code": "A"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("before drop: %v", err)
	}
	if err := s.DropIndex("q", []string{"code"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "2", "code": "A"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropIndex("q", []string{"nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("drop missing: %v", err)
	}
}

func TestCompoundUniqueIndex(t *testing.T) {
	s := New()
	_ = s.Insert("m", Document{"_id": "1", "user": "u1", "acct": "a1"})
	if err := s.EnsureIndex("m", []string{"user", "acct"}, true); err != nil {
		t.Fatal(err)
	}
	// same pair blocked
	err := s.Insert("m", Document{"_id": "2", "user": "u1", "acct": "a1"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("compound clash: %v", err)
	}
	// different second field ok
	if err := s.Insert("m", Document{"_id": "3", "user": "u1", "acct": "a2"}); err != nil {
		t.Fatal(err)
	}
}

func TestFindUsesIndexEquality(t *testing.T) {
	s := New()
	for i := 0; i < 5; i++ {
		_ = s.Insert("q", Document{"_id": string(rune('a' + i)), "status": "S"})
	}
	_ = s.EnsureIndex("q", []string{"status"}, false)
	docs, err := s.Find("q", Document{"status": "S"}, nil)
	if err != nil || len(docs) != 5 {
		t.Fatalf("indexed eq: %d %v", len(docs), err)
	}
	// planner used — result must equal full-scan semantics (already covered by other tests)
	docs, _ = s.Find("q", Document{"status": "NOPE"}, nil)
	if len(docs) != 0 {
		t.Errorf("miss: %v", ids(docs))
	}
}

func TestFindIndexWithIn(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "st": "A"})
	_ = s.Insert("q", Document{"_id": "2", "st": "B"})
	_ = s.Insert("q", Document{"_id": "3", "st": "C"})
	_ = s.EnsureIndex("q", []string{"st"}, false)
	docs, err := s.Find("q", Document{"st": Document{"$in": []any{"A", "C"}}}, nil)
	if err != nil || len(docs) != 2 {
		t.Fatalf("$in indexed: %v %v", ids(docs), err)
	}
}

func TestFindIndexIntersectsEqualities(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "a": "x", "b": "y"})
	_ = s.Insert("q", Document{"_id": "2", "a": "x", "b": "z"})
	_ = s.Insert("q", Document{"_id": "3", "a": "w", "b": "y"})
	_ = s.EnsureIndex("q", []string{"a"}, false)
	_ = s.EnsureIndex("q", []string{"b"}, false)
	docs, err := s.Find("q", Document{"a": "x", "b": "y"}, nil)
	if err != nil || len(docs) != 1 || ids(docs)[0] != "1" {
		t.Fatalf("intersect: %v %v", ids(docs), err)
	}
}

func TestProjection(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "a": 1, "b": 2, "c": 3})
	docs, err := s.Find("q", nil, &FindOptions{Projection: []string{"a"}})
	if err != nil || len(docs) != 1 {
		t.Fatal(err)
	}
	d := docs[0]
	if _, ok := d["b"]; ok {
		t.Errorf("b should be projected out: %v", d)
	}
	if d["a"] != 1 || d["_id"] != "1" {
		t.Errorf("projection: %v", d)
	}
}

func TestNullAndMissingIndexSameSentinel(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "v": nil})
	_ = s.Insert("q", Document{"_id": "2"})
	_ = s.Insert("q", Document{"_id": "3", "v": 1})
	if err := s.EnsureIndex("q", []string{"v"}, true); err == nil {
		// null + missing both sentinel → should clash on unique
		t.Fatal("expected unique clash null vs missing")
	}
	// non-unique is fine
	if err := s.EnsureIndex("q", []string{"v"}, false); err != nil {
		t.Fatal(err)
	}
	docs, err := s.Find("q", Document{"v": nil}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// implicit eq null matches missing too (match semantics)
	if len(docs) < 2 {
		t.Errorf("null eq: %v", ids(docs))
	}
}

func TestMultiValueArrayIndex(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "tags": []any{"a", "b"}})
	_ = s.Insert("q", Document{"_id": "2", "tags": []any{"b", "c"}})
	if err := s.EnsureIndex("q", []string{"tags"}, true); err == nil {
		// b appears in both → unique clash
		t.Fatal("expected array unique clash")
	}
	if err := s.EnsureIndex("q", []string{"tags"}, false); err != nil {
		t.Fatal(err)
	}
	// index maintained: delete frees b for one doc
	if err := s.Delete("q", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "3", "tags": []any{"a"}}); err != nil {
		t.Fatal(err)
	}
}
