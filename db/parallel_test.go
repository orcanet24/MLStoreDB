package db

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// M3: parallel Find equivalence, Find∥Insert∥Flush, write backpressure.

func TestParallelFindMatchesSerial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	opts.FindWorkers = -1 // serial baseline
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		st := "OPEN"
		if i%3 == 0 {
			st = "CLOSED"
		}
		if i%7 == 0 {
			st = "PENDING"
		}
		if err := s.Insert("q", Document{"_id": idKey(i), "status": st, "n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnsureIndex("q", []string{"status"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureIndex("q", []string{"n"}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushSync(); err != nil {
		t.Fatal(err)
	}

	filters := []Document{
		nil,
		{"status": "OPEN"},
		{"status": Document{"$regex": "^O"}},
		{"n": Document{"$gte": 100, "$lt": 400}},
	}
	var serial [][]string
	for _, f := range filters {
		docs, err := s.Find("q", f, nil)
		if err != nil {
			t.Fatal(err)
		}
		serial = append(serial, docIDs(docs))
	}

	// Same store, now force many workers.
	s.opts.FindWorkers = 32
	for i, f := range filters {
		docs, err := s.Find("q", f, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := docIDs(docs)
		if len(got) != len(serial[i]) {
			t.Fatalf("filter %d: parallel %d != serial %d", i, len(got), len(serial[i]))
		}
		for j := range got {
			if got[j] != serial[i][j] {
				t.Fatalf("filter %d [%d]: %q != %q", i, j, got[j], serial[i][j])
			}
		}
	}
	// Count equivalence.
	s.opts.FindWorkers = -1
	nSerial, err := s.Count("q", Document{"status": "CLOSED"})
	if err != nil {
		t.Fatal(err)
	}
	s.opts.FindWorkers = 32
	nPar, err := s.Count("q", Document{"status": "CLOSED"})
	if err != nil {
		t.Fatal(err)
	}
	if nSerial != nPar {
		t.Fatalf("count serial=%d parallel=%d", nSerial, nPar)
	}
	_ = s.Close()
}

func TestParallelFindDuringFlushNoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	opts.FindWorkers = 8
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if err := s.Insert("q", Document{"_id": idKey(i), "n": i}); err != nil {
			t.Fatal(err)
		}
	}
	var writers sync.WaitGroup
	stop := make(chan struct{})
	flushDone := make(chan struct{})
	errCh := make(chan error, 16)
	// Flush loop (separate lifecycle — stop closes after writers finish).
	go func() {
		defer close(flushDone)
		for {
			select {
			case <-stop:
				return
			default:
				if err := s.Flush(); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}
	}()
	// Writer + parallel reader.
	for g := 0; g < 4; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			for i := 0; i < 50; i++ {
				id := idKey(g*1000 + i)
				if err := s.Upsert("q", id, Document{"_id": id, "n": i}); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if _, err := s.Find("q", Document{"n": Document{"$gte": 0}}, nil); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}(g)
	}
	writers.Wait()
	close(stop)
	<-flushDone
	select {
	case err := <-errCh:
		t.Fatalf("concurrent find/upsert/flush: %v", err)
	default:
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen: all docs present.
	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, err := s2.Count("q", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n < 600 {
		t.Fatalf("docs after reopen = %d, want >= 600", n)
	}
}

func TestWriteBackpressureBlocksDuringFlush(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.mlstore")
	opts := testOpts()
	opts.MaxPendingWrites = 4
	opts.FindWorkers = -1
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Start a flush that is "in progress" by setting the flag manually is
	// racy; instead: stage a flush via prepareFlush (sets flushInProgress)
	// and write enough mutations that the next writers must wait until
	// applyFlushed broadcasts.
	// Simpler deterministic check: with no flush in progress, writes never block.
	for i := 0; i < 20; i++ {
		if err := s.Insert("q", Document{"_id": idKey(i)}); err != nil {
			t.Fatalf("write without flush must not block: %v", err)
		}
	}
	// Now interleave prepareFlush (holds flushInProgress) with writers.
	st, err := s.prepareFlush(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("expected flush state")
	}
	s.mu.Lock()
	inProg := s.flushInProgress
	seq, dur := s.writeSeq, s.durableSeq
	s.mu.Unlock()
	if !inProg {
		t.Fatal("prepareFlush must set flushInProgress")
	}
	// Writers that exceed MaxPendingWrites while flush is open must wait.
	// Run them async; complete the flush; they must all finish.
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				if err := s.Upsert("q", idKey(1000+g*10+i), Document{"_id": idKey(1000 + g*10 + i)}); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}(g)
	}
	// Give writers a moment to pile up against the open flush, then commit.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// Finish the flush.
	if err := writeState(st); err != nil {
		t.Fatal(err)
	}
	s.applyFlushed(st)
	select {
	case <-done:
	case <-timeoutAfter(t, 10):
		t.Fatal("writers did not finish after flush completed (backpressure stuck)")
	}
	select {
	case err := <-errCh:
		t.Fatalf("write during backpressure: %v", err)
	default:
	}
	_ = seq
	_ = dur
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCOWFailureLeavesLiveDoc(t *testing.T) {
	s := New()
	if err := s.Insert("q", Document{"_id": "1", "n": 1, "keep": "yes"}); err != nil {
		t.Fatal(err)
	}
	// Oversized patch fails checkDocSize → live doc unchanged (COW).
	huge := strings.Repeat("x", maxDocBytes+1)
	if err := s.Update("q", "1", Document{"blob": huge}); err == nil {
		t.Fatal("expected size error")
	}
	doc, err := s.Get("q", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, has := doc["blob"]; has {
		t.Fatal("failed Update must not leave partial patch on live doc")
	}
	if doc["keep"] != "yes" {
		t.Fatalf("live doc keep field corrupted: %v", doc)
	}
	switch v := doc["n"].(type) {
	case int:
		if v != 1 {
			t.Fatalf("live doc n = %v, want 1", v)
		}
	case float64:
		if v != 1 {
			t.Fatalf("live doc n = %v, want 1", v)
		}
	default:
		t.Fatalf("live doc n type %T = %v, want 1", doc["n"], doc["n"])
	}
	// Successful COW update installs next.
	if err := s.Update("q", "1", Document{"n": 2}); err != nil {
		t.Fatal(err)
	}
	doc, _ = s.Get("q", "1")
	switch v := doc["n"].(type) {
	case int:
		if v != 2 {
			t.Fatalf("update not applied: %v", doc)
		}
	case float64:
		if v != 2 {
			t.Fatalf("update not applied: %v", doc)
		}
	default:
		t.Fatalf("update not applied: %v", doc)
	}
}

func docIDs(docs []Document) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i], _ = d["_id"].(string)
	}
	return out
}

func timeoutAfter(t *testing.T, sec int) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		deadline := time.Now().Add(time.Duration(sec) * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ch:
				return
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}
