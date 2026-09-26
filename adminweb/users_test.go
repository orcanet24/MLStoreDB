package adminweb

import (
	"encoding/json"
	"testing"

	"mlstoredb/db"
)

func TestUsersRolesAdmin(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()

	store := te.store
	store.CreateRole("editor", []db.Permission{{Collection: "orders", Read: true, Write: true}})
	store.CreateRole("viewer", []db.Permission{{Collection: "orders", Read: true}})
	store.CreateUser("bob", "secret1", []string{"editor"})
	store.CreateUser("carla", "secret1", []string{"viewer"})

	code, out := te.do("GET", "/api/roles", nil, false)
	if code != 200 {
		t.Fatalf("list roles: %d %v", code, out)
	}
	t.Logf("roles raw: %v", out)
	code, out = te.do("GET", "/api/users", nil, false)
	if code != 200 {
		t.Fatalf("list users: %d", code)
	}
	users := out["users"].([]any)
	if len(users) != 3 {
		t.Fatalf("expected 3 users: %v", users)
	}

	code, out = te.do("POST", "/api/users", map[string]any{
		"username": "dave", "password": "secret1", "roles": []string{"viewer"},
	}, true)
	if code != 201 {
		t.Fatalf("create user: %d %v", code, out)
	}

	code, out = te.do("POST", "/api/users", map[string]any{
		"username": "x", "password": "secret1",
	}, true)
	if code != 400 {
		t.Errorf("short username should 400, got %d", code)
	}

	code, _ = te.do("DELETE", "/api/users/dave", nil, true)
	if code != 200 {
		t.Fatalf("delete user: %d", code)
	}

	code, out = te.do("GET", "/api/roles", nil, false)
	if code != 200 {
		t.Fatalf("list roles: %d", code)
	}
	roles := out["roles"].([]any)
	if len(roles) != 3 {
		t.Fatalf("expected 3 roles: %v", roles)
	}
	var editorRole map[string]any
	for _, r := range roles {
		rl := r.(map[string]any)
		if rl["name"] == "editor" {
			editorRole = rl
		}
	}
	if editorRole == nil {
		t.Fatalf("editor role not found: %v", roles)
	}
	perms := editorRole["permissions"].([]any)
	if len(perms) != 1 {
		t.Errorf("editor perms: %v", perms)
	}

	code, out = te.do("POST", "/api/roles", map[string]any{
		"name":        "temp",
		"permissions": []any{map[string]any{"collection": "*", "read": true, "write": false}},
	}, true)
	if code != 201 {
		t.Fatalf("create role: %d %v", code, out)
	}
	code, _ = te.do("DELETE", "/api/roles/temp", nil, true)
	if code != 200 {
		t.Fatalf("delete role: %d", code)
	}
	code, out = te.do("DELETE", "/api/roles/editor", nil, true)
	if code != 409 {
		t.Errorf("delete role in use should 409, got %d %v", code, out)
	}
}

func TestAdminOnlyGuard(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.store.CreateRole("reader", []db.Permission{{Collection: "x", Read: true}})
	te.store.CreateUser("peon", "secret1", []string{"reader"})

	te2 := newTestEnvWithStore(t, te.store)
	te2.login("peon", "secret1")
	code, _ := te2.do("GET", "/api/users", nil, false)
	if code != 403 {
		t.Errorf("non-admin should get 403 on users, got %d", code)
	}
	code, _ = te2.do("GET", "/api/roles", nil, false)
	if code != 403 {
		t.Errorf("non-admin should get 403 on roles, got %d", code)
	}
	code, _ = te2.do("DELETE", "/api/users/bob", nil, true)
	if code != 403 {
		t.Errorf("non-admin should get 403 on delete user, got %d", code)
	}
}

func TestQueryConsole(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "orders"}, true)
	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "1", "status": "PAID", "n": 10}, true)
	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "2", "status": "PAID", "n": 20}, true)
	te.do("POST", "/api/collections/orders/docs", db.Document{"_id": "3", "status": "OPEN", "n": 5}, true)

	code, out := te.do("POST", "/api/query", map[string]any{
		"coll": "orders", "filter": map[string]any{"status": "PAID"}, "limit": 50,
	}, true)
	if code != 200 {
		t.Fatalf("query: %d %v", code, out)
	}
	if out["total"] != float64(2) {
		t.Errorf("query total: %v", out["total"])
	}
	docs := out["docs"].([]any)
	if len(docs) != 2 {
		t.Errorf("query docs: %v", docs)
	}

	code, out = te.do("POST", "/api/query", map[string]any{
		"coll": "orders", "filter": map[string]any{},
	}, true)
	if out["total"] != float64(3) {
		t.Errorf("query all total: %v", out["total"])
	}

	code, out = te.do("POST", "/api/query", map[string]any{
		"coll": "missing", "filter": map[string]any{},
	}, true)
	if code != 200 || out["total"] != float64(0) {
		t.Errorf("query missing coll should 200 with 0 total, got %d %v", code, out)
	}
}

func TestListUsersEmpty(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	code, out := te.do("GET", "/api/users", nil, false)
	if code != 200 {
		t.Fatalf("list users: %d", code)
	}
	b, _ := json.Marshal(out)
	if string(b) != `{"users":[{"roles":["admin"],"username":"admin"}]}` {
		t.Errorf("expected admin user, got %s", string(b))
	}
}
