package db

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHookBeforeVeto(t *testing.T) {
	s := New()
	veto := errors.New("veto: no inserts")
	if _, err := s.On(BeforeInsert, "q", func(h *HookContext) error {
		return veto
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1"}); !errors.Is(err, veto) {
		t.Errorf("Insert = %v, want veto", err)
	}
	if _, err := s.Get("q", "1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("doc must not exist after veto: %v", err)
	}
}

func TestHookBeforeCanModifyDoc(t *testing.T) {
	s := New()
	if _, err := s.On(BeforeInsert, "q", func(h *HookContext) error {
		h.Doc["stamped"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Get("q", "1")
	if err != nil {
		t.Fatal(err)
	}
	if doc["stamped"] != true {
		t.Errorf("before hook mutation lost: %v", doc)
	}
}

func TestHookAfterSyncAndMatchAll(t *testing.T) {
	s := New()
	var got atomic.Value
	if _, err := s.On(AfterInsert, "", func(h *HookContext) error {
		got.Store(h.Collection + ":" + h.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("alpha", Document{"_id": "a1"}); err != nil {
		t.Fatal(err)
	}
	if got.Load() != "alpha:a1" {
		t.Errorf("after hook got %v", got.Load())
	}
	// "*" pattern too
	s2 := New()
	var hit atomic.Bool
	if _, err := s2.On(AfterInsert, "*", func(h *HookContext) error {
		hit.Store(true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = s2.Insert("beta", Document{"_id": "b"})
	if !hit.Load() {
		t.Error("* pattern did not fire")
	}
}

func TestHookOff(t *testing.T) {
	s := New()
	var n atomic.Int64
	id, err := s.On(AfterInsert, "q", func(h *HookContext) error {
		n.Add(1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1"})
	if n.Load() != 1 {
		t.Fatalf("n = %d", n.Load())
	}
	if !s.Off(id) {
		t.Error("Off = false")
	}
	if s.Off(id) {
		t.Error("double Off = true")
	}
	_ = s.Insert("q", Document{"_id": "2"})
	if n.Load() != 1 {
		t.Errorf("hook fired after Off: n=%d", n.Load())
	}
}

func TestHookUpdateDeleteEvents(t *testing.T) {
	s := New()
	if err := s.Insert("q", Document{"_id": "1", "v": 1}); err != nil {
		t.Fatal(err)
	}
	var events []string
	var mu sync.Mutex
	add := func(tag string) HookFunc {
		return func(h *HookContext) error {
			mu.Lock()
			events = append(events, tag)
			mu.Unlock()
			return nil
		}
	}
	if _, err := s.On(BeforeUpdate, "q", add("bu")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.On(AfterUpdate, "q", func(h *HookContext) error {
		if h.Old == nil || h.Doc == nil {
			t.Errorf("after_update payload: old=%v doc=%v", h.Old, h.Doc)
		}
		mu.Lock()
		events = append(events, "au")
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.On(BeforeDelete, "q", func(h *HookContext) error {
		if h.Old == nil {
			t.Error("before_delete missing Old")
		}
		mu.Lock()
		events = append(events, "bd")
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.On(AfterDelete, "q", add("ad")); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("q", "1", Document{"v": 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("q", "1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string{}, events...)
	mu.Unlock()
	want := []string{"bu", "au", "bd", "ad"}
	if len(got) != len(want) {
		t.Fatalf("events = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("events = %v, want %v", got, want)
		}
	}
}

func TestHookNestedMutationAndDepth(t *testing.T) {
	s := New()
	// chain: insert into c0 → hook inserts into c1 → ... each level +1 depth
	for i := 0; i < maxHookDepth+2; i++ {
		src := "c" + string(rune('a'+i))
		dst := "c" + string(rune('a'+i+1))
		if _, err := s.On(AfterInsert, src, func(h *HookContext) error {
			return h.Insert(dst, Document{"_id": h.ID})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// root insert: depth 1 (c0 hook) then nested... exceeds maxHookDepth → ErrHookDepth
	// After-hooks ignore errors, so the chain stops silently at the limit.
	if err := s.Insert("ca", Document{"_id": "x"}); err != nil {
		t.Fatal(err)
	}
	// deepest level that should exist: maxHookDepth nested inserts succeeded
	// ca (root) → cb(1) → cc(2) → ... up to depth maxHookDepth
 deepest := "c" + string(rune('a'+maxHookDepth))
	if _, err := s.Get(deepest, "x"); err != nil {
		t.Errorf("expected doc at %s (depth limit): %v", deepest, err)
	}
	beyond := "c" + string(rune('a'+maxHookDepth+1))
	if _, err := s.Get(beyond, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("depth limit not enforced: %s exists (%v)", beyond, err)
	}
}

func TestHookCycleDetectionBefore(t *testing.T) {
	s := New()
	// before_insert on "a" inserts into "a" again → same hook re-entered → cycle
	if _, err := s.On(BeforeInsert, "a", func(h *HookContext) error {
		return h.Insert("a", Document{"_id": "nested"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("a", Document{"_id": "root"}); !errors.Is(err, ErrHookCycle) {
		t.Errorf("Insert = %v, want ErrHookCycle", err)
	}
	if _, err := s.Get("a", "root"); !errors.Is(err, ErrNotFound) {
		t.Errorf("root must be vetoed by cycle: %v", err)
	}
}

func TestHookDepthExceededBefore(t *testing.T) {
	s := New()
	// distinct hooks chained a→b→c... : depth exceeded (not cycle)
	for i := 0; i < maxHookDepth+2; i++ {
		src := "c" + string(rune('a'+i))
		dst := "c" + string(rune('a'+i+1))
		if _, err := s.On(BeforeInsert, src, func(h *HookContext) error {
			return h.Insert(dst, Document{"_id": "x"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := s.Insert("ca", Document{"_id": "x"})
	if !errors.Is(err, ErrHookDepth) {
		t.Errorf("Insert = %v, want ErrHookDepth", err)
	}
	// root was vetoed at depth exceeded inside the chain → nothing persisted at root
	if _, err := s.Get("ca", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("root must not exist: %v", err)
	}
}

func TestHookAsyncFIFO(t *testing.T) {
	s := New()
	var mu sync.Mutex
	var order []string
	if _, err := s.OnAsync(AfterInsert, "q", func(h *HookContext) error {
		mu.Lock()
		order = append(order, h.ID)
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := s.Insert("q", Document{"_id": string(rune('a' + i%26)) + string(rune('0'+i/26))}); err != nil {
			t.Fatal(err)
		}
	}
	s.WaitAsyncHooks()
	mu.Lock()
	n := len(order)
	mu.Unlock()
	if n != 50 {
		t.Errorf("async ran %d/50", n)
	}
	// FIFO: queue was filled in insert order; single worker preserves it
	mu.Lock()
	for i := 1; i < len(order); i++ {
		if order[i] != string(rune('a'+i%26))+string(rune('0'+i/26)) {
			t.Errorf("order broken at %d: %v", i, order)
			break
		}
	}
	mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHookAsyncRejectsBefore(t *testing.T) {
	s := New()
	if _, err := s.OnAsync(BeforeInsert, "q", func(h *HookContext) error { return nil }); err == nil {
		t.Error("OnAsync(before_*) must fail")
	}
	if _, err := s.On("nope", "q", func(h *HookContext) error { return nil }); err == nil {
		t.Error("On(unknown event) must fail")
	}
	if _, err := s.On(AfterInsert, "q", nil); err == nil {
		t.Error("On(nil) must fail")
	}
}

func TestHookRBACBypass(t *testing.T) {
	s := New()
	s.opts.LightKDF = true
	if err := s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("root", "secret-admin-1", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.Authenticate("root", "secret-admin-1")
	if err != nil {
		t.Fatal(err)
	}
	var auditMsg atomic.Value // string: "" = ok
	auditMsg.Store("")
	if _, err := s.On(AfterInsert, "q", func(h *HookContext) error {
		// hook is trusted: bypasses RBAC, writes audit even though
		// raw Store API would be ErrUnauthorized
		if err := h.Insert("audit", Document{"_id": h.ID}); err != nil {
			auditMsg.Store(err.Error())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	if msg := auditMsg.Load().(string); msg != "" {
		t.Errorf("hook Insert audit = %s", msg)
	}
	// audit exists (hook bypassed auth); raw Store still denied
	if _, err := s.Get("audit", "1"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("raw Get audit = %v", err)
	}
	// but hook cannot write system colls
	var sysMsg atomic.Value
	sysMsg.Store("")
	if _, err := s.On(AfterInsert, "q2", func(h *HookContext) error {
		if err := h.Insert("_users", Document{"_id": "evil"}); err != nil {
			sysMsg.Store(err.Error())
		} else {
			sysMsg.Store("no-error")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = sess.Insert("q2", Document{"_id": "1"})
	if msg := sysMsg.Load().(string); msg != ErrForbidden.Error() {
		t.Errorf("hook system coll = %q, want %q", msg, ErrForbidden)
	}
}

func TestHookNoRegistrationOverheadPath(t *testing.T) {
	// sanity: mutations work identically with zero hooks registered
	s := New()
	for i := 0; i < 10; i++ {
		if err := s.Insert("q", Document{"_id": string(rune('0' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := s.Count("q", nil)
	if n != 10 {
		t.Errorf("count = %d", n)
	}
}

func TestHookAsyncBackpressureBounded(t *testing.T) {
	s := New()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if _, err := s.OnAsync(AfterInsert, "q", func(h *HookContext) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// first job blocks the worker
	if err := s.Insert("q", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	<-started
	// fill queue beyond capacity without deadlocking the test:
	// queue bound = hookQueueSize; insert hookQueueSize more (fills buffer),
	// the next enqueue would block — we don't issue it (that's the contract).
	// Instead verify WaitAsyncHooks still drains after release.
	for i := 0; i < 5; i++ {
		if err := s.Insert("q", Document{"_id": string(rune('2' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	s.WaitAsyncHooks()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// ensure worker actually stopped
	time.Sleep(10 * time.Millisecond)
}
