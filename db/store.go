package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gofrs/flock"
	"github.com/oklog/ulid/v2"
)

// Document is a JSON object with a required string _id.
type Document map[string]any

type collection struct {
	entries   map[string]*docEntry
	indexes   []*index
	sensitive []string
}

// Store is an in-memory JSON document store; optional encrypted file backing (M4).
// Safe for concurrent use.
type Store struct {
	mu            sync.RWMutex
	collections   map[string]*collection
	schemaVersion uint64

	// file backing (empty path = RAM-only)
	path    string
	opts    Options
	dek     []byte
	created uint64
	dirty   bool
	closed  bool

	kdfSalt [16]byte
	kdfTime uint32
	kdfMem  uint32
	kdfPar  uint32

	// M5 runtime
	lock      *flock.Flock
	flushStop chan struct{}
	flushDone chan struct{}

	// non-blocking flush (point 3)
	flushMu  sync.Mutex // serializes writeState (I/O) without holding mu
	dirtyGen uint64     // bumped by markDirty; flush clears dirty only if unchanged
	machine  []byte     // KEK machine id resolved at Open (persist across Flush)

	// M1 record log / cache
	sawFile         bool // a durable v2 body exists (enables append + DELs)
	v1Migration     bool // loaded v1 or suspect state → next flush is full rewrite
	compactForce    bool // next prepareFlush is a full rewrite
	flushInProgress bool // snapshot taken; deletes must enqueue DELs
	pendingDels     []delItem
	cacheMu         sync.Mutex
	cache           map[*docEntry]struct{}
	residentBytes   int64
	cacheEvictions  uint64
	noEvict         atomicBool // hold eviction (v1 migration window)
	kek             []byte     // cached KEK (Argon2 once per credentials)

	// M3 backpressure: mutations since last durable flush snapshot.
	writeSeq   uint64 // bumped by markDirty
	durableSeq uint64 // writeSeq included in the last successful writeState
	bpCond     *sync.Cond

	// M4 RBAC: token → live Session. Guarded by mu. Empty until Authenticate.
	sessions map[string]*Session

	// M5a hooks: registrations + bounded async FIFO worker.
	hooksMu  sync.Mutex
	hooks    []*hookReg
	hookSeq  uint64
	hookQ    chan hookJob
	hookStop chan struct{}
	hookDone chan struct{}

	// M5b declarative triggers: _triggers → M5a hook registration.
	trigMu         sync.Mutex
	triggersLoaded bool
	triggerHooks   map[string]uint64 // trigger _id → M5a hook id

	// M6 graph: CSR adjacency snapshots for edges.* collections.
	graphFields
}

// atomicBool wraps atomic.Bool for the eviction hold flag.
type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) Load() bool     { return b.v.Load() }
func (b *atomicBool) Store(x bool)   { b.v.Store(x) }

// New creates an empty in-memory store (schemaVersion 0 = needs migrations).
func New() *Store {
	s := &Store{
		collections: map[string]*collection{},
		sessions:    map[string]*Session{},
	}
	s.bpCond = sync.NewCond(&s.mu)
	return s
}

func (s *Store) coll(name string) *collection {
	c, ok := s.collections[name]
	if !ok {
		c = &collection{entries: map[string]*docEntry{}}
		s.collections[name] = c
	}
	return c
}

// EnsureIndex creates (or no-ops if identical) an index on coll.
// fields is 1..N (compound). unique enforces one doc per key combination.
// On non-empty coll, builds from existing docs; unique conflicts → ErrDuplicate.
func (s *Store) EnsureIndex(coll string, fields []string, unique bool) error {
	if err := s.ensureIndex(coll, fields, unique); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) ensureIndex(coll string, fields []string, unique bool) error {
	if len(fields) == 0 {
		return fmt.Errorf("%w: EnsureIndex needs at least one field", ErrBadFilter)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.coll(coll)
	for _, idx := range c.indexes {
		if sameFields(idx.Fields, fields) {
			if idx.Unique == unique {
				return nil
			}
			return fmt.Errorf("%w: index %v already exists with different uniqueness", ErrDuplicate, fields)
		}
	}
	idx := newIndex(fields, unique)
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		doc, err := s.resident(c, id)
		if err != nil {
			return err
		}
		if err := idx.addDoc(doc, id); err != nil {
			return err
		}
	}
	c.indexes = append(c.indexes, idx)
	s.markDirty()
	return nil
}

// DropIndex removes the index matching fields (order-sensitive).
func (s *Store) DropIndex(coll string, fields []string) error {
	if err := s.dropIndex(coll, fields); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) dropIndex(coll string, fields []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	for i, idx := range c.indexes {
		if sameFields(idx.Fields, fields) {
			c.indexes = append(c.indexes[:i], c.indexes[i+1:]...)
			s.markDirty()
			return nil
		}
	}
	return ErrNotFound
}

// ListIndexes returns index metadata for coll (empty slice if none/missing).
func (s *Store) ListIndexes(coll string) ([]IndexInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collRO(coll)
	if !ok {
		return []IndexInfo{}, nil
	}
	out := make([]IndexInfo, 0, len(c.indexes))
	for _, idx := range c.indexes {
		out = append(out, idx.info())
	}
	return out, nil
}

