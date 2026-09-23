package db

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDocSizeLimit1MB(t *testing.T) {
	s := New()
	big := make([]byte, maxDocBytes+10)
	for i := range big {
		big[i] = 'x'
	}
	err := s.Insert("q", Document{"_id": "1", "blob": string(big)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// just under limit ok
	ok := make([]byte, maxDocBytes-100)
	for i := range ok {
		ok[i] = 'y'
	}
	if err := s.Insert("q", Document{"_id": "2", "blob": string(ok)}); err != nil {
		t.Fatalf("under limit: %v", err)
	}
}

func TestUpsertSizeLimit(t *testing.T) {
	s := New()
	big := make([]byte, maxDocBytes+10)
	for i := range big {
		big[i] = 'x'
	}
	if err := s.Upsert("q", "1", Document{"blob": string(big)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestUpdateSizeLimitRollback(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "a": 1})
	big := make([]byte, maxDocBytes+10)
	for i := range big {
		big[i] = 'x'
	}
	if err := s.Update("q", "1", Document{"blob": string(big)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	d, _ := s.Get("q", "1")
	if _, bad := d["blob"]; bad {
		t.Error("rollback failed")
	}
	if d["a"] != 1 {
		t.Errorf("doc: %v", d)
	}
}

// realistic-ish ml_orders payload for benchmarks
func benchDoc(i int) Document {
	return Document{
		"_id":        fmt.Sprintf("%d", i),
		"order_id":   fmt.Sprintf("%d", 1000000+i),
		"status":     "paid",
		"account_id": fmt.Sprintf("acc-%d", i%10),
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"buyer_id":   fmt.Sprintf("buyer-%d", i%1000),
		"total":      float64(i) * 1.5,
		"payload": Document{
			"item_id":     fmt.Sprintf("MLV%d", i),
			"title":       "Producto de prueba con descripción larga para tamaño realista",
			"quantity":    1,
			"unit_price":  999.99,
			"shipping":    Document{"mode": "me2", "status": "ready"},
			"payments":    []any{Document{"method": "credit_card", "status": "approved"}},
			"annotations": []any{"a", "b", "c"},
		},
	}
}

func BenchmarkInsert10k(b *testing.B) {
	s := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Insert("ml_orders", benchDoc(i))
	}
}

func BenchmarkFindFullScan10k(b *testing.B) {
	s := New()
	for i := 0; i < 10000; i++ {
		_ = s.Insert("ml_orders", benchDoc(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Find("ml_orders", Document{"buyer_id": "buyer-42"}, nil)
	}
}

func BenchmarkFindIndexed10k(b *testing.B) {
	s := New()
	for i := 0; i < 10000; i++ {
		_ = s.Insert("ml_orders", benchDoc(i))
	}
	_ = s.EnsureIndex("ml_orders", []string{"account_id"}, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Find("ml_orders", Document{"account_id": "acc-3"}, nil)
	}
}

func BenchmarkFlush10k(b *testing.B) {
	dir := b.TempDir()
	opts := Options{
		MasterKey: []byte("bench-master-key-32-bytes-long!!!!"),
		MachineID: "bench",
		LightKDF:  true,
	}
	path := fmt.Sprintf("%s/bench.mlstore", dir)
	s, err := Open(path, opts)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		_ = s.Insert("ml_orders", benchDoc(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.mu.Lock()
		s.dirty = true
		s.dirtyGen++
		s.mu.Unlock()
		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_ = s.Close()
}

func BenchmarkExportCSV10k(b *testing.B) {
	s := New()
	for i := 0; i < 10000; i++ {
		_ = s.Insert("ml_orders", benchDoc(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := s.ExportCSV("ml_orders", &buf); err != nil {
			b.Fatal(err)
		}
	}
}

// complexDoc is a deep/nested ML-like payload (~real order with payments,
// shipping, buyer profile, annotations arrays — multi-level maps/slices).
func complexDoc(i int) Document {
	payments := make([]any, 0, 3)
	for p := 0; p < 3; p++ {
		payments = append(payments, Document{
			"id":           fmt.Sprintf("pay-%d-%d", i, p),
			"method":       []string{"credit_card", "bank_transfer", "cash"}[p%3],
			"status":       "approved",
			"amount":       float64(100 + p*37),
			"installments": float64(p + 1),
			"transaction":  Document{"id": fmt.Sprintf("tx-%d-%d", i, p), "external": false},
			"payer":        Document{"id": fmt.Sprintf("buyer-%d", i%1000), "email": fmt.Sprintf("b%d@x.com", i%1000)},
		})
	}
	items := make([]any, 0, 4)
	for it := 0; it < 4; it++ {
		items = append(items, Document{
			"item_id":     fmt.Sprintf("MLV%d-%d", i, it),
			"title":       "Zapatillas Urban Run Pro talle 42 azul marino edición limitada",
			"quantity":    float64(1 + it%2),
			"unit_price":  999.99,
			"category_id": "MLA1051",
			"attributes": []any{
				Document{"id": float64(1), "name": "Marca", "value_name": "Nike"},
				Document{"id": float64(2), "name": "Talle", "value_name": "42"},
			},
			"seller": Document{"id": float64(12345), "nickname": "TIENDA_OFICIAL"},
		})
	}
	return Document{
		"_id":        fmt.Sprintf("%d", i),
		"order_id":   fmt.Sprintf("%d", 1000000+i),
		"status":     "paid",
		"account_id": fmt.Sprintf("acc-%d", i%10),
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"buyer_id":   fmt.Sprintf("buyer-%d", i%1000),
		"total":      float64(i) * 1.5,
		"buyer": Document{
			"id":       fmt.Sprintf("buyer-%d", i%1000),
			"nickname": fmt.Sprintf("nick_%d", i%1000),
			"email":    fmt.Sprintf("b%d@x.com", i%1000),
			"addresses": []any{
				Document{"id": float64(1), "city": "Córdoba", "zip": "5000",
					"geo": Document{"lat": -31.4201, "lon": -64.1888}},
				Document{"id": float64(2), "city": "BA", "zip": "1000",
					"geo": Document{"lat": -34.6037, "lon": -58.3816}},
			},
		},
		"payload": Document{
			"items":    items,
			"payments": payments,
			"shipping": Document{"mode": "me2", "status": "ready", "tracking": "AR123456789"},
			"feedback": Document{"rating": float64(5), "message": "todo bien"},
			"note":     "cliente frecuente — verificar dirección antes de despachar",
			"tags":     []any{"priority", "fragile", "gift"},
			"flippers": map[string]any{"a": true, "b": false, "c": float64(3)},
		},
	}
}

func seedComplex(b *testing.B, n int) *Store {
	b.Helper()
	s := New()
	for i := 0; i < n; i++ {
		if err := s.Insert("ml_orders", complexDoc(i)); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

// Insert of deeply nested JSON (~2–4 KB/doc).
func BenchmarkInsertComplex(b *testing.B) {
	s := New()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Insert("ml_orders", complexDoc(i)); err != nil {
			b.Fatal(err)
		}
	}
}

// Get by _id: full deep-clone of complex JSON out of the store.
func BenchmarkGetComplexJSON(b *testing.B) {
	const n = 10000
	s := seedComplex(b, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get("ml_orders", fmt.Sprintf("%d", i%n)); err != nil {
			b.Fatal(err)
		}
	}
}

// Find full page of complex docs (e.g. 20 orders by account) — clone page out.
func BenchmarkFindComplexPage20(b *testing.B) {
	const n = 10000
	s := seedComplex(b, n)
	_ = s.EnsureIndex("ml_orders", []string{"account_id"}, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		docs, err := s.Find("ml_orders", Document{"account_id": "acc-3"},
			&FindOptions{Limit: 20, Sort: map[string]int{"created_at": -1}})
		if err != nil {
			b.Fatal(err)
		}
		if len(docs) == 0 {
			b.Fatal("empty page")
		}
	}
}

// Projection: only top-level fields (payload left behind — cheaper path).
func BenchmarkFindComplexProjection(b *testing.B) {
	const n = 10000
	s := seedComplex(b, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		docs, err := s.Find("ml_orders", nil, &FindOptions{
			Projection: []string{"order_id", "status", "total"},
			Limit:      50,
		})
		if err != nil || len(docs) != 50 {
			b.Fatalf("proj: %d %v", len(docs), err)
		}
	}
}

// Count of nested path filter (deep walk without returning docs).
func BenchmarkCountComplexNested(b *testing.B) {
	const n = 5000
	s := seedComplex(b, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := s.Count("ml_orders", Document{"payload.shipping.mode": "me2"})
		if err != nil || got != n {
			b.Fatalf("count=%d err=%v", got, err)
		}
	}
}

// Reopen path: decrypt + JSON unmarshal + rebuild indexes of complex docs.
func BenchmarkReopenComplex10k(b *testing.B) {
	dir := b.TempDir()
	opts := Options{
		MasterKey: []byte("bench-master-key-32-bytes-long!!!!"),
		MachineID: "bench",
		LightKDF:  true,
	}
	path := fmt.Sprintf("%s/complex.mlstore", dir)
	// Build file once outside the timed loop.
	{
		s, err := Open(path, opts)
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < 10000; i++ {
			if err := s.Insert("ml_orders", complexDoc(i)); err != nil {
				b.Fatal(err)
			}
		}
		_ = s.EnsureIndex("ml_orders", []string{"account_id"}, false)
		_ = s.EnsureIndex("ml_orders", []string{"order_id"}, true)
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if i > 0 {
			// keep file; Open is read-only until mutation
		}
		b.StartTimer()
		s, err := Open(path, opts)
		if err != nil {
			b.Fatal(err)
		}
		// pull a complex doc to prove full nested structure recovered
		if _, err := s.Get("ml_orders", "42"); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = s.Close()
		b.StartTimer()
	}
}
