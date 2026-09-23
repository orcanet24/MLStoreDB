package db

import (
	"sync/atomic"
)

// docEntry is one document slot in a collection: metadata always present,
// payload may be cold (docP == nil) and reloaded from the record log.
type docEntry struct {
	docP   atomic.Pointer[Document] // nil = cold (must load from file)
	recOff atomic.Int64             // record offset; -1 = never written
	recLen atomic.Int32             // full record length at recOff
	dirty  atomic.Bool              // mutated since last successful flush
	refBit atomic.Bool              // second-chance clock bit
	size   atomic.Int64             // resident payload estimate (bytes)

	// mutGen is only accessed under Store.mu.
	mutGen uint64 // dirtyGen when dirty was last set
}

func newDocEntry(doc Document) *docEntry {
	e := &docEntry{}
	e.recOff.Store(-1)
	if doc != nil {
		e.docP.Store(&doc)
	}
	return e
}

// resident returns the in-memory doc (loading from file if cold).
// Caller must hold Store.mu.
func (s *Store) resident(c *collection, id string) (Document, error) {
	e, ok := c.entries[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s.loadEntry(e)
}

// loadEntry materializes e.docP from the record log if cold.
// Caller must hold Store.mu. May perform file I/O (full scans / cold hits).
func (s *Store) loadEntry(e *docEntry) (Document, error) {
	if d := e.docP.Load(); d != nil {
		e.refBit.Store(true)
		return *d, nil
	}
	off := e.recOff.Load()
	length := e.recLen.Load()
	if length <= 0 || s.path == "" {
		return nil, ErrNotFound
	}
	raw := make([]byte, length)
	f, err := openRead(s.path)
	if err != nil {
		return nil, err
	}
	_, err = f.ReadAt(raw, off)
	_ = f.Close()
	if err != nil {
		return nil, ErrCorrupt
	}
	doc, err := s.decodeDocRecord(raw)
	if err != nil {
		return nil, err
	}
	if cur := e.docP.Load(); cur != nil {
		e.refBit.Store(true)
		return *cur, nil
	}
	e.docP.Store(&doc)
	e.size.Store(int64(length))
	e.refBit.Store(true)
	s.cacheAdmit(e)
	return doc, nil
}

// markEntryClean clears dirty after a successful flush if not re-mutated.
// Caller must hold Store.mu.
func (e *docEntry) markEntryClean(stGen uint64) {
	if e.mutGen <= stGen {
		e.dirty.Store(false)
	}
}

// CacheStats reports page-cache residency (M1).
type CacheStats struct {
	Resident     int    `json:"resident"`
	Cold         int    `json:"cold"`
	Bytes        int64  `json:"bytes"`
	Budget       int64  `json:"budget"`
	Evictions    uint64 `json:"evictions"`
	TotalEntries int    `json:"total_entries"`
}

// CacheStats returns current cache occupancy. Safe under concurrent use.
func (s *Store) CacheStats() CacheStats {
	s.mu.RLock()
	total := 0
	resident := 0
	for _, c := range s.collections {
		total += len(c.entries)
		for _, e := range c.entries {
			if e.docP.Load() != nil {
				resident++
			}
		}
	}
	budget := s.opts.cacheBudget()
	s.mu.RUnlock()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return CacheStats{
		Resident:     resident,
		Cold:         total - resident,
		Bytes:        s.residentBytes,
		Budget:       budget,
		Evictions:    s.cacheEvictions,
		TotalEntries: total,
	}
}

// cacheAdmit registers e as resident under the byte budget (best-effort).
// Never takes Store.mu (caller already holds it).
func (s *Store) cacheAdmit(e *docEntry) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = make(map[*docEntry]struct{})
	}
	if _, ok := s.cache[e]; ok {
		return
	}
	s.cache[e] = struct{}{}
	s.residentBytes += e.size.Load()
	s.evictLocked()
}

// evictLocked drops clean entries until under budget. Caller holds cacheMu.
func (s *Store) evictLocked() {
	budget := s.opts.cacheBudget()
	if budget <= 0 {
		return // unlimited (CacheBytes < 0)
	}
	// No eviction for RAM-only stores or while a v1 rewrite is pending.
	if s.path == "" || s.noEvict.Load() {
		return
	}
	for s.residentBytes > budget && len(s.cache) > 0 {
		evicted := false
		// Phase A: clean + refBit already clear.
		for e := range s.cache {
			if s.residentBytes <= budget {
				return
			}
			if e.dirty.Load() || e.refBit.Load() {
				continue
			}
			s.evictEntryLocked(e)
			evicted = true
		}
		if s.residentBytes <= budget {
			return
		}
		// Phase B: second chance — clear refBits.
		cleared := false
		for e := range s.cache {
			if e.refBit.Load() {
				e.refBit.Store(false)
				cleared = true
			}
		}
		if !cleared && !evicted {
			return // all remaining dirty
		}
	}
}

// evictEntryLocked clears the resident payload. Caller holds cacheMu.
func (s *Store) evictEntryLocked(e *docEntry) {
	if e.dirty.Load() {
		return
	}
	if e.docP.Load() == nil {
		delete(s.cache, e)
		return
	}
	e.docP.Store(nil)
	delete(s.cache, e)
	if n := e.size.Load(); n > 0 {
		s.residentBytes -= n
		if s.residentBytes < 0 {
			s.residentBytes = 0
		}
	}
	s.cacheEvictions++
}