// applyIndexes after a doc change: remove old entries (if old non-nil), add new.
// On unique failure during add, restores old entries.
func applyIndexes(c *collection, old, newDoc Document, id string) error {
	for _, idx := range c.indexes {
		if old != nil {
			idx.removeDoc(old, id)
		}
	}
	var firstErr error
	for _, idx := range c.indexes {
		if err := idx.addDoc(newDoc, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		// rollback: drop whatever was added for newDoc, restore old
		for _, idx := range c.indexes {
			idx.removeDoc(newDoc, id)
		}
		if old != nil {
			for _, idx := range c.indexes {
				_ = idx.addDoc(old, id)
			}
		}
		return firstErr
	}
	return nil
}

func (s *Store) collRO(name string) (*collection, bool) {
	c, ok := s.collections[name]
	return c, ok
}

// maxDocBytes is NF1: 1 MB per document (DESIGN §3.1).
const maxDocBytes = 1 << 20

// docID extracts _id from doc. Missing/empty _id ⇒ auto ULID (DESIGN §3.1).
// Mutates doc when generating.
func docID(doc Document) (string, error) {
	v, ok := doc["_id"]
	if !ok || v == nil {
		id := ulid.Make().String()
		doc["_id"] = id
		return id, nil
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", ErrNoID
	}
	return id, nil
}

// checkDocSize rejects docs > 1MB (NF1) before insert/upsert.
func checkDocSize(doc Document) error {
	// cheap path: marshal only if map is large; always check for safety
	b, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadFilter, err)
	}
	if len(b) > maxDocBytes {
		return ErrTooLarge
	}
	return nil
}

