package db

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"time"
)

// M1 record-log format (formatVersion 2):
//
//	[152B header][record]* and the last COMMIT record is the atomic commit point.
//
// Record layout (recHdrSize = 24):
//
//	type u8 | flags u8 | idLen u16 | payloadLen u32 | nonce [12] | crc u32
//	id     [idLen]      — plaintext: u16be(collLen) || coll || docID
//	payload[payloadLen] — AES-256-GCM(DEK, nonce, plain|zlib)
//
// CRC32 covers hdr[0:20] || id || payload.

const (
	recHdrSize = 24

	recDOC    = byte(1)
	recMETA   = byte(2)
	recIDX    = byte(3)
	recDEL    = byte(4)
	recCOMMIT = byte(5)

	// per-record flags
	flagRecCompressed = byte(1 << 0)
	flagRecSuspect    = byte(1 << 1)
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// defaultCacheBytes: 256 MB resident budget when Options.CacheBytes is zero.
const defaultCacheBytes = int64(256) << 20

// cacheBudget resolves the resident byte budget. Negative = unlimited.
func (o Options) cacheBudget() int64 {
	if o.CacheBytes == 0 {
		return defaultCacheBytes
	}
	if o.CacheBytes < 0 {
		return -1
	}
	return o.CacheBytes
}

// metaCollPayload is per-collection metadata inside the META record.
type metaCollPayload struct {
	Indexes   []IndexInfo `json:"indexes"`
	Sensitive []string    `json:"sensitive,omitempty"`
}

// metaPayload is the plaintext of a META record (authoritative schema for v2).
type metaPayload struct {
	App           string                  `json:"app"`
	SchemaVersion uint64                  `json:"schema_version"`
	Collections   map[string]metaCollPayload `json:"collections"`
}

// recOp is one record staged for the next flush.
type recOp struct {
	typ     byte
	coll    string
	id      string // docID or "" for META/IDX/COMMIT
	payload []byte
	suspect bool
	// idxFields identifies an IDX record (coll + fields+unique key).
	idxFields []string
	idxUnique bool
	// outOff/outLen filled by the writer (full rewrite offsets).
	outOff int64
	outLen int32
}

// flushState is a consistent snapshot of store state for writing outside the lock.
type flushState struct {
	path          string
	clearDirty    bool
	gen           uint64
	writeSeq      uint64 // s.writeSeq snapshot (M3 backpressure durable mark)
	dek           []byte
	machine       []byte
	salt          [16]byte
	kdfTime       uint32
	kdfMem        uint32
	kdfPar        uint32
	created       uint64
	schemaVersion uint64
	master        []byte

	// v2 record staging
	full  bool // rewrite whole file vs append
	meta  metaPayload
	ops   []recOp // ordered: [DELs] [DOCs] [META] [IDXs] [COMMIT] for full; incremental similar
	dels  []delItem
	// dirty entry bookkeeping for post-flush clean-up
	flushedEntries []flushedRef
}

type flushedRef struct {
	coll string
	id   string
	gen  uint64
}

type delItem struct {
	coll string
	id   string
	gen  uint64
}

func (d delItem) key() string { return d.coll + "\x00" + d.id }

func encodeIDBlob(coll, id string) []byte {
	if len(coll) > 0xffff {
		coll = coll[:0xffff]
	}
	b := make([]byte, 2+len(coll)+len(id))
	binary.BigEndian.PutUint16(b[0:2], uint16(len(coll)))
	copy(b[2:], coll)
	copy(b[2+len(coll):], id)
	return b
}

func decodeIDBlob(b []byte) (coll, id string, err error) {
	if len(b) < 2 {
		return "", "", ErrCorrupt
	}
	cl := int(binary.BigEndian.Uint16(b[0:2]))
	if 2+cl > len(b) {
		return "", "", ErrCorrupt
	}
	return string(b[2 : 2+cl]), string(b[2+cl:]), nil
}

// buildRecord assembles one full on-disk record (hdr+id+payload).
func buildRecord(dek []byte, typ, flags byte, idBlob, plain []byte, suspect bool) ([]byte, error) {
	body := plain
	fl := flags
	if len(plain) > 0 {
		if c, compressed := compressIfNeeded(plain); compressed {
			body = c
			fl |= flagRecCompressed
		}
	}
	if suspect {
		fl |= flagRecSuspect
	}
	nonce, err := randomBytes(12)
	if err != nil {
		return nil, err
	}
	var ct []byte
	if len(body) > 0 {
		ct, err = sealGCM(dek, nonce, body, nil)
		if err != nil {
			return nil, err
		}
	}
	if len(idBlob) > 0xffff || len(ct) > 0xffffffff {
		return nil, errors.New("db: record too large")
	}
	hdr := make([]byte, recHdrSize)
	hdr[0] = typ
	hdr[1] = fl
	binary.LittleEndian.PutUint16(hdr[2:4], uint16(len(idBlob)))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(ct)))
	copy(hdr[8:20], nonce)
	crc := crc32.Checksum(append(append(append([]byte{}, hdr[0:20]...), idBlob...), ct...), crcTable)
	binary.LittleEndian.PutUint32(hdr[20:24], crc)
	out := make([]byte, 0, recHdrSize+len(idBlob)+len(ct))
	out = append(out, hdr...)
	out = append(out, idBlob...)
	out = append(out, ct...)
	return out, nil
}

