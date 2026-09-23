package db

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
)

func jsonUnmarshalMeta(b []byte, m *metaPayload) error { return json.Unmarshal(b, m) }
func jsonUnmarshalDoc(b []byte, d *Document) error     { return json.Unmarshal(b, d) }

// RepairReport summarizes what Repair changed while rewriting a store file.
type RepairReport struct {
	Path            string `json:"path"`
	Collections     int    `json:"collections"`
	DocsKept        int    `json:"docs_kept"`
	DocsDropped     int    `json:"docs_dropped"`
	IndexesRebuilt  int    `json:"indexes_rebuilt"`
	UniqueConflicts int    `json:"unique_conflicts"`
	Resaved         bool   `json:"resaved"`
}

// Repair attempts to recover a semantically corrupt store at path and rewrites
// a clean file (atomic replace). It can fix:
//   - docs with non-string _id (dropped); missing _id gets an auto ULID
//   - docs over the 1MB limit (dropped)
//   - duplicate _id keys in the file (first sorted id kept)
//   - unique-index conflicts (first sorted _id kept, later docs dropped)
//   - stale/broken index state (indexes rebuilt from surviving docs)
//
// It cannot fix cryptographic corruption (bad magic/HMAC/GCM/JSON): those
// return ErrCorrupt — restore a Snapshot instead. Takes the same flock as
// OpenWithLock; returns ErrAlreadyOpen if another process holds the file.
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
		dek         []byte
		effMachine  []byte
		t, m, p     uint32
		fd          *fileData
		kdfSalt     [16]byte
		createdAt   uint64
		schemaHint  uint64
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
		// Tolerant scan: decrypt all DOC records regardless of COMMIT/suspect.
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
	s.v1Migration = true // force full rewrite as clean v2

	rep := s.loadFileDataRepair(fd)
	rep.Path = path

	if err := s.Flush(); err != nil {
		return nil, err
	}
	rep.Resaved = true
	return rep, nil
}

// loadV2FileDataTolerant decrypts every DOC/META record in a v2 file
// (ignoring COMMIT boundary) into fileData for Repair.
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
	// stable doc order for deterministic dup/conflict keep
	for _, fc := range fd.Collections {
		sort.SliceStable(fc.Docs, func(i, j int) bool {
			a, _ := fc.Docs[i]["_id"].(string)
			b, _ := fc.Docs[j]["_id"].(string)
			return a < b
		})
	}
	return fd, nil
}

// loadFileDataRepair rebuilds collections tolerantly and fills rep.
// Docs are loaded first; indexes are rebuilt after so unique conflicts can
// drop the later doc instead of aborting the whole open.
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
			entries:  map[string]*docEntry{},
			sensitive: append([]string{}, fc.Sensitive...),
		}

		// Pass 1: keep readable docs (sorted id order → deterministic dup keep).
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

		// Stable order for unique-conflict resolution (keep lowest _id).
		ids := make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		// Pass 2: rebuild indexes; unique conflict ⇒ drop later doc.
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