// clone deep-copies doc so callers cannot mutate stored docs (nested too).
func clone(doc Document) Document {
	out := make(Document, len(doc))
	for k, v := range doc {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneShallow copies only top-level slots. Safe for Update rollback/index
// snapshot: Update replaces top-level keys and never mutates nested values
// in place, so shared nested refs stay intact for extractIndexRows(old).
func cloneShallow(doc Document) Document {
	out := make(Document, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	return out
}

// cloneValue deep-copies JSON-like values (maps/slices); scalars returned as-is.
// map[string]any stays map[string]any (not Document) so type asserts keep working.
func cloneValue(v any) any {
	switch t := v.(type) {
	case Document:
		out := make(Document, len(t))
		for k, e := range t {
			out[k] = cloneValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = cloneValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneValue(e)
		}
		return out
	case []Document:
		out := make([]Document, len(t))
		for i, d := range t {
			out[i] = clone(d)
		}
		return out
	case []string:
		return append([]string{}, t...)
	case string, bool, float64, float32, int, int32, int64, uint, uint64, nil:
		return v
	default:
		return v
	}
}

// Insert adds doc; fails if _id missing, already exists, unique conflict, or >1MB.
func (s *Store) Insert(coll string, doc Document) error {
	if err := s.insertDoc(nil, coll, doc); err != nil {
		return err
	}
	return s.afterMutation()
}

// insertDoc is Insert/Session.Insert with optional RBAC session (M4).
// nil sess = raw Store API (allowed only while no users exist).
func (s *Store) insertDoc(sess *Session, coll string, doc Document) error {
	return s.insertDocFrame(nil, sess, coll, doc)
}

// insertDocFrame runs before/after hooks around the locked apply (M5a).
func (s *Store) insertDocFrame(frame *hookFrame, sess *Session, coll string, doc Document) error {
	id, err := docID(doc)
	if err != nil {
		return err
	}
	if err := checkDocSize(doc); err != nil {
		return err
	}
	if before := s.matchHooks(BeforeInsert, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		if err := s.previewMissing(coll, id, ErrDuplicate); err != nil {
			return err
		}
		work := clone(doc)
		if err := s.runBeforeHooks(frame, before, BeforeInsert, coll, id, work, nil); err != nil {
			return err
		}
		doc = work
	}
	if err := s.insertApply(sess, coll, id, doc); err != nil {
		return err
	}
	s.queueAfterHook(frame, AfterInsert, coll, id, doc, nil)
	return nil
}

func (s *Store) insertApply(sess *Session, coll string, id string, doc Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c := s.coll(coll)
	if _, exists := c.entries[id]; exists {
		return ErrDuplicate
	}
	stored := clone(doc)
	if err := applyIndexes(c, nil, stored, id); err != nil {
		return err
	}
	e := newDocEntry(stored)
	s.markEntryMutated(e)
	c.entries[id] = e
	s.cacheAdmit(e)
	s.noteEdgeMutation(coll)
	return nil
}

// previewWrite auth-checks under RLock (before-hook phase; no side effects).
func (s *Store) previewWrite(sess *Session, coll string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkWriteLocked(sess, coll)
}

// previewMissing returns errIfPresent when the id already exists (pre-hook).
func (s *Store) previewMissing(coll, id string, errIfPresent error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.collections[coll]; ok {
		if _, exists := c.entries[id]; exists {
			return errIfPresent
		}
	}
	return nil
}

// previewDoc clones the current doc or ErrNotFound (before-hook phase).
func (s *Store) previewDoc(coll, id string) (Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collRO(coll)
	if !ok {
		return nil, ErrNotFound
	}
	d, err := s.resident(c, id)
	if err != nil {
		return nil, err
	}
	return clone(d), nil
}

// Upsert replaces or creates the doc with the given _id (id wins over doc["_id"]).
func (s *Store) Upsert(coll string, id string, doc Document) error {
	if err := s.upsertDoc(nil, coll, id, doc); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) upsertDoc(sess *Session, coll string, id string, doc Document) error {
	return s.upsertDocFrame(nil, sess, coll, id, doc)
}

func (s *Store) upsertDocFrame(frame *hookFrame, sess *Session, coll string, id string, doc Document) error {
	if id == "" {
		return ErrNoID
	}
	if err := checkDocSize(doc); err != nil {
		return err
	}
	var old Document
	if before := s.matchHooks(BeforeUpsert, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		old, _ = s.previewDoc(coll, id) // nil when creating
		work := clone(doc)
		if err := s.runBeforeHooks(frame, before, BeforeUpsert, coll, id, work, old); err != nil {
			return err
		}
		doc = work
	}
	if err := s.upsertApply(sess, coll, id, doc); err != nil {
		return err
	}
	s.queueAfterHook(frame, AfterUpsert, coll, id, doc, old)
	return nil
}

func (s *Store) upsertApply(sess *Session, coll string, id string, doc Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c := s.coll(coll)
	var old Document
	if _, ok := c.entries[id]; ok {
		d, err := s.resident(c, id)
		if err == nil {
			old = d
		}
	}
	stored := clone(doc)
	stored["_id"] = id
	if err := applyIndexes(c, old, stored, id); err != nil {
		return err
	}
	if e, ok := c.entries[id]; ok {
		e.docP.Store(&stored)
		s.markEntryMutated(e)
		s.cacheAdmit(e)
	} else {
		e := newDocEntry(stored)
		s.markEntryMutated(e)
		c.entries[id] = e
		s.cacheAdmit(e)
	}
	s.noteEdgeMutation(coll)
	return nil
}

// Update shallow-merges patch top-level keys into the existing doc.
func (s *Store) Update(coll string, id string, patch Document) error {
	if err := s.updateDoc(nil, coll, id, patch); err != nil {
		return err
	}
	return s.afterMutation()
}

// UpdateFields is Update plus explicit top-level field removals (M8 wire:
// Mongo $unset / replacement diffs). Removals are applied to the merged
// preview before hooks run (hooks see the final shape) and land in the same
// updateApply path, so triggers/async hooks behave exactly like an Update.
func (s *Store) UpdateFields(coll string, id string, patch Document, remove []string) error {
	if err := s.updateDocFrame(nil, nil, coll, id, patch, remove); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) updateDoc(sess *Session, coll string, id string, patch Document) error {
	return s.updateDocFrame(nil, sess, coll, id, patch, nil)
}

func (s *Store) updateDocFrame(frame *hookFrame, sess *Session, coll string, id string, patch Document, preRemove []string) error {
	var old Document
	var remove []string
	if before := s.matchHooks(BeforeUpdate, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		var err error
		old, err = s.previewDoc(coll, id)
		if err != nil {
			return err
		}
		// Hooks see the merged preview (old + patch) so set/unset act on the
		// final shape; afterwards we diff back into patch + removals.
		merged := clone(old)
		for k, v := range patch {
			if k == "_id" {
				continue
			}
			merged[k] = cloneValue(v)
		}
		for _, k := range preRemove {
			if k != "_id" {
				delete(merged, k)
			}
		}
		if err := s.runBeforeHooks(frame, before, BeforeUpdate, coll, id, merged, old); err != nil {
			return err
		}
		patch = Document{}
		for k, v := range merged {
			if k == "_id" {
				continue
			}
			ov, existed := old[k]
			if !existed || !equalJSON(ov, v) {
				patch[k] = cloneValue(v)
			}
		}
		for k := range old {
			if k == "_id" {
				continue
			}
			if _, ok := merged[k]; !ok {
				remove = append(remove, k)
			}
		}
	} else if len(preRemove) > 0 {
		for _, k := range preRemove {
			if k != "_id" {
				remove = append(remove, k)
			}
		}
	}
	if err := s.updateApply(sess, coll, id, patch, remove); err != nil {
		return err
	}
	if s.hasAfter(AfterUpdate, coll) {
		if fresh, err := s.previewDoc(coll, id); err == nil {
			s.queueAfterHook(frame, AfterUpdate, coll, id, fresh, old)
		} else {
			s.queueAfterHook(frame, AfterUpdate, coll, id, patch, old)
		}
		return nil
	}
	s.queueAfterHook(frame, AfterUpdate, coll, id, patch, old)
	return nil
}

func (s *Store) hasAfter(event, coll string) bool {
	return len(s.matchHooks(event, coll)) > 0
}

func (s *Store) updateApply(sess *Session, coll string, id string, patch Document, remove []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	if _, ok := c.entries[id]; !ok {
		return ErrNotFound
	}
	doc, err := s.resident(c, id)
	if err != nil {
		return err
	}
	// COW: build next off to the side so a failed size/index check leaves the
	// live resident map untouched (no in-place rollback).
	old := cloneShallow(doc)
	next := cloneShallow(doc)
	for k, v := range patch {
		if k == "_id" {
			continue
		}
		next[k] = cloneValue(v)
	}
	for _, k := range remove {
		delete(next, k)
	}
	if err := checkDocSize(next); err != nil {
		return err
	}
	if err := applyIndexes(c, old, next, id); err != nil {
		return err
	}
	e := c.entries[id]
	e.docP.Store(&next)
	s.markEntryMutated(e)
	s.noteEdgeMutation(coll)
	return nil
}

// Delete removes doc by _id.
func (s *Store) Delete(coll string, id string) error {
	if err := s.deleteDoc(nil, coll, id); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) deleteDoc(sess *Session, coll string, id string) error {
	return s.deleteDocFrame(nil, sess, coll, id)
}

func (s *Store) deleteDocFrame(frame *hookFrame, sess *Session, coll string, id string) error {
	var old Document
	if before := s.matchHooks(BeforeDelete, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		var err error
		old, err = s.previewDoc(coll, id)
		if err != nil {
			return err
		}
		if err := s.runBeforeHooks(frame, before, BeforeDelete, coll, id, nil, old); err != nil {
			return err
		}
	}
	if err := s.deleteApply(sess, coll, id); err != nil {
		return err
	}
	s.queueAfterHook(frame, AfterDelete, coll, id, nil, old)
	return nil
}

func (s *Store) deleteApply(sess *Session, coll string, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	e, ok := c.entries[id]
	if !ok {
		return ErrNotFound
	}
	doc, err := s.loadEntry(e)
	if err != nil && !isNotFound(err) {
		return err
	}
	if doc != nil {
		for _, idx := range c.indexes {
			idx.removeDoc(doc, id)
		}
	}
	// Evict from cache registry.
	s.cacheMu.Lock()
	if _, in := s.cache[e]; in {
		s.evictEntryLocked(e)
	}
	s.cacheMu.Unlock()
	delete(c.entries, id)
	s.markDirty()
	s.noteEdgeMutation(coll)
	// Resurrection guard: record DEL when a durable body exists or a flush
	// snapshot may already include this doc.
	if s.sawFile || s.flushInProgress {
		s.pendingDels = append(s.pendingDels, delItem{coll: coll, id: id, gen: s.dirtyGen})
	}
	return nil
}

func isNotFound(err error) bool { return err == ErrNotFound }

// waitBackpressureLocked blocks while a flush is in progress and the number of
// mutations since the last durable snapshot exceeds Options.MaxPendingWrites.
// Caller holds s.mu (write). Wait releases s.mu so the flush can finish.
// No-op when MaxPendingWrites is 0 (legacy unlimited).
func (s *Store) waitBackpressureLocked() {
	limit := s.opts.maxPendingWrites()
	if limit <= 0 || s.bpCond == nil {
		return
	}
	for s.flushInProgress && int(s.writeSeq-s.durableSeq) >= limit {
		s.bpCond.Wait()
	}
}

// Get returns a copy of the doc, or ErrNotFound.
// When ≥1 user exists (M4), raw Store reads return ErrUnauthorized — use
// Authenticate → Session.Get instead.
func (s *Store) Get(coll string, id string) (Document, error) {
	return s.getDoc(nil, coll, id)
}

func (s *Store) getDoc(sess *Session, coll string, id string) (Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return nil, err
	}
	c, ok := s.collRO(coll)
	if !ok {
		return nil, ErrNotFound
	}
	doc, err := s.resident(c, id)
	if err != nil {
		return nil, err
	}
	out := clone(doc)
	if sess != nil {
		redactDoc(out, sess.fieldDenySet(coll))
	}
	return out, nil
}

// FindOptions controls result shape (sort/skip/limit from M1; projection M2).
type FindOptions struct {
	Sort       map[string]int // 1 asc, -1 desc
	Limit      int
	Skip       int
	Projection []string // top-level fields to return (+ _id always); empty = all
}

// Find uses an index seek when possible (equality/$in on indexed field),
// else full-scans coll. nil/empty filter returns all docs.
// Filter operators: see filter.go ($eq $ne $gt $gte $lt $lte $in $nin $regex $exists $and $or $not $nor).
// Results ordered by _id when no Sort is given (deterministic).
// Only the returned page is cloned (refs held during sort/skip/limit).
// When ≥1 user exists (M4), raw Store reads return ErrUnauthorized.
func (s *Store) Find(coll string, filter Document, opts *FindOptions) ([]Document, error) {
	return s.findDocs(nil, coll, filter, opts)
}

func (s *Store) findDocs(sess *Session, coll string, filter Document, opts *FindOptions) ([]Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return nil, err
	}
	// Session fieldDeny: forbid filters that probe hidden fields (M4).
	var deny map[string]bool
	if sess != nil {
		deny = sess.fieldDenySet(coll)
		if len(deny) > 0 && filterTouchesFields(deny, filter) {
			return nil, ErrForbidden
		}
	}
	c, ok := s.collRO(coll)
	if !ok {
		return nil, nil
	}

	// Hold pointers into the store during match/sort; clone only the page out.
	var refs []Document
	plan := planCandidates(c, filter)
	if plan.used {
		var idList []string
		if plan.order != nil {
			idList = plan.order
		} else {
			idList = make([]string, 0, len(plan.cand))
			for id := range plan.cand {
				idList = append(idList, id)
			}
		}
		// exact ⇒ candidates already satisfy the filter (no rematch).
		// !exact ⇒ rematch each loaded doc (range / partial prefix).
		var err error
		refs, err = s.materializeRefs(c, idList, filter, !plan.exact)
		if err != nil {
			return nil, err
		}
	} else {
		ids := make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var err error
		refs, err = s.materializeRefs(c, ids, filter, true)
		if err != nil {
			return nil, err
		}
	}

	// Sort: index-ordered short-circuit when sort field == range index field.
	sortedByIndex := false
	if opts != nil && opts.Sort != nil && len(opts.Sort) == 1 && plan.order != nil {
		for sf, dir := range opts.Sort {
			if sf == plan.orderField {
				if dir < 0 {
					reverseRefs(refs)
				}
				sortedByIndex = true
			}
		}
	}
	// Deterministic base order by _id only when no user Sort (or full sort later).
	if !sortedByIndex && (opts == nil || opts.Sort == nil || len(opts.Sort) == 0) {
		sortRefsByID(refs)
	}

	if opts != nil {
		if opts.Sort != nil && len(opts.Sort) > 0 && !sortedByIndex {
			need := 0
			if opts.Limit > 0 {
				need = opts.Skip + opts.Limit
			}
			sortByPartial(refs, opts.Sort, need)
		}
		if opts.Skip > 0 {
			if opts.Skip >= len(refs) {
				return []Document{}, nil
			}
			refs = refs[opts.Skip:]
		}
		if opts.Limit > 0 && opts.Limit < len(refs) {
			refs = refs[:opts.Limit]
		}
	}

	// Clone only what the caller receives (page, not the whole match set).
	out := make([]Document, len(refs))
	for i, doc := range refs {
		if opts != nil && len(opts.Projection) > 0 {
			out[i] = project(doc, opts.Projection)
		} else {
			out[i] = clone(doc)
		}
	}
	if len(deny) > 0 {
		for _, d := range out {
			redactDoc(d, deny)
		}
	}
	return out, nil
}

func sortRefsByID(refs []Document) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i]["_id"].(string) < refs[j]["_id"].(string)
	})
}

