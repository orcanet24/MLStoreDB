package db

import (
	"sync"
)

// parallelMinIDs es el umbral de número de candidatos para lanzar workers de Find.
const parallelMinIDs = 512

// materializeRefs carga los documentos residentes de los ids (opcionalmente
// reevaluando el filtro) bajo el s.mu.RLock del llamador. Los workers nunca tocan
// s.mu/flushMu — solo los internos de loadEntry/match/index (idx.mu, cacheMu), que
// se anidan correctamente.
//
// Orden: los resultados se concatenan por trozo contiguo de ids en el orden de
// entrada, de modo que plan.order y los recorridos completos de ids ordenados conservan
// su orden relativo sin volver a ordenar. Gana el primer error no nil (errores de
// evaluación, por ejemplo un regex inválido).
func (s *Store) materializeRefs(c *collection, ids []string, filter Document, rematch bool) ([]Document, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	workers := s.opts.findWorkers()
	if workers <= 1 || len(ids) < parallelMinIDs {
		return s.materializeRefsSerial(c, ids, filter, rematch)
	}
	// Trocear para que cada worker reciba una porción contigua del orden de entrada.
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

// countMatches es el gemelo para Count de materializeRefs (no retiene Document).
func (s *Store) countMatches(c *collection, ids []string, filter Document, rematch bool) (int, error) {
	refs, err := s.materializeRefs(c, ids, filter, rematch)
	if err != nil {
		return 0, err
	}
	return len(refs), nil
}