// parseRecordAt validates the record at data[off:]; returns parsed fields
// and total record size. flags has flagRecSuspect/Compressed.
func parseRecordAt(data []byte, off int) (typ, flags byte, idBlob, ct []byte, total int, err error) {
	if off < 0 || off+recHdrSize > len(data) {
		return 0, 0, nil, nil, 0, errRecTorn
	}
	hdr := data[off : off+recHdrSize]
	typ = hdr[0]
	flags = hdr[1]
	idLen := int(binary.LittleEndian.Uint16(hdr[2:4]))
	payLen := int(binary.LittleEndian.Uint32(hdr[4:8]))
	total = recHdrSize + idLen + payLen
	if total < recHdrSize || off+total > len(data) {
		return 0, 0, nil, nil, 0, errRecTorn
	}
	idBlob = data[off+recHdrSize : off+recHdrSize+idLen]
	ct = data[off+recHdrSize+idLen : off+total]
	want := binary.LittleEndian.Uint32(hdr[20:24])
	// CRC covers hdr[0:20] || id || ct — id/ct sit AFTER the crc field, so
	// they are not contiguous with hdr[0:20]; hash the two spans.
	crc := crc32.New(crcTable)
	crc.Write(data[off : off+20])
	crc.Write(idBlob)
	crc.Write(ct)
	if want != crc.Sum32() {
		return 0, 0, nil, nil, 0, ErrCorrupt
	}
	return typ, flags, idBlob, ct, total, nil
}

// errRecTorn marks an incomplete tail (crash mid-write) — self-heal by truncate.
var errRecTorn = errors.New("db: torn record tail")

// decodePayload decrypts (+ optional zlib) a record payload.
func decodePayload(dek []byte, flags byte, nonce, ct []byte) ([]byte, error) {
	if len(ct) == 0 {
		return nil, nil
	}
	plain, err := openGCM(dek, nonce, ct, nil)
	if err != nil {
		return nil, ErrCorrupt
	}
	if flags&flagRecCompressed != 0 {
		return zlibDecompress(plain)
	}
	return plain, nil
}

// openRead opens path for random reads.
func openRead(path string) (*os.File, error) {
	return os.Open(path)
}

// scanResult is the outcome of scanning a v2 record log.
type scanResult struct {
	commitEnd   int64 // end offset of last COMMIT (apply boundary)
	truncated   int64 // if >0, file should be truncated to this size
	suspect     bool
	meta        *metaPayload
	docs        []scannedDoc
	dels        map[string]struct{} // coll\x00id
	idxByColl   map[string][]idxPayload
	metaSeen    bool
	idxSeen     map[string]bool // "coll\x00fields\x00unique"
}

type scannedDoc struct {
	coll    string
	id      string
	off     int64 // absolute record offset
	length  int32
	flags   byte
	nonce   [12]byte
	payload []byte // raw ciphertext slice (subslice of file data — copy)
}