func reverseRefs(refs []Document) {
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
}

// sortByPartial sorts docs by sortSpec. If need>0 and need<n and spec has a
// single key, uses a bounded max-heap of size need (O(n log k)); else full sort.
// After a partial sort, only the first min(need,n) elements are correctly ordered.
func sortByPartial(docs []Document, spec map[string]int, need int) {
	if need <= 0 || need >= len(docs) || len(spec) != 1 {
		sortBy(docs, spec)
		return
	}
	var field string
	var dir int
	for k, d := range spec {
		field, dir = k, d
	}
	// max-heap of (key, doc) keeping the `need` best according to dir
	h := &docHeap{dir: dir}
	for _, d := range docs {
		h.push(d, d[field])
		if h.Len() > need {
			h.pop()
		}
	}
	// extract in order into docs[0:need], rest stay as-is (unordered tail)
	n := h.Len()
	for i := n - 1; i >= 0; i-- {
		docs[i] = h.pop()
	}
}

// docHeap keeps the `need` best docs: for asc (dir>0) evict largest; for desc evict smallest.
type docHeap struct {
	keys []any
	docs []Document
	dir  int // 1 asc, -1 desc
}

func (h *docHeap) Len() int { return len(h.docs) }

// betterKey reports whether key a is preferable to b (a should be kept over b).
// Ascending: smaller key is better; Descending: larger key is better.
func (h *docHeap) betterKey(a, b any) bool {
	cmp := compareValues(a, b)
	if h.dir < 0 {
		return cmp > 0
	}
	return cmp < 0
}

