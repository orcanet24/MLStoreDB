package db

import (
	"errors"
	"testing"
)

func TestInsertAndGet(t *testing.T) {
	s := New()
	err := s.Insert("users", Document{"_id": "1", "email": "a@b.c", "name": "Ana"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.Get("users", "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got["email"] != "a@b.c" {
		t.Errorf("email = %v", got["email"])
	}
}

func TestInsertRequiresID(t *testing.T) {
	s := New()
	// missing _id → auto ULID (DESIGN §3.1)
	if err := s.Insert("users", Document{"email": "x"}); err != nil {
		t.Errorf("auto ULID: %v", err)
	}
	if err := s.Insert("users", Document{"_id": 42}); !errors.Is(err, ErrNoID) {
		t.Errorf("want ErrNoID for non-string, got %v", err)
	}
}

func TestInsertDuplicate(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "x"})
	if err := s.Insert("c", Document{"_id": "x"}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("want ErrDuplicate, got %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := New()
	if _, err := s.Get("nope", "1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if _, err := s.Get("c", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestUpsertCreatesAndReplaces(t *testing.T) {
	s := New()
	_ = s.Upsert("ml_orders", "100", Document{"status": "paid", "total": 10})
	_ = s.Upsert("ml_orders", "100", Document{"status": "shipped"})
	got, err := s.Get("ml_orders", "100")
	if err != nil {
		t.Fatal(err)
	}
	if got["status"] != "shipped" {
		t.Errorf("status = %v", got["status"])
	}
	if got["_id"] != "100" {
		t.Errorf("_id = %v", got["_id"])
	}
	if _, ok := got["total"]; ok {
		t.Error("upsert should replace, not merge")
	}
}

func TestUpdateShallowMerge(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "a": 1, "b": 2})
	err := s.Update("c", "1", Document{"b": 3, "c": 4, "_id": "hacked"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("c", "1")
	if got["a"] != 1 || got["b"] != 3 || got["c"] != 4 {
		t.Errorf("merge wrong: %v", got)
	}
	if got["_id"] != "1" {
		t.Error("_id must not change")
	}
	if err := s.Update("c", "999", Document{"x": 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1"})
	if err := s.Delete("c", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("c", "1"); !errors.Is(err, ErrNotFound) {
		t.Error("should be gone")
	}
	if err := s.Delete("c", "1"); !errors.Is(err, ErrNotFound) {
		t.Error("second delete → ErrNotFound")
	}
}

func TestFindAllAndEquality(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "status": "UNANSWERED", "ml": "A"})
	_ = s.Insert("q", Document{"_id": "2", "status": "ANSWERED", "ml": "A"})
	_ = s.Insert("q", Document{"_id": "3", "status": "UNANSWERED", "ml": "B"})

	all, err := s.Find("q", nil, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("all=%d err=%v", len(all), err)
	}
	// ordered by _id
	if all[0]["_id"] != "1" || all[2]["_id"] != "3" {
		t.Error("not sorted by _id")
	}

	un, _ := s.Find("q", Document{"status": "UNANSWERED"}, nil)
	if len(un) != 2 {
		t.Errorf("unanswered = %d", len(un))
	}

	both, _ := s.Find("q", Document{"status": "UNANSWERED", "ml": "B"}, nil)
	if len(both) != 1 || both[0]["_id"] != "3" {
		t.Errorf("compound equality failed: %v", both)
	}

	none, _ := s.Find("q", Document{"status": "CLOSED"}, nil)
	if len(none) != 0 {
		t.Error("want empty")
	}
}

func TestFindNumericEquality(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "1", "n": 5})
	_ = s.Insert("c", Document{"_id": "2", "n": 6})
	docs, _ := s.Find("c", Document{"n": 5}, nil)
	if len(docs) != 1 || docs[0]["_id"] != "1" {
		t.Errorf("int/float equality: %v", docs)
	}
}

func TestFindSkipLimitSort(t *testing.T) {
	s := New()
	_ = s.Insert("c", Document{"_id": "a", "n": 3})
	_ = s.Insert("c", Document{"_id": "b", "n": 1})
	_ = s.Insert("c", Document{"_id": "c", "n": 2})

	docs, _ := s.Find("c", nil, &FindOptions{Sort: map[string]int{"n": 1}})
	if len(docs) != 3 || toInt(docs[0]["n"]) != 1 {
		t.Errorf("sort asc: %v", docs)
	}

	docs, _ = s.Find("c", nil, &FindOptions{Sort: map[string]int{"n": -1}, Limit: 2})
	if len(docs) != 2 || toInt(docs[0]["n"]) != 3 {
		t.Errorf("sort desc limit: %v", docs)
	}

	docs, _ = s.Find("c", nil, &FindOptions{Skip: 1, Limit: 1})
	if len(docs) != 1 || docs[0]["_id"] != "b" {
		t.Errorf("skip/limit by _id order: %v", docs)
	}
}

func TestCountAndCollections(t *testing.T) {
	s := New()
	_ = s.Insert("a", Document{"_id": "1"})
	_ = s.Insert("a", Document{"_id": "2"})
	_ = s.Insert("b", Document{"_id": "1"})
	n, _ := s.Count("a", nil)
	if n != 2 {
		t.Errorf("count = %d", n)
	}
	names := s.Collections()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("collections = %v", names)
	}
}

func TestCallerCannotMutateStoredDoc(t *testing.T) {
	s := New()
	src := Document{"_id": "1", "tags": []any{"x"}}
	_ = s.Insert("c", src)
	src["email"] = "mutated"
	got, _ := s.Get("c", "1")
	if _, ok := got["email"]; ok {
		t.Error("stored doc must be a copy")
	}
}

func TestSchemaVersion(t *testing.T) {
	s := New()
	// Fresh store = 0 (unmigrated); migrations bump to N.
	if s.SchemaVersion() != 0 {
		t.Errorf("version = %d", s.SchemaVersion())
	}
	if err := s.ApplyMigrations([]Migration{{Version: 1, Name: "init", Up: func(*Store) error { return nil }}}); err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion() != 1 {
		t.Errorf("after migration = %d", s.SchemaVersion())
	}
}

func toInt(v any) int {
	f, _ := toFloat(v)
	return int(f)
}
