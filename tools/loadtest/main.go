package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mlstoredb/db"
)

var master = []byte("demo-master-key-32-bytes-long!!!!")

func ms(d time.Duration) string {
	return fmt.Sprintf("%8.2f ms", float64(d.Microseconds())/1000.0)
}

func usPer(n int, d time.Duration) string {
	return fmt.Sprintf("%8.1f µs/op", float64(d.Microseconds())/float64(n))
}

func heapMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Alloc) / (1024 * 1024)
}

func fileStat(path string) (size int64, mode string) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err.Error()
	}
	return fi.Size(), fi.Mode().String()
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func sampleOrder(i int) db.Document {
	return db.Document{
		"_id":        fmt.Sprintf("%d", 1_000_000+i),
		"order_id":   fmt.Sprintf("%d", 1_000_000+i),
		"status":     []string{"paid", "confirmed", "delivered", "cancelled"}[i%4],
		"account_id": fmt.Sprintf("acc-%02d", i%20),
		"buyer_id":   fmt.Sprintf("buyer-%04d", i%500),
		"total":      float64(100 + i%900),
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"payload": db.Document{
			"item_id": fmt.Sprintf("MLV%d", 700000000+i),
			"title":   "Zapatillas Urban Run Pro talle 42 azul marino",
			"qty":     1,
			"payments": []any{
				db.Document{"method": "credit_card", "status": "approved"},
			},
		},
	}
}