// scanV2 parses records from data (full file bytes including header).
// Applies only records up to the last COMMIT; detects torn tails and suspects.
func scanV2(data []byte, dek []byte) (*scanResult, error) {
	res := &scanResult{
		dels:      map[string]struct{}{},
		idxByColl: map[string][]idxPayload{},
		idxSeen:   map[string]bool{},
	}
	off := int64(headerSize)
	var lastCommit int64 = -1
	// First pass: walk all complete records, note COMMIT boundaries.
	type applied struct {
		typ     byte
		flags   byte
		idBlob  []byte
		ct      []byte
		nonce   [12]byte
		recOff  int64
		recLen  int32
	}
	var recs []applied
	for off < int64(len(data)) {
		typ, flags, idBlob, ct, total, err := parseRecordAt(data, int(off))
		if errors.Is(err, errRecTorn) {
			// Durable files are written atomically through COMMIT (full rewrite)
			// or appended after an existing COMMIT. A torn head with no prior
			// COMMIT means corruption/truncation, not a recoverable append tear.
			if lastCommit < 0 && off == int64(headerSize) {
				return nil, ErrCorrupt
			}
			res.truncated = off
			break
		}
		if err != nil {
			return nil, err
		}
		var nonce [12]byte
		copy(nonce[:], data[int(off)+8:int(off)+20])
		recs = append(recs, applied{
			typ: typ, flags: flags, idBlob: idBlob, ct: ct,
			nonce: nonce, recOff: off, recLen: int32(total),
		})
		if typ == recCOMMIT {
			lastCommit = off + int64(total)
		}
		off += int64(total)
	}
	if lastCommit < 0 {
		// No COMMIT: nothing durable (or torn first flush). Truncate to header.
		res.truncated = int64(headerSize)
		if res.truncated < int64(len(data)) && len(data) > headerSize {
			// keep truncation request
		}
		return res, nil
	}
	res.commitEnd = lastCommit
	// Second pass: apply records with recOff < lastCommit.
	for _, r := range recs {
		if r.recOff+int64(r.recLen) > lastCommit {
			break
		}
		if r.flags&flagRecSuspect != 0 {
			res.suspect = true
		}
		switch r.typ {
		case recCOMMIT:
			// boundary only
		case recMETA:
			plain, err := decodePayload(dek, r.flags, r.nonce[:], r.ct)
			if err != nil {
				return nil, err
			}
			var m metaPayload
			if err := json.Unmarshal(plain, &m); err != nil {
				return nil, ErrCorrupt
			}
			res.meta = &m
			res.metaSeen = true
		case recIDX:
			plain, err := decodePayload(dek, r.flags, r.nonce[:], r.ct)
			if err != nil {
				return nil, err
			}
			var p idxPayload
			if err := json.Unmarshal(plain, &p); err != nil {
				return nil, ErrCorrupt
			}
			coll, id, err := decodeIDBlob(r.idBlob)
			if err != nil {
				return nil, err
			}
			_ = id
			if p.Coll == "" {
				p.Coll = coll
			}
			res.idxByColl[p.Coll] = append(res.idxByColl[p.Coll], p)
			res.idxSeen[idxKey(p.Coll, p.Fields, p.Unique)] = true
		case recDOC:
			coll, id, err := decodeIDBlob(r.idBlob)
			if err != nil {
				return nil, err
			}
			// copy payload bytes (data may be reused conceptually)
			ctCopy := append([]byte(nil), r.ct...)
			res.docs = append(res.docs, scannedDoc{
				coll: coll, id: id, off: r.recOff, length: r.recLen,
				flags: r.flags, nonce: r.nonce, payload: ctCopy,
			})
		case recDEL:
			coll, id, err := decodeIDBlob(r.idBlob)
			if err != nil {
				return nil, err
			}
			res.dels[coll+"\x00"+id] = struct{}{}
		default:
			// unknown type: ignore (forward compat within same major)
		}
	}
	// Apply DELs over docs (delete wins if same key appears before del in log —
	// bitcask: last write wins by scan order; we applied in order, so re-apply dels
	// that come after a doc in recs order).
	pos := map[string]int{}
	for i, d := range res.docs {
		pos[d.coll+"\x00"+d.id] = i
	}
	// Re-walk applied order for DOC/DEL interleaving:
	live := make([]scannedDoc, 0, len(res.docs))
	alive := map[string]bool{}
	deleted := map[string]bool{}
	for _, r := range recs {
		if r.recOff+int64(r.recLen) > lastCommit {
			break
		}
		coll, id, err := decodeIDBlob(r.idBlob)
		if err != nil && (r.typ == recDOC || r.typ == recDEL) {
			return nil, err
		}
		k := coll + "\x00" + id
		switch r.typ {
		case recDOC:
			deleted[k] = false
			alive[k] = true
		case recDEL:
			deleted[k] = true
			alive[k] = false
		}
	}
	for _, d := range res.docs {
		k := d.coll + "\x00" + d.id
		if !deleted[k] && alive[k] {
			live = append(live, d)
		}
	}
	res.docs = live
	return res, nil
}

