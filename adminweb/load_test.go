package adminweb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func generateLargeJSON(n int) string {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"_id":"doc-%d","name":"User %d","email":"user%d@example.com","score":%d,"active":%t}`,
			i, i, i, i%100, i%2 == 0)
	}
	sb.WriteString("]")
	return sb.String()
}

func TestImportPipelineLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skip load test in short mode")
	}
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "big"}, true)

	const n = 36000
	data := generateLargeJSON(n)

	start := time.Now()
	url := fmt.Sprintf("http://test/api/collections/big/import?format=json&mode=insert")
	req, _ := http.NewRequest("POST", url, strings.NewReader(data))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", te.csrf)
	res, err := te.client.Do(req)
	if err != nil {
		te.t.Fatalf("post import: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	code := res.StatusCode
	if code != 202 {
		t.Fatalf("import: %d %v", code, out)
	}
	jobID, _ := out["jobId"].(string)

	deadline := time.Now().Add(120 * time.Second)
	for {
		code, st := te.do("GET", "/api/import/"+jobID, nil, false)
		if code != 200 {
			t.Fatalf("status: %d", code)
		}
		if st["status"] == "done" || st["status"] == "error" {
			took := time.Since(start)
			t.Logf("imported %d/%d in %s (%.0f docs/sec)", st["inserted"], st["total"], took, float64(st["inserted"].(float64))/took.Seconds())
			if st["status"] == "error" {
				t.Fatalf("import error: %v", st)
			}
			if st["inserted"] != float64(n) {
				t.Fatalf("expected %d inserted, got %v", n, st["inserted"])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: processed %v/%v", st["processed"], st["total"])
		}
		time.Sleep(200 * time.Millisecond)
	}

	code, out = te.do("GET", "/api/collections/big/docs?limit=5", nil, false)
	if code != 200 || out["total"] != float64(n) {
		t.Fatalf("verify count: %d %v", code, out)
	}

	var count int
	if c, ok := out["total"].(float64); ok {
		count = int(c)
	}
	docs, _ := out["docs"].([]any)
	t.Logf("collection has %d docs, first page shows %d", count, len(docs))
}

func TestImportBrowsePagination(t *testing.T) {
	te := newTestEnv(t)
	te.setupAndLogin()
	te.do("POST", "/api/collections", map[string]string{"name": "paged"}, true)
	for i := 0; i < 250; i++ {
		te.do("POST", "/api/collections/paged/docs", map[string]any{"_id": fmt.Sprintf("d%d", i), "v": i}, true)
	}

	code, out := te.do("GET", "/api/collections/paged/docs?limit=25&skip=0", nil, false)
	if code != 200 {
		t.Fatalf("page1: %d", code)
	}
	docs, _ := out["docs"].([]any)
	if len(docs) != 25 {
		t.Errorf("page size should be 25, got %d", len(docs))
	}
	if out["total"] != float64(250) {
		t.Errorf("total should be 250, got %v", out["total"])
	}

	code, out = te.do("GET", "/api/collections/paged/docs?limit=25&skip=225", nil, false)
	docs, _ = out["docs"].([]any)
	if len(docs) != 25 {
		t.Errorf("last page should be 25, got %d", len(docs))
	}
}