// worstIdx returns the index of the worst kept element (candidate to evict).
func (h *docHeap) worstIdx() int {
	wi := 0
	for i := 1; i < len(h.keys); i++ {
		// if keys[wi] is better than keys[i], then i is worse
		if h.betterKey(h.keys[wi], h.keys[i]) {
			wi = i
		}
	}
	return wi
}

func (h *docHeap) push(d Document, k any) {
	h.docs = append(h.docs, d)
	h.keys = append(h.keys, k)
}

// pop removes and returns the worst kept element.
func (h *docHeap) pop() Document {
	wi := h.worstIdx()
	d := h.docs[wi]
	h.docs = append(h.docs[:wi], h.docs[wi+1:]...)
	h.keys = append(h.keys[:wi], h.keys[wi+1:]...)
	return d
}

// planResult describes how the planner used indexes for one filter.
type planResult struct {
	cand       map[string]struct{}
	order      []string // IDs in index order when a range seek produced them
	orderField string   // indexed field corresponding to order (sort-by-index)
	exact      bool     // no re-match needed
	used       bool     // at least one index seek
	index      []string // index fields used (Explain)
	covered    []string // top-level filter fields served by equality seeks
}

// fieldPlan is one top-level filter field classified for the planner.
type fieldPlan struct {
	field   string
	eqVals  []any
	isEq    bool
	rb      rangeBound
	isRange bool
	blocked bool // logical op or unsupported cond
}

