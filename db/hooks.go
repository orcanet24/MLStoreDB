package db

import (
	"fmt"
)

// Hook events (M5a). String ids so M5b JSON triggers can reuse them.
const (
	BeforeInsert = "before_insert"
	AfterInsert  = "after_insert"
	BeforeUpdate = "before_update"
	AfterUpdate  = "after_update"
	BeforeDelete = "before_delete"
	AfterDelete  = "after_delete"
	BeforeUpsert = "before_upsert"
	AfterUpsert  = "after_upsert"
)

const (
	// maxHookDepth bounds nested hook-triggered mutations (M5a).
	maxHookDepth = 8
	// hookQueueSize is the bounded FIFO for OnAsync jobs.
	hookQueueSize = 1024
)

// HookFunc runs for a matched event. Before-* hooks run synchronously and
// may veto the mutation by returning an error. After-* hooks run after the
// mutation is applied (On: inline, error ignored; OnAsync: queued).
// Nested mutations MUST go through HookContext methods (depth/cycle guards).
type HookFunc func(h *HookContext) error

// HookContext is passed to a hook: event payload + scoped Store access.
type HookContext struct {
	Event      string
	Collection string
	ID         string
	Doc        Document // new doc / patch (insert/update/upsert); nil for delete
	Old        Document // pre-image (update/delete/upsert); nil for insert

	frame *hookFrame
	store *Store
}

// Get/Find/Count are read helpers (run as trusted hook principal).
func (h *HookContext) Get(coll string, id string) (Document, error) {
	return h.store.getDoc(hookBypass, coll, id)
}

func (h *HookContext) Find(coll string, filter Document, opts *FindOptions) ([]Document, error) {
	return h.store.findDocs(hookBypass, coll, filter, opts)
}

func (h *HookContext) Count(coll string, filter Document) (int, error) {
	return h.store.countDocs(hookBypass, coll, filter)
}

// Insert/Upsert/Update/Delete propagate the hook frame (depth + cycle checks).
// They run as hookBypass (trusted; RBAC does not gate hook code).
func (h *HookContext) Insert(coll string, doc Document) error {
	return h.store.insertDocFrame(h.frame, hookBypass, coll, doc)
}

func (h *HookContext) Upsert(coll string, id string, doc Document) error {
	return h.store.upsertDocFrame(h.frame, hookBypass, coll, id, doc)
}

func (h *HookContext) Update(coll string, id string, patch Document) error {
	return h.store.updateDocFrame(h.frame, hookBypass, coll, id, patch, nil)
}

func (h *HookContext) Delete(coll string, id string) error {
	return h.store.deleteDocFrame(h.frame, hookBypass, coll, id)
}

// hookBypass is the trusted principal for hook-initiated mutations.
// system=true ⇒ valid without being in the sessions map; RBAC skips it.
// Store ops still refuse system collections for everyone.
var hookBypass = &Session{system: true, user: "_hook"}

// hookFrame links one hook invocation to its ancestors (depth + cycle walk).
type hookFrame struct {
	parent *hookFrame
	hookID uint64
	depth  int
}

type hookReg struct {
	id    uint64
	event string
	coll  string // "" or "*" = all collections
	fn    HookFunc
	async bool
}

type hookJob struct {
	frame   *hookFrame
	reg     *hookReg
	event   string
	coll    string
	id      string
	doc     Document
	old     Document
	barrier chan struct{} // WaitAsyncHooks sentinel (reg == nil)
}

// On registers a synchronous hook. coll "" or "*" matches every collection.
// Before-* hooks may veto (return error); after-* errors are ignored.
func (s *Store) On(event string, coll string, fn HookFunc) (uint64, error) {
	return s.addHook(event, coll, fn, false)
}

// OnAsync registers an after-* hook on the bounded FIFO worker queue.
// Enqueue blocks when the queue is full (backpressure). Before-* rejected:
// async hooks cannot veto.
func (s *Store) OnAsync(event string, coll string, fn HookFunc) (uint64, error) {
	switch event {
	case AfterInsert, AfterUpdate, AfterDelete, AfterUpsert:
	default:
		return 0, fmt.Errorf("db: OnAsync requires after-* event, got %q", event)
	}
	return s.addHook(event, coll, fn, true)
}

func (s *Store) addHook(event string, coll string, fn HookFunc, async bool) (uint64, error) {
	if !validHookEvent(event) {
		return 0, fmt.Errorf("db: unknown hook event %q", event)
	}
	if fn == nil {
		return 0, fmt.Errorf("db: nil hook func")
	}
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.hookSeq++
	id := s.hookSeq
	s.hooks = append(s.hooks, &hookReg{id: id, event: event, coll: coll, fn: fn, async: async})
	return id, nil
}

// Off removes a hook by id; reports whether it existed.
func (s *Store) Off(id uint64) bool {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	for i, r := range s.hooks {
		if r.id == id {
			s.hooks = append(s.hooks[:i], s.hooks[i+1:]...)
			return true
		}
	}
	return false
}

