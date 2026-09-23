package db

import (
	"errors"
	"testing"
)

func seedQuestions(s *Store) {
	_ = s.Insert("q", Document{"_id": "1", "status": "UNANSWERED", "n": 10, "tag": "alpha", "closed": false})
	_ = s.Insert("q", Document{"_id": "2", "status": "ANSWERED", "n": 20, "tag": "beta"})
	_ = s.Insert("q", Document{"_id": "3", "status": "UNANSWERED", "n": 30, "tag": "Alpha"})
	_ = s.Insert("q", Document{"_id": "4", "status": "CLOSED", "n": 40})
	_ = s.Insert("q", Document{"_id": "5", "from": Document{"name": "nested"}})
}

func ids(docs []Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i], _ = d["_id"].(string)
	}
	return out
}

func TestFilterComparisonOps(t *testing.T) {
	s := New()
	seedQuestions(s)

	cases := []struct {
		name   string
		filter Document
		want   []string
	}{
		{"$gt", Document{"n": Document{"$gt": 20}}, []string{"3", "4"}},
		{"$gte", Document{"n": Document{"$gte": 30}}, []string{"3", "4"}},
		{"$lt", Document{"n": Document{"$lt": 20}}, []string{"1"}},
		{"$lte", Document{"n": Document{"$lte": 10}}, []string{"1"}},
		{"$ne", Document{"status": Document{"$ne": "UNANSWERED"}}, []string{"2", "4", "5"}},
		{"$eq explicit", Document{"tag": Document{"$eq": "beta"}}, []string{"2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := s.Find("q", tc.filter, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := ids(docs)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

func TestFilterInNin(t *testing.T) {
	s := New()
	seedQuestions(s)

	docs, err := s.Find("q", Document{"_id": Document{"$in": []any{"1", "3"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || ids(docs)[0] != "1" || ids(docs)[1] != "3" {
		t.Errorf("$in: %v", ids(docs))
	}

	docs, _ = s.Find("q", Document{"_id": Document{"$nin": []any{"1", "3"}}}, nil)
	if len(docs) != 3 {
		t.Errorf("$nin: %v", ids(docs))
	}
}

func TestFilterExists(t *testing.T) {
	s := New()
	seedQuestions(s)

	docs, err := s.Find("q", Document{"tag": Document{"$exists": true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 {
		t.Errorf("$exists true: %v", ids(docs))
	}

	docs, _ = s.Find("q", Document{"tag": Document{"$exists": false}}, nil)
	if len(docs) != 2 {
		t.Errorf("$exists false: %v", ids(docs))
	}
}

func TestFilterRegex(t *testing.T) {
	s := New()
	seedQuestions(s)

	docs, err := s.Find("q", Document{"tag": Document{"$regex": "^a", "$options": "i"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Errorf("case-insensitive ^a: %v", ids(docs))
	}

	docs, _ = s.Find("q", Document{"tag": Document{"$regex": "^a"}}, nil)
	if len(docs) != 1 || ids(docs)[0] != "1" {
		t.Errorf("case-sensitive ^a: %v", ids(docs))
	}
}

func TestFilterLogicalAndOrNot(t *testing.T) {
	s := New()
	seedQuestions(s)

	docs, err := s.Find("q", Document{
		"$and": []any{
			Document{"status": "UNANSWERED"},
			Document{"n": Document{"$gte": 30}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || ids(docs)[0] != "3" {
		t.Errorf("$and: %v", ids(docs))
	}

	docs, _ = s.Find("q", Document{
		"$or": []any{
			Document{"status": "ANSWERED"},
			Document{"status": "CLOSED"},
		},
	}, nil)
	if len(docs) != 2 {
		t.Errorf("$or: %v", ids(docs))
	}

	docs, _ = s.Find("q", Document{
		"$not": Document{"status": "UNANSWERED"},
	}, nil)
	if len(docs) != 3 {
		t.Errorf("$not: %v", ids(docs))
	}
}

func TestFilterDottedPath(t *testing.T) {
	s := New()
	seedQuestions(s)

	docs, err := s.Find("q", Document{"from.name": "nested"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || ids(docs)[0] != "5" {
		t.Errorf("dotted: %v", ids(docs))
	}
}

func TestFilterDottedPathThroughArray(t *testing.T) {
	s := New()
	_ = s.Insert("o", Document{"_id": "1", "items": []any{
		Document{"item_id": "A"},
		Document{"item_id": "B"},
	}})
	_ = s.Insert("o", Document{"_id": "2", "items": []any{
		Document{"item_id": "C"},
	}})
	docs, err := s.Find("o", Document{"items.item_id": "B"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || ids(docs)[0] != "1" {
		t.Errorf("array path: %v", ids(docs))
	}
}

func TestFilterBadOperator(t *testing.T) {
	s := New()
	seedQuestions(s)
	_, err := s.Find("q", Document{"n": Document{"$bogus": 1}}, nil)
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("want ErrBadFilter, got %v", err)
	}
}

func TestFilterCombinedWithSortLimit(t *testing.T) {
	s := New()
	seedQuestions(s)
	docs, err := s.Find("q", Document{"n": Document{"$gte": 10}}, &FindOptions{
		Sort:  map[string]int{"n": -1},
		Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0]["n"] != 40 {
		t.Errorf("filter+sort+limit: %v", ids(docs))
	}
}

func TestFilterEqualityStillWorks(t *testing.T) {
	s := New()
	seedQuestions(s)
	docs, _ := s.Find("q", Document{"status": "UNANSWERED"}, nil)
	if len(docs) != 2 {
		t.Errorf("implicit equality: %v", ids(docs))
	}
}