// planCandidates seeks indexes for equality/$in (single + compound prefix)
// and ranges on the first indexed field. Ranges always set exact=false so
// Find re-matches (cross-type / residual predicates). Logical ops force
// re-match but do not block other field seeks.
func planCandidates(c *collection, filter Document) planResult {
	res := planResult{exact: true}
	if len(filter) == 0 || len(c.indexes) == 0 {
		return planResult{exact: len(filter) == 0, used: false}
	}

	// Classify top-level fields.
	plans := make([]fieldPlan, 0, len(filter))
	for field, cond := range filter {
		if isLogicalOp(field) {
			plans = append(plans, fieldPlan{field: field, blocked: true})
			continue
		}
		if vals, ok := equalityValues(cond); ok {
			plans = append(plans, fieldPlan{field: field, eqVals: vals, isEq: true})
			continue
		}
		if rb, ok := rangeValues(cond); ok {
			plans = append(plans, fieldPlan{field: field, rb: rb, isRange: true})
			continue
		}
		plans = append(plans, fieldPlan{field: field, blocked: true})
	}

	// Mark fields consumed by a multi-field compound equality seek.
	consumed := map[string]bool{}

	// Pass 1: compound indexes — leading single-value equalities.
	for _, idx := range c.indexes {
		if len(idx.Fields) < 2 {
			continue
		}
		vals := make([]any, 0, len(idx.Fields))
		var coveredFields []string
		for _, f := range idx.Fields {
			v, ok := singleEqualityFor(plans, f)
			if !ok {
				break
			}
			vals = append(vals, v)
			coveredFields = append(coveredFields, f)
		}
		if len(coveredFields) == 0 {
			continue
		}
		got := idx.seekPrefix(vals)
		if res.cand == nil {
			res.cand = got
		} else {
			intersectIDs(res.cand, got)
		}
		res.used = true
		res.index = idx.Fields
		for _, f := range coveredFields {
			consumed[f] = true
			res.covered = append(res.covered, f)
		}
	}
	// Exact only if every covered field is fully equality-served; ranges/blocked clear exact below.

	// Pass 2: remaining single-field equality and ranges.
	for _, fp := range plans {
		if fp.blocked {
			res.exact = false
			continue
		}
		if consumed[fp.field] {
			continue
		}
		if fp.isEq {
			got, idxFields, ok := seekEqualityField(c, fp.field, fp.eqVals)
			if !ok {
				res.exact = false
				continue
			}
			if res.cand == nil {
				res.cand = got
			} else {
				intersectIDs(res.cand, got)
			}
			res.used = true
			res.index = idxFields
			res.covered = append(res.covered, fp.field)
			continue
		}
		if fp.isRange {
			idx := findIndexForRange(c, fp.field)
			if idx == nil {
				res.exact = false
				continue
			}
			ids := idx.seekRange(fp.rb)
			got := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				got[id] = struct{}{}
			}
			if res.cand == nil {
				res.cand = got
				res.order = ids
				res.orderField = fp.field
			} else {
				// Preserve range order filtered by existing candidates.
				filtered := make([]string, 0, len(ids))
				for _, id := range ids {
					if _, ok := res.cand[id]; ok {
						filtered = append(filtered, id)
					}
				}
				intersectIDs(res.cand, got)
				res.order = filtered
				res.orderField = fp.field
			}
			res.used = true
			res.index = idx.Fields
			res.exact = false // ranges always re-match
			continue
		}
		res.exact = false
	}

	if !res.used {
		return planResult{exact: false, used: false}
	}
	// Reconcile ordered IDs with final candidate set (equality may have removed some).
	if res.order != nil && res.cand != nil {
		filtered := res.order[:0]
		for _, id := range res.order {
			if _, ok := res.cand[id]; ok {
				filtered = append(filtered, id)
			}
		}
		res.order = filtered
	}
	// Every top-level field must be equality-covered for exactness.
	if len(res.covered) != len(filter) {
		res.exact = false
	}
	return res
}

// singleEqualityFor returns a single literal equality value for field from plans.
func singleEqualityFor(plans []fieldPlan, field string) (any, bool) {
	for _, fp := range plans {
		if fp.field == field && fp.isEq && len(fp.eqVals) == 1 {
			return fp.eqVals[0], true
		}
	}
	return nil, false
}

// seekEqualityField seeks $eq/literal/$in on field via single-field index
// or as the leading field of a compound index.
func seekEqualityField(c *collection, field string, vals []any) (map[string]struct{}, []string, bool) {
	if idx := findIndexForField(c, field); idx != nil {
		got := map[string]struct{}{}
		for _, v := range vals {
			for id := range idx.lookupIDs([]any{v}) {
				got[id] = struct{}{}
			}
		}
		return got, idx.Fields, true
	}
	// Compound index with field as leading component ($in → union of prefix seeks).
	for _, idx := range c.indexes {
		if len(idx.Fields) > 1 && idx.Fields[0] == field {
			got := map[string]struct{}{}
			for _, v := range vals {
				for id := range idx.seekPrefix([]any{v}) {
					got[id] = struct{}{}
				}
			}
			return got, idx.Fields, true
		}
	}
	return nil, nil, false
}