func idxKey(coll string, fields []string, unique bool) string {
	return coll + "\x00" + fmt.Sprint(fields) + "\x00" + fmt.Sprintf("%v", unique)
}

// eagerV2 rebuilds a fileData from scanned records + decrypted payloads and
// runs loadFileData semantics (ErrNoID / ErrDuplicate on suspect files).
func (s *Store) eagerV2(res *scanResult, dek []byte) (*fileData, error) {
	fd := &fileData{
		Meta:        fileMeta{App: "mlstoredb"},
		Collections: map[string]*fileColl{},
	}
	if res.meta != nil {
		fd.Meta.SchemaVersion = res.meta.SchemaVersion
		for name, mc := range res.meta.Collections {
			fc := &fileColl{
				Indexes:   append([]IndexInfo{}, mc.Indexes...),
				Sensitive: append([]string{}, mc.Sensitive...),
				Docs:      []Document{},
			}
			fd.Collections[name] = fc
		}
	}
	ensure := func(name string) *fileColl {
		fc, ok := fd.Collections[name]
		if !ok {
			fc = &fileColl{Docs: []Document{}}
			fd.Collections[name] = fc
		}
		return fc
	}
	for _, d := range res.docs {
		plain, err := decodePayload(dek, d.flags, d.nonce[:], d.payload)
		if err != nil {
			return nil, err
		}
		var doc Document
		if err := json.Unmarshal(plain, &doc); err != nil {
			return nil, ErrCorrupt
		}
		fc := ensure(d.coll)
		fc.Docs = append(fc.Docs, doc)
	}
	// stable order for deterministic dup-keep is handled by loadFileData via map;
	// sort docs by id for determinism.
	for _, fc := range fd.Collections {
		sort.SliceStable(fc.Docs, func(i, j int) bool {
			a, _ := fc.Docs[i]["_id"].(string)
			b, _ := fc.Docs[j]["_id"].(string)
			return a < b
		})
	}
	return fd, nil
}

// decodeDocRecord parses a full raw record (from loadEntry) into a Document.
func (s *Store) decodeDocRecord(raw []byte) (Document, error) {
	typ, flags, _, ct, total, err := parseRecordAt(raw, 0)
	if err != nil || typ != recDOC || total != len(raw) {
		return nil, ErrCorrupt
	}
	var nonce [12]byte
	copy(nonce[:], raw[8:20])
	plain, err := decodePayload(s.dek, flags, nonce[:], ct)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(plain, &doc); err != nil {
		return nil, ErrCorrupt
	}
	return doc, nil
}

// buildMeta snapshots schema + collection defs for the META record.
func (s *Store) buildMetaLocked() metaPayload {
	m := metaPayload{
		App:           "mlstoredb",
		SchemaVersion: s.schemaVersion,
		Collections:   map[string]metaCollPayload{},
	}
	for name, c := range s.collections {
		mc := metaCollPayload{
			Indexes:   make([]IndexInfo, 0, len(c.indexes)),
			Sensitive: append([]string{}, c.sensitive...),
		}
		for _, idx := range c.indexes {
			mc.Indexes = append(mc.Indexes, idx.info())
		}
		m.Collections[name] = mc
	}
	return m
}