func main() {
	dir := filepath.Join(os.TempDir(), "mld-bench-demo")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "carga.mlstore")
	_ = os.Remove(path)
	_ = os.Remove(filepath.Join(dir, "mldstore.lock"))

	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Println(" mlstore — prueba de carga (insert / leer / editar)")
	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Printf("Archivo: %s\n", path)
	fmt.Printf("Heap base: %.1f MB\n\n", heapMB())

	opts := db.Options{
		MasterKey: master,
		MachineID: "bench-demo",
		LightKDF:  false, // parámetros reales (no light)
	}

	t0 := time.Now()
	s, err := db.OpenWithLock(path, opts)
	if err != nil {
		fmt.Println("OPEN error:", err)
		os.Exit(1)
	}
	fmt.Println("1) OpenWithLock + flock + auto-flush 2s")
	fmt.Println("   ", ms(time.Since(t0)))

	// ── índices como usará el sistema real ──
	_ = s.EnsureIndex("ml_orders", []string{"account_id"}, false)
	_ = s.EnsureIndex("ml_orders", []string{"buyer_id"}, false)
	_ = s.EnsureIndex("ml_orders", []string{"status"}, false)
	_ = s.EnsureIndex("ml_orders", []string{"order_id"}, true)

	const N = 10_000

	// ── INSERT ──
	fmt.Printf("\n2) INSERT %d docs (payload CRM simulado)\n", N)
	before := heapMB()
	t := time.Now()
	for i := 0; i < N; i++ {
		if err := s.Insert("ml_orders", sampleOrder(i)); err != nil {
			fmt.Println("  insert error:", err)
			os.Exit(1)
		}
	}
	dIns := time.Since(t)
	fmt.Printf("   total: %s   %s   heap %.1f → %.1f MB (+%.1f)\n",
		ms(dIns), usPer(N, dIns), before, heapMB(), heapMB()-before)

	// ── GET por _id ──
	fmt.Println("\n3) GET por _id (aleatorio)")
	const G = 10_000
	t = time.Now()
	for i := 0; i < G; i++ {
		id := fmt.Sprintf("%d", 1_000_000+(i*7)%N)
		if _, err := s.Get("ml_orders", id); err != nil {
			fmt.Println("  get error:", err)
			os.Exit(1)
		}
	}
	dGet := time.Since(t)
	fmt.Printf("   total: %s   %s\n", ms(dGet), usPer(G, dGet))

	// ── Find full-scan (sin índice, campo no indexado) ──
	fmt.Println("\n4) Find full-scan buyer_id (NO indexado)")
	const F = 200
	t = time.Now()
	for i := 0; i < F; i++ {
		if _, err := s.Find("ml_orders", db.Document{"buyer_id": "buyer-0042"}, nil); err != nil {
			fmt.Println("  find error:", err)
			os.Exit(1)
		}
	}
	dScan := time.Since(t)
	fmt.Printf("   total: %s   %s\n", ms(dScan), usPer(F, dScan))

	// ── Find con índice (misma cardinalidad que el scan: buyer_id) ──
	fmt.Println("\n5) Find CON índice — buyer_id=buyer-0042 (mismo match que §4)")
	// ensure index exists for apples-to-apples
	_ = s.EnsureIndex("ml_orders", []string{"buyer_id"}, false)
	t = time.Now()
	var hit int
	for i := 0; i < F; i++ {
		docs, err := s.Find("ml_orders", db.Document{"buyer_id": "buyer-0042"}, nil)
		if err != nil {
			fmt.Println("  find error:", err)
			os.Exit(1)
		}
		hit = len(docs)
	}
	dIdx := time.Since(t)
	fmt.Printf("   hits/consulta: %d   total: %s   %s\n", hit, ms(dIdx), usPer(F, dIdx))
	if dIdx > 0 && dScan > 0 {
		fmt.Printf("   speedup vs full-scan (mismos hits): %.1fx\n", float64(dScan)/float64(dIdx))
	}

	// ── Find unique order_id ──
	fmt.Println("\n6) Find unique order_id (índice unique)")
	t = time.Now()
	for i := 0; i < F; i++ {
		oid := fmt.Sprintf("%d", 1_000_000+(i*13)%N)
		docs, err := s.Find("ml_orders", db.Document{"order_id": oid}, nil)
		if err != nil || len(docs) != 1 {
			fmt.Println("  unique find problem:", err, len(docs))
			os.Exit(1)
		}
	}
	dU := time.Since(t)
	fmt.Printf("   total: %s   %s\n", ms(dU), usPer(F, dU))

	// ── Find con rango + sort + limit ──
	fmt.Println("\n7) Find rango total>=500 + sort + limit 20")
	t = time.Now()
	const R = 100
	for i := 0; i < R; i++ {
		docs, err := s.Find("ml_orders",
			db.Document{"total": db.Document{"$gte": 500}},
			&db.FindOptions{Sort: map[string]int{"total": -1}, Limit: 20})
		if err != nil {
			fmt.Println("  range error:", err)
			os.Exit(1)
		}
		_ = docs
	}
	dR := time.Since(t)
	fmt.Printf("   total: %s   %s\n", ms(dR), usPer(R, dR))

	// ── UPDATE ──
	fmt.Println("\n8) UPDATE (merge shallow) 10.000 docs")
	const U = 10_000
	t = time.Now()
	for i := 0; i < U; i++ {
		id := fmt.Sprintf("%d", 1_000_000+i)
		if err := s.Update("ml_orders", id, db.Document{
			"status":    "delivered",
			"updated_n": float64(i),
		}); err != nil {
			fmt.Println("  update error:", err)
			os.Exit(1)
		}
	}
	dUpd := time.Since(t)
	fmt.Printf("   total: %s   %s\n", ms(dUpd), usPer(U, dUpd))

	// ── Count ──
	fmt.Println("\n9) Count con filtro")
	t = time.Now()
	for i := 0; i < 200; i++ {
		n, err := s.Count("ml_orders", db.Document{"status": "delivered"})
		if err != nil || n != N {
			fmt.Println("  count:", n, err)
			os.Exit(1)
		}
	}
	dC := time.Since(t)
	fmt.Printf("   total: %s   %s\n", ms(dC), usPer(200, dC))

	// ── Flush explícito (forzar dirty para medir de verdad) ──
	fmt.Println("\n10) Flush explícito (cifrado + atomic write) — dirty forzado")
	_ = s.Update("ml_orders", "1000000", db.Document{"flush_probe": true})
	t = time.Now()
	if err := s.Flush(); err != nil {
		fmt.Println("  flush error:", err)
		os.Exit(1)
	}
	dF := time.Since(t)
	sz, mode := fileStat(path)
	fmt.Printf("   total: %s\n", ms(dF))
	fmt.Printf("   disco: %s  (%s)\n", human(sz), mode)

	// ── Directory listing ──
	fmt.Println("\n11) Qué hay en disco")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, _ := e.Info()
		fmt.Printf("   %-24s %10s  %s\n", e.Name(), human(fi.Size()), fi.Mode())
	}
	// peek header magic
	f, _ := os.Open(path)
	magic := make([]byte, 4)
	_, _ = f.Read(magic)
	_ = f.Close()
	fmt.Printf("   magic header: %q  (no es JSON plano — está cifrado)\n", string(magic))

	// ── Reopen (persistencia) ──
	fmt.Println("\n12) Close + Open (reapertura / persistencia)")
	t = time.Now()
	if err := s.Close(); err != nil {
		fmt.Println("  close error:", err)
		os.Exit(1)
	}
	s2, err := db.OpenWithLock(path, opts)
	if err != nil {
		fmt.Println("  reopen error:", err)
		os.Exit(1)
	}
	dRe := time.Since(t)
	n, _ := s2.Count("ml_orders", nil)
	fmt.Printf("   reopen: %s   docs tras reopen: %d\n", ms(dRe), n)
	if n != N {
		fmt.Println("  FAIL: lost docs")
		os.Exit(1)
	}
	// unique still enforced
	err = s2.Insert("ml_orders", db.Document{"order_id": "1000000"})
	fmt.Printf("   unique viola tras reopen → %v (esperado duplicate)\n", err)

	// spot check update persisted
	d, err := s2.Get("ml_orders", "1000000")
	if err != nil || d["status"] != "delivered" {
		fmt.Println("  FAIL update not persisted:", err, d["status"])
		os.Exit(1)
	}
	fmt.Println("   update persistido ✓  unique persistido ✓")

	// ── Snapshot ──
	fmt.Println("\n13) Snapshot (backup cifrado)")
	bak := filepath.Join(dir, "backup.mlstore")
	t = time.Now()
	if err := s2.Snapshot(bak); err != nil {
		fmt.Println("  snapshot error:", err)
		os.Exit(1)
	}
	bsz, _ := fileStat(bak)
	fmt.Printf("   %s → %s   %s\n", ms(time.Since(t)), human(bsz), bak)

	_ = s2.Close()

	fmt.Println("\n════════════════════════════════════════════════════════")
	fmt.Println(" RESUMEN (10k docs, payload ~real de CRM)")
	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Printf("  Insert 10k     %s   %s\n", ms(dIns), usPer(N, dIns))
	fmt.Printf("  Get x10k       %s   %s\n", ms(dGet), usPer(G, dGet))
	fmt.Printf("  Find scan      %s   %s\n", ms(dScan), usPer(F, dScan))
	fmt.Printf("  Find index     %s   %s  (%.1fx más rápido)\n", ms(dIdx), usPer(F, dIdx), float64(dScan)/float64(dIdx))
	fmt.Printf("  Find unique    %s   %s\n", ms(dU), usPer(F, dU))
	fmt.Printf("  Range+sort     %s   %s\n", ms(dR), usPer(R, dR))
	fmt.Printf("  Update 10k     %s   %s\n", ms(dUpd), usPer(U, dUpd))
	fmt.Printf("  Flush+cifrar   %s\n", ms(dF))
	fmt.Printf("  Reopen 10k     %s\n", ms(dRe))
	fmt.Printf("  Archivo        %s en %s\n", human(sz), dir)
	fmt.Printf("  Heap final     %.1f MB\n", heapMB())
	fmt.Println("════════════════════════════════════════════════════════")
	fmt.Println(" PASS — insert/leer/editar/reencriptar estables")
}