func findIndexForRange(c *collection, field string) *index {
	for _, idx := range c.indexes {
		if idx.Fields[0] == field {
			return idx
		}
	}
	return nil
}

func intersectIDs(dst, src map[string]struct{}) {
	for id := range dst {
		if _, ok := src[id]; !ok {
			delete(dst, id)
		}
	}
}

// rangeValues extracts a pure range conjunction ($gt/$gte/$lt/$lte only)
// from a field condition. Mixed with other ops → not indexable as range.
func rangeValues(cond any) (rangeBound, bool) {
	var ops Document
	switch t := cond.(type) {
	case Document:
		ops = t
	case map[string]any:
		ops = Document(t)
	default:
		return rangeBound{}, false
	}
	if !hasOpKeys(ops) {
		return rangeBound{}, false
	}
	var rb rangeBound
	n := 0
	for op, arg := range ops {
		switch op {
		case "$gt":
			if rb.hasLo {
				return rangeBound{}, false
			}
			rb.hasLo, rb.lo, rb.loIncl = true, arg, false
			n++
		case "$gte":
			if rb.hasLo {
				return rangeBound{}, false
			}
			rb.hasLo, rb.lo, rb.loIncl = true, arg, true
			n++
		case "$lt":
			if rb.hasHi {
				return rangeBound{}, false
			}
			rb.hasHi, rb.hi, rb.hiIncl = true, arg, false
			n++
		case "$lte":
			if rb.hasHi {
				return rangeBound{}, false
			}
			rb.hasHi, rb.hi, rb.hiIncl = true, arg, true
			n++
		default:
			return rangeBound{}, false
		}
	}
	if n == 0 {
		return rangeBound{}, false
	}
	return rb, true
}

// ExplainResult reports how Find would execute a filter (point 5).
type ExplainResult struct {
	Collection     string   `json:"collection"`
	Plan           string   `json:"plan"` // COLLSCAN | IXSCAN
	Index          []string `json:"index,omitempty"`
	Covered        []string `json:"covered,omitempty"`
	Candidates     int      `json:"candidates"`
	Exact          bool     `json:"exact"`
	ReMatch        bool     `json:"re_match"`
	FilterFields   []string `json:"filter_fields"`
	CollectionSize int      `json:"collection_size"`
}

// Explain returns the query plan for filter without executing match on docs.
func (s *Store) Explain(coll string, filter Document) ExplainResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := ExplainResult{Collection: coll, Plan: "COLLSCAN"}
	for f := range filter {
		res.FilterFields = append(res.FilterFields, f)
	}
	sort.Strings(res.FilterFields)
	c, ok := s.collRO(coll)
	if !ok {
		res.ReMatch = true
		return res
	}
	res.CollectionSize = len(c.entries)
	if len(filter) == 0 {
		res.Plan = "COLLSCAN"
		res.Candidates = len(c.entries)
		res.Exact = true
		res.ReMatch = false
		return res
	}
	plan := planCandidates(c, filter)
	if plan.used {
		res.Plan = "IXSCAN"
		res.Index = plan.index
		res.Covered = plan.covered
		res.Candidates = len(plan.cand)
		res.Exact = plan.exact
		res.ReMatch = !plan.exact
	} else {
		res.Candidates = len(c.entries)
		res.ReMatch = true
	}
	return res
}

// equalityValues returns literal values for field: value, { $eq: v }, or { $in: [...] }.
func equalityValues(cond any) ([]any, bool) {
	if ops, ok := cond.(Document); ok && hasOpKeys(ops) {
		if eq, has := ops["$eq"]; has && len(ops) == 1 {
			return []any{eq}, true
		}
		if in, has := ops["$in"]; has && len(ops) == 1 {
			list, err := toAnyList(in)
			if err != nil {
				return nil, false
			}
			return list, true
		}
		return nil, false
	}
	if ops, ok := cond.(map[string]any); ok && hasOpKeys(Document(ops)) {
		return equalityValues(Document(ops))
	}
	return []any{cond}, true
}

func findIndexForField(c *collection, field string) *index {
	for _, idx := range c.indexes {
		if len(idx.Fields) == 1 && idx.Fields[0] == field {
			return idx
		}
	}
	return nil
}

// project keeps only _id + listed top-level fields (deep-copies values).
func project(doc Document, fields []string) Document {
	out := Document{}
	if v, ok := doc["_id"]; ok {
		out["_id"] = v
	}
	for _, f := range fields {
		if f == "_id" {
			continue
		}
		if v, ok := doc[f]; ok {
			out[f] = cloneValue(v)
		}
	}
	return out
}

// Count returns number of matching docs without cloning them.
// Pure equality/$in on indexed fields ⇒ O(1)/O(k) from the index (no scan).
// When ≥1 user exists (M4), raw Store reads return ErrUnauthorized.
func (s *Store) Count(coll string, filter Document) (int, error) {
	return s.countDocs(nil, coll, filter)
}

