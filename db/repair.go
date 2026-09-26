package db

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
)

func jsonUnmarshalMeta(b []byte, m *metaPayload) error { return json.Unmarshal(b, m) }
func jsonUnmarshalDoc(b []byte, d *Document) error     { return json.Unmarshal(b, d) }

// RepairReport resume lo que cambió Repair al reescribir el archivo del almacén.
type RepairReport struct {
	Path            string `json:"path"`
	Collections     int    `json:"collections"`
	DocsKept        int    `json:"docs_kept"`
	DocsDropped     int    `json:"docs_dropped"`
	IndexesRebuilt  int    `json:"indexes_rebuilt"`
	UniqueConflicts int    `json:"unique_conflicts"`
	Resaved         bool   `json:"resaved"`
}

// Repair intenta recuperar un almacén semánticamente corrupto en path y reescribe un
// archivo limpio (reemplazo atómico). Puede arreglar:
//   - documentos con _id no string (se descartan); si falta el _id se genera un ULID
//   - documentos que superan el límite de 1MB (se descartan)
//   - claves _id duplicadas en el archivo (se conserva el primer id ordenado)
//   - conflictos de índice único (se conserva el _id ordenado menor y se descartan los posteriores)
//   - estado de índices obsoleto o roto (los índices se reconstruyen desde los documentos supervivientes)
//
// No puede arreglar corrupción criptográfica (magic/HMAC/GCM/JSON incorrectos): esos casos
// devuelven ErrCorrupt — restaura una Snapshot en su lugar. Toma el mismo flock que
// OpenWithLock; devuelve ErrAlreadyOpen si otro proceso tiene el archivo.
func Repair(path string, opts Options) (*RepairReport, error) {
	if len(opts.MasterKey) == 0 {
		return nil, errNoMaster
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	machine := opts.machineID()
	if len(data) < headerSize {
		return nil, ErrCorrupt
	}
	h, err := parseHeader(data)
	if err != nil {
		return nil, err
	}

	var (
		dek        []byte
		effMachine []byte
		t, m, p    uint32
		fd         *fileData
		kdfSalt    [16]byte
		createdAt  uint64
		schemaHint uint64
	)

	if h.FormatVer == 1 {
		h2, dek2, eff2, t2, m2, p2, fd2, err := decryptFileBytes(data, opts, machine)
		if err != nil {
			return nil, err
		}
		dek, effMachine, t, m, p, fd = dek2, eff2, t2, m2, p2, fd2
		copy(kdfSalt[:], h2.KDFSalt[:])
		createdAt = h2.CreatedAt
		schemaHint = h2.SchemaVersion
	} else {
		_, _, dek2, eff2, t2, m2, p2, err := authHeader(data, opts, machine)
		if err != nil {
			return nil, err
		}
		dek, effMachine, t, m, p = dek2, eff2, t2, m2, p2
		copy(kdfSalt[:], h.KDFSalt[:])
		createdAt = h.CreatedAt
		schemaHint = h.SchemaVersion
		// Escaneo tolerante: descifrar todos los registros DOC sin tener en cuenta COMMIT o sospechas.
		fd, err = loadV2FileDataTolerant(data, dek, schemaHint)
		if err != nil {
			return nil, err
		}
	}

	lk, lerr := acquireLock(path, opts.lockFileName())
	if lerr != nil {
		return nil, lerr
	}
	defer func() {
		_ = lk.Unlock()
		_ = lk.Close()
	}()

	s := New()
	s.path = path
	s.opts = opts
	s.machine = effMachine
	s.dek = dek
	s.created = createdAt
	s.schemaVersion = fd.Meta.SchemaVersion
	if s.schemaVersion == 0 {
		s.schemaVersion = schemaHint
	}
	copy(s.kdfSalt[:], kdfSalt[:])
	s.kdfTime, s.kdfMem, s.kdfPar = t, m, p
	s.dirty = true
	s.sawFile = true
	s.v1Migration = true // forzar una reescritura completa como v2 limpio

	rep := s.loadFileDataRepair(fd)
	rep.Path = path

	if err := s.Flush(); err != nil {
		return nil, err
	}
	rep.Resaved = true
	return rep, nil
}

// loadV2FileDataTolerant descifra todos los registros DOC/META de un archivo v2
// (ignorando la frontera COMMIT) y los lleva a fileData para Repair.
func loadV2FileDataTolerant(data []byte, dek []byte, schemaHint uint64) (*fileData, error) {
	fd := &fileData{
		Meta:        fileMeta{App: "mlstoredb", SchemaVersion: schemaHint},
		Collections: map[string]*fileColl{},
	}
	off := headerSize
	for off+recHdrSize <= len(data) {
		typ, flags, idBlob, ct, total, err := parseRecordAt(data, off)
		if err != nil {
			if errors.Is(err, errRecTorn) {
				break
			}
			return nil, err
		}
		var nonce [12]byte
		copy(nonce[:], data[off+8:off+20])
		switch typ {
		case recMETA:
			plain, err := decodePayload(dek, flags, nonce[:], ct)
			if err != nil {
				return nil, err
			}
			var mp metaPayload
			if err := jsonUnmarshalMeta(plain, &mp); err != nil {
				return nil, ErrCorrupt
			}
			fd.Meta.SchemaVersion = mp.SchemaVersion
			for name, mc := range mp.Collections {
				fc, ok := fd.Collections[name]
				if !ok {
					fc = &fileColl{}
					fd.Collections[name] = fc
				}
				fc.Indexes = append([]IndexInfo{}, mc.Indexes...)
				fc.Sensitive = append([]string{}, mc.Sensitive...)
			}
		case recDOC:
			coll, _, err := decodeIDBlob(idBlob)
			if err != nil {
				return nil, err
			}
			plain, err := decodePayload(dek, flags, nonce[:], ct)
			if err != nil {
				return nil, err
			}
			var doc Document
			if err := jsonUnmarshalDoc(plain, &doc); err != nil {
				return nil, ErrCorrupt
			}
			fc, ok := fd.Collections[coll]
			if !ok {
				fc = &fileColl{}
				fd.Collections[coll] = fc
			}
			fc.Docs = append(fc.Docs, doc)
		}
		off += total
	}
	// orden estable de documentos para elegir duplicados/conflictos de forma determinista
	for _, fc := range fd.Collections {
		sort.SliceStable(fc.Docs, func(i, j int) bool {
			a, _ := fc.Docs[i]["_id"].(string)
			b, _ := fc.Docs[j]["_id"].(string)
			return a < b
		})
	}
	return fd, nil
}

// loadFileDataRepair reconstruye las colecciones de forma tolerante y rellena rep.
// Los documentos se cargan primero y los índices se reconstruyen después, para que los
// conflictos de único puedan descartar el documento posterior en lugar de abortar toda la apertura.
func (s *Store) loadFileDataRepair(fd *fileData) *RepairReport {
	rep := &RepairReport{}
	s.schemaVersion = fd.Meta.SchemaVersion
	names := make([]string, 0, len(fd.Collections))
	for name := range fd.Collections {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		fc := fd.Collections[name]
		if fc == nil {
			continue
		}
		c := &collection{
			entries:   map[string]*docEntry{},
			sensitive: append([]string{}, fc.Sensitive...),
		}

		// Paso 1: conservar los documentos legibles (orden de id creciente → elección determinista de duplicados).
		for _, doc := range fc.Docs {
			if doc == nil {
				rep.DocsDropped++
				continue
			}
			stored := clone(doc)
			id, err := docID(stored)
			if err != nil {
				rep.DocsDropped++
				continue
			}
			stored["_id"] = id
			if err := checkDocSize(stored); err != nil {
				rep.DocsDropped++
				continue
			}
			if _, exists := c.entries[id]; exists {
				rep.DocsDropped++
				continue
			}
			c.entries[id] = newDocEntry(stored)
		}

		// Orden estable para resolver conflictos de único (conservar el _id menor).
		ids := make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		// Paso 2: reconstruir los índices; si hay conflicto de único ⇒ descartar el documento posterior.
		for _, info := range fc.Indexes {
			if len(info.Fields) == 0 {
				continue
			}
			idx := newIndex(info.Fields, info.Unique)
			for _, id := range ids {
				e, ok := c.entries[id]
				if !ok {
					continue
				}
				doc := *e.docP.Load()
				if err := idx.addDoc(doc, id); err != nil {
					if errors.Is(err, ErrDuplicate) {
						delete(c.entries, id)
						rep.DocsDropped++
						if idx.Unique {
							rep.UniqueConflicts++
						}
						continue
					}
					delete(c.entries, id)
					rep.DocsDropped++
				}
			}
			c.indexes = append(c.indexes, idx)
			rep.IndexesRebuilt++
		}

		for _, e := range c.entries {
			s.markEntryResident(e)
		}
		s.collections[name] = c
		rep.Collections++
		rep.DocsKept += len(c.entries)
	}
	return rep
}