func validHookEvent(e string) bool {
	switch e {
	case BeforeInsert, AfterInsert, BeforeUpdate, AfterUpdate,
		BeforeDelete, AfterDelete, BeforeUpsert, AfterUpsert:
		return true
	}
	return false
}

func collMatch(pattern, coll string) bool {
	return pattern == "" || pattern == "*" || pattern == coll
}

// matchHooks returns regs for event+coll. Caller must not hold hooksMu.
func (s *Store) matchHooks(event, coll string) []*hookReg {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	var out []*hookReg
	for _, r := range s.hooks {
		if r.event == event && collMatch(r.coll, coll) {
			out = append(out, r)
		}
	}
	return out
}

// beginHookErr validates depth + cycle and builds the child context.
func (s *Store) beginHookErr(parent *hookFrame, r *hookReg) (*HookContext, error) {
	depth := 1
	if parent != nil {
		depth = parent.depth + 1
	}
	if depth > maxHookDepth {
		return nil, ErrHookDepth
	}
	for f := parent; f != nil; f = f.parent {
		if f.hookID == r.id {
			return nil, ErrHookCycle
		}
	}
	return &HookContext{
		frame: &hookFrame{parent: parent, hookID: r.id, depth: depth},
		store: s,
	}, nil
}

// runBeforeHooks executes matched before-* regs; first error vetoes.
func (s *Store) runBeforeHooks(frame *hookFrame, regs []*hookReg, event, coll, id string, doc, old Document) error {
	for _, r := range regs {
		h, err := s.beginHookErr(frame, r)
		if err != nil {
			return err
		}
		h.Event, h.Collection, h.ID, h.Doc, h.Old = event, coll, id, doc, old
		if err := r.fn(h); err != nil {
			return err
		}
	}
	return nil
}

// runAfterHooksInline executes sync after-* regs (errors ignored).
func (s *Store) runAfterHooksInline(frame *hookFrame, regs []*hookReg, event, coll, id string, doc, old Document) {
	for _, r := range regs {
		h, err := s.beginHookErr(frame, r)
		if err != nil {
			return
		}
		h.Event, h.Collection, h.ID, h.Doc, h.Old = event, coll, id, doc, old
		_ = r.fn(h)
	}
}

// queueAfterHook dispatches after-* regs: sync inline, async to FIFO queue.
// No-op when no regs match (zero overhead without hooks).
func (s *Store) queueAfterHook(frame *hookFrame, event, coll, id string, doc, old Document) {
	regs := s.matchHooks(event, coll)
	if len(regs) == 0 {
		return
	}
	for _, r := range regs {
		if !r.async {
			s.runAfterHooksInline(frame, []*hookReg{r}, event, coll, id, doc, old)
			continue
		}
		s.ensureHookWorker()
		job := hookJob{
			frame: frame,
			reg:   r,
			event: event,
			coll:  coll,
			id:    id,
			doc:   cloneDocSafe(doc),
			old:   cloneDocSafe(old),
		}
		s.hooksMu.Lock()
		q, stop := s.hookQ, s.hookStop
		s.hooksMu.Unlock()
		if q == nil {
			return
		}
		select {
		case q <- job: // blocks when full (bounded FIFO backpressure)
		case <-stop:
			return
		}
	}
}

func cloneDocSafe(d Document) Document {
	if d == nil {
		return nil
	}
	return clone(d)
}

// ensureHookWorker lazily starts the async FIFO worker.
func (s *Store) ensureHookWorker() {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	if s.hookQ != nil {
		return
	}
	q := make(chan hookJob, hookQueueSize)
	stop := make(chan struct{})
	done := make(chan struct{})
	s.hookQ, s.hookStop, s.hookDone = q, stop, done
	go s.hookWorker(q, stop, done)
}

// hookWorker drains the bounded FIFO. Channels are passed in (not re-read
// from fields) so stopHookWorker can nil the fields without a race.
func (s *Store) hookWorker(q <-chan hookJob, stop <-chan struct{}, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case job := <-q:
			if job.barrier != nil {
				close(job.barrier)
				continue
			}
			h, err := s.beginHookErr(job.frame, job.reg)
			if err != nil {
				continue // cycle/depth on async path: drop
			}
			h.Event, h.Collection, h.ID, h.Doc, h.Old =
				job.event, job.coll, job.id, job.doc, job.old
			_ = job.reg.fn(h)
		}
	}
}

// stopHookWorker stops the async worker (Close). Drops undelivered jobs.
func (s *Store) stopHookWorker() {
	s.hooksMu.Lock()
	stop, done := s.hookStop, s.hookDone
	s.hookQ, s.hookStop, s.hookDone = nil, nil, nil
	s.hooksMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// WaitAsyncHooks blocks until every queued async job is done (barrier).
// Test/ops helper — do not call from inside a hook (deadlock).
func (s *Store) WaitAsyncHooks() {
	s.hooksMu.Lock()
	q, stop := s.hookQ, s.hookStop
	s.hooksMu.Unlock()
	if q == nil {
		return
	}
	b := make(chan struct{})
	select {
	case q <- hookJob{barrier: b}:
	case <-stop:
		return
	}
	<-b
}