// stageDocOp marshals doc and builds a DOC recOp (sets suspect when _id or
// index coverage looks wrong at write time).
func stageDocOp(dek []byte, coll string, id string, doc Document, c *collection) (recOp, error) {
	plain, err := json.Marshal(doc)
	if err != nil {
		return recOp{}, err
	}
	suspect := false
	v, ok := doc["_id"]
	if !ok {
		suspect = true
	} else if sid, ok2 := v.(string); !ok2 || sid == "" || sid != id {
		suspect = true
	} else {
		for _, idx := range c.indexes {
			if !idx.covers(doc, id) {
				suspect = true
				break
			}
		}
	}
	return recOp{
		typ:     recDOC,
		coll:    coll,
		id:      id,
		payload: plain,
		suspect: suspect,
	}, nil
}

// prepareFlush snapshots store state under s.mu (brief write lock for crypto
// first-time params, then R/Lock for content). Returns nil,nil when no-op.
func (s *Store) prepareFlush(path string, clearDirty bool) (*flushState, error) {
	if path == "" {
		return nil, nil
	}
	s.mu.Lock()
	if s.closed && clearDirty {
		s.mu.Unlock()
		return nil, nil
	}
	if clearDirty && !s.dirty && !s.compactForce {
		s.mu.Unlock()
		return nil, nil
	}
	if len(s.opts.MasterKey) == 0 {
		s.mu.Unlock()
		return nil, errors.New("db: no MasterKey for Flush")
	}
	if s.kdfTime == 0 {
		salt, err := randomBytes(16)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		copy(s.kdfSalt[:], salt)
		if s.opts.LightKDF {
			s.kdfTime, s.kdfMem, s.kdfPar = lightKDFTime, lightKDFMem, lightKDFPar
		} else {
			s.kdfTime, s.kdfMem, s.kdfPar = defaultKDFTime, defaultKDFMem, defaultKDFPar
		}
	}
	if s.dek == nil {
		dek, err := randomBytes(32)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.dek = dek
	}
	if s.created == 0 {
		s.created = uint64(time.Now().Unix())
	}
	if s.machine == nil {
		s.machine = s.opts.machineID()
	}

	st := &flushState{
		path:          path,
		clearDirty:    clearDirty,
		gen:           s.dirtyGen,
		writeSeq:      s.writeSeq,
		dek:           append([]byte(nil), s.dek...),
		machine:       append([]byte(nil), s.machine...),
		salt:          s.kdfSalt,
		kdfTime:       s.kdfTime,
		kdfMem:        s.kdfMem,
		kdfPar:        s.kdfPar,
		created:       s.created,
		schemaVersion: s.schemaVersion,
		master:        append([]byte(nil), s.opts.MasterKey...),
	}

	// Full rewrite when the file has no durable v2 body yet, a migration is
	// pending, or Compact/Repair forced it. Snapshots to a new dest are full.
	isSnapshotDest := clearDirty == false && st.path != s.path
	wantCompact := s.compactForce
	full := !s.sawFile || s.v1Migration || wantCompact || isSnapshotDest || s.path == ""
	if clearDirty {
		s.flushInProgress = true
		if wantCompact {
			s.compactForce = false
		}
	}
	if full && clearDirty {
		s.v1Migration = false // consume: this flush rewrites as v2
		s.noEvict.Store(false)
	}
	st.full = full

	st.meta = s.buildMetaLocked()

	// Stage ops under the same lock (content snapshot).
	st.dels = append([]delItem(nil), s.pendingDels...)
	if full {
		// DELs are meaningless on rewrite (fresh body); docs come from entries.
		st.dels = nil
	}

	colls := make([]string, 0, len(s.collections))
	for name := range s.collections {
		colls = append(colls, name)
	}
	sort.Strings(colls)

	for _, name := range colls {
		c := s.collections[name]
		ids := make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		// INDEX records: full serialize each flush (design M1).
		for _, idx := range c.indexes {
			p := idx.serialize()
			p.Coll = name
			jp, err := json.Marshal(p)
			if err != nil {
				s.flushInProgress = false
				s.mu.Unlock()
				if s.bpCond != nil {
					s.bpCond.Broadcast()
				}
				return nil, err
			}
			st.ops = append(st.ops, recOp{
				typ:       recIDX,
				coll:      name,
				id:        idxKey(name, p.Fields, p.Unique),
				payload:   jp,
				idxFields: p.Fields,
				idxUnique: p.Unique,
			})
		}

		for _, id := range ids {
			e := c.entries[id]
			if !full && !e.dirty.Load() {
				continue
			}
			doc, err := s.loadEntry(e)
			if err != nil {
				// Cold entry with no record (shouldn't happen when dirty)
				if errors.Is(err, ErrNotFound) {
					continue
				}
				s.flushInProgress = false
				s.mu.Unlock()
				if s.bpCond != nil {
					s.bpCond.Broadcast()
				}
				return nil, err
			}
			op, err := stageDocOp(st.dek, name, id, doc, c)
			if err != nil {
				s.flushInProgress = false
				s.mu.Unlock()
				if s.bpCond != nil {
					s.bpCond.Broadcast()
				}
				return nil, err
			}
			st.ops = append(st.ops, op)
			st.flushedEntries = append(st.flushedEntries, flushedRef{
				coll: name, id: id, gen: e.mutGen,
			})
		}
	}

	// Order: DELs, DOCs, META, IDXs, COMMIT (buildCommit assembles).
	// Currently ops are IDX-then-DOC interleaved per coll; rewrite order below.
	st.ops = orderOpsForCommit(st.dels, st.ops, st.meta, st.gen, st.dek)
	s.mu.Unlock()
	return st, nil
}