func (s *Store) countDocs(sess *Session, coll string, filter Document) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return 0, err
	}
	if sess != nil {
		deny := sess.fieldDenySet(coll)
		if len(deny) > 0 && filterTouchesFields(deny, filter) {
			return 0, ErrForbidden
		}
	}
	c, ok := s.collRO(coll)
	if !ok {
		return 0, nil
	}
	if len(filter) > 0 {
		// Fast path: single-field equality/$in fully covered by one index.
		if n, hit := countIndexed(c, filter); hit {
			return n, nil
		}
	}
	plan := planCandidates(c, filter)
	if plan.used {
		if plan.exact {
			return len(plan.cand), nil
		}
		var idList []string
		if plan.order != nil {
			idList = plan.order
		} else {
			idList = make([]string, 0, len(plan.cand))
			for id := range plan.cand {
				idList = append(idList, id)
			}
		}
		return s.countMatches(c, idList, filter, true)
	}
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return s.countMatches(c, ids, filter, true)
}

// countIndexed handles Count when filter is a single top-level field with
// equality or $in only, and that field has a single-field index.
func countIndexed(c *collection, filter Document) (int, bool) {
	if len(filter) != 1 {
		return 0, false
	}
	for field, cond := range filter {
		if isLogicalOp(field) {
			return 0, false
		}
		vals, ok := equalityValues(cond)
		if !ok {
			return 0, false
		}
		idx := findIndexForField(c, field)
		if idx == nil {
			return 0, false
		}
		// $in / multi-value equality: sum per value (unique keys don't overlap across values)
		n := 0
		for _, v := range vals {
			n += idx.countKey([]any{v})
		}
		return n, true
	}
	return 0, false
}

// Collections lists collection names (sorted). System collections
// (_users/_roles, M4) are hidden.
func (s *Store) Collections() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.collections))
	for n := range s.collections {
		if isSystemColl(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SchemaVersion returns the store schema version.
func (s *Store) SchemaVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schemaVersion
}

// CreateCollection registers an empty collection (M8 wire). It survives
// flush/reopen via the META record even with no documents. Returns
// ErrExists when the collection is already present.
func (s *Store) CreateCollection(coll string) error {
	return s.createCollection(nil, coll)
}

func (s *Store) createCollection(sess *Session, coll string) error {
	if coll == "" || strings.HasPrefix(coll, "$") || strings.ContainsRune(coll, 0) {
		return ErrForbidden
	}
	if isSystemColl(coll) {
		return ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	if _, ok := s.collections[coll]; ok {
		return ErrExists
	}
	s.collections[coll] = &collection{entries: map[string]*docEntry{}}
	s.markDirty()
	return nil
}

// DropCollection removes a collection and all its documents (M8 wire).
// Every document goes through the normal delete path (DEL records, hooks,
// cache eviction); afterwards the collection definition leaves the map and
// the next flush META stops listing it. Stale DOC/IDX records from earlier
// commits are ignored on reopen (the v2 scan is META-gated). Hooks may veto
// individual deletes, leaving a partially dropped collection.
func (s *Store) DropCollection(coll string) error {
	return s.dropCollection(nil, coll)
}

func (s *Store) dropCollection(sess *Session, coll string) error {
	if coll == "" || isSystemColl(coll) {
		return ErrForbidden
	}
	if err := s.previewWrite(sess, coll); err != nil {
		return err
	}
	s.mu.RLock()
	var ids []string
	if c, ok := s.collections[coll]; ok {
		ids = make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
	} else {
		s.mu.RUnlock()
		return ErrNotFound
	}
	s.mu.RUnlock()
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.deleteDoc(sess, coll, id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if c, ok := s.collections[coll]; ok {
		s.purgeCollCacheLocked(c)
		delete(s.collections, coll)
		s.markDirty()
	}
	s.mu.Unlock()
	if isEdgeColl(coll) {
		s.graphMu.Lock()
		s.graphEpoch.Add(1)
		s.graphMu.Unlock()
	}
	return s.afterMutation()
}

// matchEqual removed — superseded by match() in filter.go (M2).

func equalJSON(a, b any) bool {
	// Numbers from JSON are float64; accept int/float cross-compare.
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

func sortBy(docs []Document, sortSpec map[string]int) {
	if len(docs) < 2 {
		return
	}
	// _id tiebreak first (lowest priority — later stable sorts keep it for equal keys).
	// Then user keys in reverse alphabetical so the first alpha key wins.
	sort.SliceStable(docs, func(a, b int) bool {
		return docs[a]["_id"].(string) < docs[b]["_id"].(string)
	})

	ordered := sortedKeys(sortSpec)
	for i := len(ordered) - 1; i >= 0; i-- {
		k := ordered[i]
		dir := sortSpec[k]
		// precompute keys for this pass
		ks := make([]any, len(docs))
		for di, d := range docs {
			ks[di] = d[k]
		}
		sort.SliceStable(docs, func(a, b int) bool {
			cmp := compareValues(ks[a], ks[b])
			if dir < 0 {
				return cmp > 0
			}
			return cmp < 0
		})
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func compareValues(a, b any) int {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		switch {
		case as < bs:
			return -1
		case as > bs:
			return 1
		default:
			return 0
		}
	}
	// null/missing last-ish: nil < everything
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	if b == nil {
		return 1
	}
	return 0
}
