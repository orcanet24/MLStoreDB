package db

import (
	"sync"
)

// parallelMinIDs is the candidate-count threshold for spawning Find workers.
const parallelMinIDs = 512

// materializeRefs loads resident docs for ids (optionally rematching filter)
// under the caller's s.mu.RLock. Workers never touch s.mu/flushMu — only
// loadEntry/match/index internals (idx.mu, cacheMu) which nest correctly.
//
// Order: results are concatenated per contiguous id-chunk in input order, so
// plan.order and sorted-id full scans keep their relative order without a
// re-sort. First non-nil error wins (match errors e.g. bad regex).
func (s *Store) materializeRefs(c *collection, ids []string, filter Document, rematch bool) ([]Document, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	workers := s.opts.findWorkers()
	if workers <= 1 || len(ids) < parallelMinIDs {
		return s.materializeRefsSerial(c, ids, filter, rematch)
	}
	// Chunk so each worker gets a contiguous slice of the input order.
	n := len(ids)
	chunk := (n + workers - 1) / workers
	if chunk < parallelMinIDs {
		chunk = parallelMinIDs
	}
	nchunks := (n + chunk - 1) / chunk
	results := make([][]Document, nchunks)
	errs := make([]error, nchunks)
	var wg sync.WaitGroup
	for i := 0; i < nchunks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lo := i * chunk
			hi := lo + chunk
			if hi > n {
				hi = n
			}
			results[i], errs[i] = s.materializeRefsSerial(c, ids[lo:hi], filter, rematch)
		}(i)
	}
	wg.Wait()
	var out []Document
	var firstErr error
	for i := range results {
		if errs[i] != nil && firstErr == nil {
			firstErr = errs[i]
		}
		out = append(out, results[i]...)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (s *Store) materializeRefsSerial(c *collection, ids []string, filter Document, rematch bool) ([]Document, error) {
	refs := make([]Document, 0, len(ids))
	for _, id := range ids {
		doc, err := s.resident(c, id)
		if err != nil {
			continue
		}
		if rematch {
			ok, err := match(doc, filter)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		refs = append(refs, doc)
	}
	return refs, nil
}

// countMatches is the Count twin of materializeRefs (no Document retention).
func (s *Store) countMatches(c *collection, ids []string, filter Document, rematch bool) (int, error) {
	refs, err := s.materializeRefs(c, ids, filter, rematch)
	if err != nil {
		return 0, err
	}
	return len(refs), nil
}