// orderOpsForCommit sorts staged ops into durable write order and appends META+COMMIT.
func orderOpsForCommit(dels []delItem, ops []recOp, meta metaPayload, gen uint64, dek []byte) []recOp {
	var docOps, idxOps, metaOps, delOps []recOp
	for _, op := range ops {
		switch op.typ {
		case recDOC:
			docOps = append(docOps, op)
		case recIDX:
			idxOps = append(idxOps, op)
		case recMETA, recCOMMIT, recDEL:
			// none yet
		}
	}
	for _, d := range dels {
		delOps = append(delOps, recOp{
			typ:     recDEL,
			coll:    d.coll,
			id:      d.id,
			payload: nil,
		})
	}
	mj, err := json.Marshal(meta)
	if err == nil {
		metaOps = append(metaOps, recOp{typ: recMETA, payload: mj})
	}
	commit := recOp{typ: recCOMMIT, payload: nil}
	// gen unused in payload; COMMIT is empty plaintext.
	_ = gen
	_ = dek
	out := make([]recOp, 0, len(delOps)+len(docOps)+len(metaOps)+len(idxOps)+1)
	out = append(out, delOps...)
	out = append(out, docOps...)
	out = append(out, metaOps...)
	out = append(out, idxOps...)
	out = append(out, commit)
	return out
}

// encodeRecordBytes serializes one staged op to disk bytes.
func encodeRecordBytes(dek []byte, op recOp) ([]byte, error) {
	var idBlob []byte
	switch op.typ {
	case recDOC, recDEL:
		idBlob = encodeIDBlob(op.coll, op.id)
	case recIDX:
		// id blob: coll only (payload carries fields/unique); keep coll for debug
		idBlob = encodeIDBlob(op.coll, "")
	case recMETA, recCOMMIT:
		idBlob = nil
	default:
		idBlob = encodeIDBlob(op.coll, op.id)
	}
	return buildRecord(dek, op.typ, 0, idBlob, op.payload, op.suspect)
}

// buildHeaderV2 constructs the cleartext header for a v2 file.
func buildHeaderV2(st *flushState) (*header, error) {
	kek := deriveKEK(st.master, st.machine, st.salt[:], st.kdfTime, st.kdfMem, st.kdfPar)
	wrapNonce := st.salt[:12]
	wrapped, err := sealGCM(kek, wrapNonce, st.dek, nil)
	if err != nil {
		return nil, err
	}
	if len(wrapped) != 48 {
		return nil, fmt.Errorf("db: unexpected wrapped DEK size %d", len(wrapped))
	}
	nonceBase, err := randomBytes(12)
	if err != nil {
		return nil, err
	}
	h := &header{
		FormatVer:     formatVersion, // 2
		Flags:         0,             // per-record compression; no whole-payload flag
		SchemaVersion: st.schemaVersion,
		CreatedAt:     st.created,
		UpdatedAt:     uint64(time.Now().Unix()),
		KDFTime:       st.kdfTime,
		KDFMemKiB:     st.kdfMem,
		KDFPar:        st.kdfPar,
	}
	copy(h.KDFSalt[:], st.salt[:])
	copy(h.NonceBase[:], nonceBase)
	copy(h.DEKWrapped[:], wrapped)
	h.setHMAC(kek)
	return h, nil
}

