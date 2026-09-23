package db

import (
	"errors"
	"strings"
	"testing"
)

func seedGraph(t *testing.T) *Store {
	t.Helper()
	s := New()
	// a→b→c→d, a→c, x←y
	edges := [][2]string{
		{"a", "b"}, {"b", "c"}, {"c", "d"}, {"a", "c"},
		{"y", "x"},
	}
	for _, e := range edges {
		if _, err := s.AddEdge("knows", e[0], e[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestAddEdgeAndNeighbors(t *testing.T) {
	s := seedGraph(t)
	got, err := s.Neighbors("knows", "a", Outgoing)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("out(a) = %v", got)
	}
	got, _ = s.Neighbors("knows", "c", Outgoing)
	if len(got) != 1 || got[0] != "d" {
		t.Errorf("out(c) = %v", got)
	}
	got, _ = s.Neighbors("knows", "c", Incoming)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("in(c) = %v", got)
	}
	got, _ = s.Neighbors("knows", "b", Both)
	// out: c ; in: a
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("both(b) = %v", got)
	}
	// unknown vertex / type
	got, err = s.Neighbors("knows", "zzz", Outgoing)
	if err != nil || len(got) != 0 {
		t.Errorf("unknown vertex = %v, %v", got, err)
	}
	got, err = s.Neighbors("nope", "a", Outgoing)
	if err != nil || len(got) != 0 {
		t.Errorf("unknown type = %v, %v", got, err)
	}
}

func TestAddEdgeValidation(t *testing.T) {
	s := New()
	if _, err := s.AddEdge("", "a", "b", nil); err == nil {
		t.Error("empty type must fail")
	}
	if _, err := s.AddEdge("knows", "", "b", nil); err == nil {
		t.Error("empty from must fail")
	}
	if _, err := s.AddEdge("knows", "a", "", nil); err == nil {
		t.Error("empty to must fail")
	}
	id, err := s.AddEdge("knows", "a", "b", Document{"w": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Error("missing id")
	}
	d, err := s.Get(edgeCollName("knows"), id)
	if err != nil {
		t.Fatal(err)
	}
	if d["_from"] != "a" || d["_to"] != "b" || d["w"] != float64(2) {
		t.Errorf("edge doc = %v", d)
	}
}

func TestGraphCacheInvalidation(t *testing.T) {
	s := seedGraph(t)
	if _, err := s.Neighbors("knows", "a", Outgoing); err != nil {
		t.Fatal(err) // build cache
	}
	// AddEdge visible
	if _, err := s.AddEdge("knows", "a", "z", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Neighbors("knows", "a", Outgoing)
	found := false
	for _, v := range got {
		if v == "z" {
			found = true
		}
	}
	if !found {
		t.Errorf("cache stale after AddEdge: %v", got)
	}
	// generic Delete on edges.* also invalidates
	ids := FindIDs(t, s, edgeCollName("knows"), Document{"_from": "a", "_to": "z"})
	if len(ids) != 1 {
		t.Fatalf("ids = %v", ids)
	}
	if err := s.Delete(edgeCollName("knows"), ids[0]); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Neighbors("knows", "a", Outgoing)
	for _, v := range got {
		if v == "z" {
			t.Errorf("cache stale after Delete: %v", got)
		}
	}
	// RemoveEdge
	id, _ := s.AddEdge("knows", "q", "r", nil)
	if err := s.RemoveEdge("knows", id); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Neighbors("knows", "q", Outgoing)
	if len(got) != 0 {
		t.Errorf("RemoveEdge leftover: %v", got)
	}
	// generic Insert invalidates too
	if err := s.Insert(edgeCollName("knows"), Document{"_id": "e1", "_from": "m", "_to": "n"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Neighbors("knows", "m", Outgoing)
	if len(got) != 1 || got[0] != "n" {
		t.Errorf("stale after Insert: %v", got)
	}
}

// FindIDs is a tiny test helper for id lookup.
func FindIDs(t *testing.T, s *Store, coll string, filter Document) []string {
	t.Helper()
	docs, err := s.Find(coll, filter, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d["_id"].(string))
	}
	return ids
}

func TestTraverseDepthAndLimit(t *testing.T) {
	s := seedGraph(t)
	// full BFS from a: a(0), b(1), c(1), d(2)
	res, err := s.Traverse(TraverseOptions{EdgeType: "knows", Start: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 4 {
		t.Fatalf("traverse = %v", res)
	}
	if res[0].Vertex != "a" || res[0].Depth != 0 {
		t.Errorf("start = %v", res[0])
	}
	depths := map[string]int{}
	for _, n := range res {
		depths[n.Vertex] = n.Depth
	}
	if depths["b"] != 1 || depths["c"] != 1 || depths["d"] != 2 {
		t.Errorf("depths = %v", depths)
	}

	// MaxDepth=1 → a,b,c only
	res, _ = s.Traverse(TraverseOptions{EdgeType: "knows", Start: "a", MaxDepth: 1})
	if len(res) != 3 {
		t.Errorf("maxdepth1 = %v", res)
	}

	// MinDepth=1 excludes start
	res, _ = s.Traverse(TraverseOptions{EdgeType: "knows", Start: "a", MinDepth: 1})
	if len(res) != 3 || res[0].Vertex == "a" {
		t.Errorf("mindepth1 = %v", res)
	}

	// Limit=2
	res, _ = s.Traverse(TraverseOptions{EdgeType: "knows", Start: "a", Limit: 2})
	if len(res) != 2 {
		t.Errorf("limit2 = %v", res)
	}

	// Incoming from d → only via incoming edges (none into d? c→d means in(d)=c)
	res, _ = s.Traverse(TraverseOptions{EdgeType: "knows", Start: "d", Direction: Incoming})
	verts := map[string]bool{}
	for _, n := range res {
		verts[n.Vertex] = true
	}
	// d, c, b, a via incoming reversed walk: d←c←b←a and c←a
	if !verts["c"] || !verts["a"] {
		t.Errorf("incoming traverse = %v", res)
	}

	// clamps: absurd values don't hang
	res, err = s.Traverse(TraverseOptions{EdgeType: "knows", Start: "a", MaxDepth: 9999, Limit: 999999999})
	if err != nil || len(res) != 4 {
		t.Errorf("clamp = %d, %v", len(res), err)
	}

	// missing start required
	if _, err := s.Traverse(TraverseOptions{EdgeType: "knows"}); err == nil {
		t.Error("missing start must fail")
	}
}

func TestShortestPath(t *testing.T) {
	s := seedGraph(t)
	path, err := s.ShortestPath("knows", "a", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	// a→c→d (length 2 edges, 3 verts) is shortest (a→b→c→d is 3 edges)
	want := []string{"a", "c", "d"}
	if len(path) != len(want) {
		t.Fatalf("path = %v", path)
	}
	for i := range want {
		if path[i] != want[i] {
			t.Fatalf("path = %v, want %v", path, want)
		}
	}
	// same vertex
	path, _ = s.ShortestPath("knows", "a", "a", 0)
	if len(path) != 1 || path[0] != "a" {
		t.Errorf("same = %v", path)
	}
	// unreachable (x has no out to a)
	path, err = s.ShortestPath("knows", "x", "a", 0)
	if err != nil || path != nil {
		t.Errorf("unreachable = %v, %v", path, err)
	}
	// maxDepth=1 too shallow for a→d
	path, _ = s.ShortestPath("knows", "a", "d", 1)
	if path != nil {
		t.Errorf("shallow = %v", path)
	}
	// maxDepth=2 works
	path, _ = s.ShortestPath("knows", "a", "d", 2)
	if len(path) != 3 {
		t.Errorf("depth2 = %v", path)
	}
	// reverse path from d to a: no outgoing from d → nil
	path, _ = s.ShortestPath("knows", "d", "a", 0)
	if path != nil {
		t.Errorf("reverse = %v", path)
	}
}

func TestGraphDedupMultiEdge(t *testing.T) {
	s := New()
	_, _ = s.AddEdge("e", "a", "b", nil)
	_, _ = s.AddEdge("e", "a", "b", nil) // parallel edge
	got, _ := s.Neighbors("e", "a", Outgoing)
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("dedup = %v", got)
	}
}

func TestGraphRBAC(t *testing.T) {
	s := New()
	s.opts.LightKDF = true
	// seed edges while open
	if _, err := s.AddEdge("knows", "a", "b", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRole("none", []Permission{{Collection: "other", Read: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("bob", "secret-reader-1", []string{"none"}); err != nil {
		t.Fatal(err)
	}
	// raw API denied once auth active
	if _, err := s.Neighbors("knows", "a", Outgoing); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("raw Neighbors = %v", err)
	}
	if _, err := s.Traverse(TraverseOptions{EdgeType: "knows", Start: "a"}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("raw Traverse = %v", err)
	}
	// admin session OK
	admin, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := admin.Neighbors("knows", "a", Outgoing)
	if err != nil || len(got) != 1 || got[0] != "b" {
		t.Errorf("admin Neighbors = %v, %v", got, err)
	}
	// session without edge coll perm → Forbidden
	bob, _ := s.Authenticate("bob", "secret-reader-1")
	if _, err := bob.Neighbors("knows", "a", Outgoing); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob Neighbors = %v", err)
	}
	// session AddEdge with write via admin
	if _, err := admin.AddEdge("knows", "b", "c", nil); err != nil {
		t.Errorf("admin AddEdge = %v", err)
	}
}

func TestGraphTypesAreDistinct(t *testing.T) {
	s := New()
	_, _ = s.AddEdge("knows", "a", "b", nil)
	_, _ = s.AddEdge("likes", "a", "z", nil)
	got, _ := s.Neighbors("knows", "a", Outgoing)
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("knows = %v", got)
	}
	got, _ = s.Neighbors("likes", "a", Outgoing)
	if len(got) != 1 || got[0] != "z" {
		t.Errorf("likes = %v", got)
	}
	// edge colls hidden from Collections? They are user-visible (not system) —
	// they SHOULD appear (they are normal colls).
	names := s.Collections()
	hasKnows := false
	for _, n := range names {
		if n == "edges.knows" {
			hasKnows = true
		}
	}
	if !hasKnows {
		t.Errorf("edges.knows missing from Collections: %v", names)
	}
	if !strings.HasPrefix("edges.knows", edgeCollPrefix) {
		t.Error("prefix helper broken")
	}
}
