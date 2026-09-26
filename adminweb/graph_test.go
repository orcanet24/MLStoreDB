package adminweb

import (
	"testing"

	"mlstoredb/db"
)

func seedGraph(t *testing.T, te *testEnv) {
	t.Helper()
	te.do("POST", "/api/collections", map[string]string{"name": "people"}, true)
	te.do("POST", "/api/collections/people/docs", db.Document{"_id": "ana", "name": "Ana"}, true)
	te.do("POST", "/api/collections/people/docs", db.Document{"_id": "bob", "name": "Bob"}, true)
	te.do("POST", "/api/collections/people/docs", db.Document{"_id": "carla", "name": "Carla"}, true)
	te.do("POST", "/api/graph/edge", map[string]any{"type": "knows", "from": "ana", "to": "bob"}, true)
	te.do("POST", "/api/graph/edge", map[string]any{"type": "knows", "from": "bob", "to": "carla", "props": db.Document{"desde": 2024.0}}, true)
}

func TestGraphMetaAndEdgeCreate(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	seedGraph(t, te)

	code, out := te.do("GET", "/api/graph", nil, false)
	if code != 200 {
		t.Fatalf("meta: %d", code)
	}
	edgeTypes := out["edgeTypes"].([]any)
	vertexColls := out["vertexColls"].([]any)
	if len(edgeTypes) != 1 || edgeTypes[0] != "knows" {
		t.Errorf("edgeTypes: %v", edgeTypes)
	}
	if len(vertexColls) != 1 || vertexColls[0] != "people" {
		t.Errorf("vertexColls: %v", vertexColls)
	}

	code, out = te.do("POST", "/api/graph/edge", map[string]any{"type": "knows", "from": "ana", "to": "carla"}, true)
	if code != 201 || out["id"] == "" {
		t.Fatalf("create edge: %d %v", code, out)
	}
	if code, _ := te.do("POST", "/api/graph/edge", map[string]any{"type": "", "from": "a", "to": "b"}, true); code != 400 {
		t.Errorf("empty type should 400, got %d", code)
	}
}

func TestGraphNeighborsAndResolve(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	seedGraph(t, te)

	code, out := te.do("GET", "/api/graph/neighbors?edge=knows&vertex=bob", nil, false)
	if code != 200 {
		t.Fatalf("neighbors: %d", code)
	}
	edges := out["edges"].([]any)
	neighbors := out["neighbors"].([]any)
	if len(edges) != 2 {
		t.Fatalf("bob should have 2 edges: %v", edges)
	}
	if len(neighbors) != 2 {
		t.Fatalf("bob neighbors: %v", neighbors)
	}

	code, out = te.do("POST", "/api/graph/resolve", map[string]any{"ids": []string{"ana", "carla", "zzz"}}, true)
	if code != 200 {
		t.Fatalf("resolve: %d", code)
	}
	found := out["found"].([]any)
	if len(found) != 2 {
		t.Fatalf("resolve found: %v", found)
	}
	first := found[0].(map[string]any)
	if first["coll"] != "people" || first["doc"].(map[string]any)["name"] != "Ana" {
		t.Errorf("resolve content: %v", first)
	}
}

func TestGraphTraverseAndPath(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	seedGraph(t, te)

	code, out := te.do("GET", "/api/graph/traverse?edge=knows&start=ana&maxDepth=2", nil, false)
	if code != 200 {
		t.Fatalf("traverse: %d", code)
	}
	nodes := out["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("traverse nodes: %v", nodes)
	}

	code, out = te.do("GET", "/api/graph/path?edge=knows&from=ana&to=carla&maxDepth=4", nil, false)
	if code != 200 {
		t.Fatalf("path: %d", code)
	}
	path := out["path"].([]any)
	if len(path) != 3 || path[0] != "ana" || path[2] != "carla" {
		t.Fatalf("path: %v", path)
	}

	code, out = te.do("GET", "/api/graph/path?edge=knows&from=carla&to=zzz&maxDepth=2", nil, false)
	if len(out["path"].([]any)) != 0 {
		t.Errorf("no path expected: %v", out["path"])
	}
}

func TestGraphEdgeDeleteAndPermissions(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	seedGraph(t, te)

	_, out := te.do("GET", "/api/graph/neighbors?edge=knows&vertex=ana", nil, false)
	edges := out["edges"].([]any)
	edgeID := edges[0].(map[string]any)["id"].(string)

	code, _ := te.do("DELETE", "/api/graph/edge?type=knows&id="+edgeID, nil, true)
	if code != 200 {
		t.Fatalf("delete edge: %d", code)
	}
	_, out = te.do("GET", "/api/graph/neighbors?edge=knows&vertex=ana", nil, false)
	if len(out["edges"].([]any)) != 0 {
		t.Errorf("edge survived delete")
	}

	te.store.CreateRole("peoplereader", []db.Permission{{Collection: "people", Read: true}})
	te.store.CreateUser("viewer2", "pass123", []string{"peoplereader"})
	te2 := newTestEnvWithStore(t, te.store)
	te2.login("viewer2", "pass123")
	if code, _ := te2.do("POST", "/api/graph/edge", map[string]any{"type": "knows", "from": "ana", "to": "bob"}, true); code != 403 {
		t.Errorf("read-only user should not create edges, got %d", code)
	}
	if code, _ := te2.do("GET", "/api/graph/neighbors?edge=knows&vertex=ana", nil, false); code != 403 {
		t.Errorf("read-only on edges.* should 403, got %d", code)
	}
}