// writeState serializes and writes st.path (v2 record log). No store lock.
func writeState(st *flushState) error {
	if st == nil {
		return nil
	}
	if st.full {
		return writeFullV2(st)
	}
	return appendV2(st)
}

func writeFullV2(st *flushState) error {
	h, err := buildHeaderV2(st)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Grow(headerSize + 4096)
	buf.Write(h.marshalBody())
	buf.Write(h.HeaderHMAC[:])
	off := int64(headerSize)
	for i := range st.ops {
		op := &st.ops[i]
		b, err := encodeRecordBytes(st.dek, *op)
		if err != nil {
			return err
		}
		op.outOff = off
		op.outLen = int32(len(b))
		buf.Write(b)
		off += int64(len(b))
	}
	return atomicReplace(st.path, buf.Bytes())
}

func appendV2(st *flushState) error {
	f, err := os.OpenFile(st.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return writeFullV2(st)
		}
		return err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	off := end
	var buf bytes.Buffer
	for i := range st.ops {
		op := &st.ops[i]
		b, err := encodeRecordBytes(st.dek, *op)
		if err != nil {
			return err
		}
		op.outOff = off
		op.outLen = int32(len(b))
		buf.Write(b)
		off += int64(len(b))
	}
	if buf.Len() == 0 {
		return nil
	}
	_, err = f.Write(buf.Bytes())
	return err
}

// openV2 loads an existing formatVersion≥2 file: scan, maybe eager, build entries.
// Caller has authenticated header and unwrapped dek. data is the full file.
func (s *Store) openV2(data []byte, h *header, dek []byte) error {
	res, err := scanV2(data, dek)
	if err != nil {
		return err
	}
	// Truncate torn/uncommitted tail (self-heal) — only when we own the path
	// and will rewrite eventually; do it eagerly so the log ends at COMMIT.
	if res.truncated > 0 && res.truncated < int64(len(data)) {
		if err := truncateFile(s.path, res.truncated); err != nil {
			return err
		}
	}

	suspect := res.suspect
	// META vs IDX consistency → eager rebuild.
	needEager := suspect
	if res.meta != nil {
		for name, mc := range res.meta.Collections {
			for _, info := range mc.Indexes {
				if !res.idxSeen[idxKey(name, info.Fields, info.Unique)] {
					needEager = true
				}
			}
		}
	}
	if needEager {
		fd, err := s.eagerV2(res, dek)
		if err != nil {
			return err
		}
		if err := s.loadFileData(fd); err != nil {
			return err
		}
		// All docs resident; mark for rewrite only if suspect/mismatch needs it.
		// Suspect or missing IDX means on-disk index state is untrustworthy —
		// keep dirty so next Flush rewrites clean (also covers repair expectations).
		s.dirty = true
		s.dirtyGen++
		s.v1Migration = true // force full rewrite path (reuse flag)
		s.noEvict.Store(true)
		s.sawFile = true
		copy(s.kdfSalt[:], h.KDFSalt[:])
		s.kdfTime, s.kdfMem, s.kdfPar = h.KDFTime, h.KDFMemKiB, h.KDFPar
		if s.kdfTime == 0 {
			s.kdfTime, s.kdfMem, s.kdfPar = defaultKDFTime, defaultKDFMem, defaultKDFPar
		}
		s.dek = dek
		s.created = h.CreatedAt
		if res.meta != nil {
			s.schemaVersion = res.meta.SchemaVersion
		} else {
			s.schemaVersion = h.SchemaVersion
		}
		return nil
	}

	// Normal path: cold entries + META defs + IDX rows.
	if res.meta != nil {
		s.schemaVersion = res.meta.SchemaVersion
	} else {
		s.schemaVersion = h.SchemaVersion
	}
	// collections from meta ∪ docs
	seenColl := map[string]*collection{}
	getColl := func(name string) *collection {
		c, ok := seenColl[name]
		if !ok {
			c = &collection{entries: map[string]*docEntry{}}
			if res.meta != nil {
				if mc, ok := res.meta.Collections[name]; ok {
					c.sensitive = append([]string{}, mc.Sensitive...)
					for _, info := range mc.Indexes {
						c.indexes = append(c.indexes, newIndex(info.Fields, info.Unique))
					}
				}
			}
			seenColl[name] = c
			s.collections[name] = c
		}
		return c
	}
	if res.meta != nil {
		for name := range res.meta.Collections {
			getColl(name)
		}
	}

	// Attach IDX payloads to collections (structure mismatch → eager already checked defs).
	for coll, list := range res.idxByColl {
		c := getColl(coll)
		for _, p := range list {
			// Find matching def index or create.
			var target *index
			for _, idx := range c.indexes {
				if sameFields(idx.Fields, p.Fields) && idx.Unique == p.Unique {
					target = idx
					break
				}
			}
			if target == nil {
				// def missing from META — structural issue → eager
				fd, eerr := s.eagerV2(res, dek)
				if eerr != nil {
					return eerr
				}
				// reset partial state
				s.collections = map[string]*collection{}
				if err := s.loadFileData(fd); err != nil {
					return err
				}
				s.dirty = true
				s.dirtyGen++
				s.v1Migration = true
				s.noEvict.Store(true)
				s.sawFile = true
				s.dek = dek
				s.created = h.CreatedAt
				copy(s.kdfSalt[:], h.KDFSalt[:])
				s.kdfTime, s.kdfMem, s.kdfPar = h.KDFTime, h.KDFMemKiB, h.KDFPar
				return nil
			}
			if err := target.loadRows(p); err != nil {
				if errors.Is(err, ErrDuplicate) || errors.Is(err, errIdxMismatch) {
					s.collections = map[string]*collection{}
					fd, eerr := s.eagerV2(res, dek)
					if eerr != nil {
						return eerr
					}
					if err := s.loadFileData(fd); err != nil {
						return err
					}
					s.dirty = true
					s.dirtyGen++
					s.v1Migration = true
					s.noEvict.Store(true)
					s.sawFile = true
					s.dek = dek
					s.created = h.CreatedAt
					copy(s.kdfSalt[:], h.KDFSalt[:])
					s.kdfTime, s.kdfMem, s.kdfPar = h.KDFTime, h.KDFMemKiB, h.KDFPar
					return nil
				}
				return err
			}
		}
	}

	// Index defs without IDX payload → eager (handled above via idxSeen check).

	// Doc entries (cold).
	for _, d := range res.docs {
		c := getColl(d.coll)
		e := newDocEntry(nil) // cold
		e.recOff.Store(d.off)
		e.recLen.Store(d.length)
		c.entries[d.id] = e
	}

	s.sawFile = true
	s.dirty = false
	s.dek = dek
	s.created = h.CreatedAt
	copy(s.kdfSalt[:], h.KDFSalt[:])
	s.kdfTime, s.kdfMem, s.kdfPar = h.KDFTime, h.KDFMemKiB, h.KDFPar
	if s.kdfTime == 0 {
		s.kdfTime, s.kdfMem, s.kdfPar = defaultKDFTime, defaultKDFMem, defaultKDFPar
	}
	s.pendingDels = nil
	return nil
}

func truncateFile(path string, size int64) error {
	return os.Truncate(path, size)
}

// prepareV1Migration is called from the v1 Open path after loadFileData.
func (s *Store) prepareV1Migration() {
	s.v1Migration = true
	s.noEvict.Store(true)
	s.dirty = true
	s.dirtyGen++
	// All docs from loadFileData are resident; give them valid entries (done
	// by loadFileData). recOff stays -1 until the migration full rewrite.
}

// Compact rewrites the file as a tight v2 log (drops dead bytes / old versions).
func (s *Store) Compact() error {
	if err := s.compact(); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) compact() error {
	s.mu.Lock()
	if s.path == "" || s.closed {
		s.mu.Unlock()
		return nil
	}
	s.compactForce = true
	if !s.dirty {
		s.dirty = true
		s.dirtyGen++
	}
	s.mu.Unlock()
	return s.Flush()
}
