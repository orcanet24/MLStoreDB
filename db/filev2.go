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

// Formato del log de registros M1 (formatVersion 2):
//
//	[152B de cabecera][registro]* y el último registro COMMIT es el punto de confirmación atómico.
//
// Estructura de un registro (recHdrSize = 24):
//
//	type u8 | flags u8 | idLen u16 | payloadLen u32 | nonce [12] | crc u32
//	id     [idLen]      — texto plano: u16be(collLen) || coll || docID
//	payload[payloadLen] — AES-256-GCM(DEK, nonce, plain|zlib)
//
// El CRC32 cubre hdr[0:20] || id || payload.

const (
	recHdrSize = 24

	recDOC    = byte(1)
	recMETA   = byte(2)
	recIDX    = byte(3)
	recDEL    = byte(4)
	recCOMMIT = byte(5)

	// flags por registro
	flagRecCompressed = byte(1 << 0)
	flagRecSuspect    = byte(1 << 1)
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// defaultCacheBytes: presupuesto residente de 256 MB cuando Options.CacheBytes es cero.
const defaultCacheBytes = int64(256) << 20

// cacheBudget resuelve el presupuesto residente en bytes. Negativo = sin límite.
func (o Options) cacheBudget() int64 {
	if o.CacheBytes == 0 {
		return defaultCacheBytes
	}
	if o.CacheBytes < 0 {
		return -1
	}
	return o.CacheBytes
}

// metaCollPayload son los metadatos por colección dentro del registro META.
type metaCollPayload struct {
	Indexes   []IndexInfo `json:"indexes"`
	Sensitive []string    `json:"sensitive,omitempty"`
}

// metaPayload es el texto plano de un registro META (esquema autoritativo para v2).
type metaPayload struct {
	App           string                     `json:"app"`
	SchemaVersion uint64                     `json:"schema_version"`
	Collections   map[string]metaCollPayload `json:"collections"`
}

// recOp es un registro preparado para el próximo flush.
type recOp struct {
	typ     byte
	coll    string
	id      string // docID o "" para META/IDX/COMMIT
	payload []byte
	suspect bool
	// idxFields identifica un registro IDX (coll + clave de fields+unique).
	idxFields []string
	idxUnique bool
	// outOff/outLen los rellena el escritor (offsets de la reescritura completa).
	outOff int64
	outLen int32
}

// flushState es una instantánea consistente del estado del almacén para escribir fuera del bloqueo.
type flushState struct {
	path          string
	clearDirty    bool
	gen           uint64
	writeSeq      uint64 // instantánea de s.writeSeq (marca duradera de contrapresión M3)
	dek           []byte
	machine       []byte
	salt          [16]byte
	kdfTime       uint32
	kdfMem        uint32
	kdfPar        uint32
	created       uint64
	schemaVersion uint64
	master        []byte

	// preparación de registros v2
	full bool // reescribir todo el archivo o añadir al final
	meta metaPayload
	ops  []recOp // orden: [DELs] [DOCs] [META] [IDXs] [COMMIT] en reescritura completa; incremental similar
	dels []delItem
	// contabilidad de entradas sucias para la limpieza posterior al flush
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

// buildRecord ensambla un registro completo en disco (hdr+id+payload).
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

// parseRecordAt valida el registro en data[off:]; devuelve los campos ya
// interpretados y el tamaño total del registro. flags contiene flagRecSuspect/Compressed.
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
	// El CRC cubre hdr[0:20] || id || ct — id/ct van DESPUÉS del campo crc, así que
	// no son contiguos con hdr[0:20]; se aplica el hash a los dos tramos.
	crc := crc32.New(crcTable)
	crc.Write(data[off : off+20])
	crc.Write(idBlob)
	crc.Write(ct)
	if want != crc.Sum32() {
		return 0, 0, nil, nil, 0, ErrCorrupt
	}
	return typ, flags, idBlob, ct, total, nil
}

// errRecTorn marca una cola incompleta (caída a mitad de escritura) — se autocorrige truncando.
var errRecTorn = errors.New("db: torn record tail")

// decodePayload descifra (y opcionalmente descomprime con zlib) la carga de un registro.
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

// openRead abre path para lecturas aleatorias.
func openRead(path string) (*os.File, error) {
	return os.Open(path)
}

// scanResult es el resultado de escanear un log de registros v2.
type scanResult struct {
	commitEnd int64 // offset final del último COMMIT (frontera de aplicación)
	truncated int64 // si es >0, el archivo debe truncarse a este tamaño
	suspect   bool
	meta      *metaPayload
	docs      []scannedDoc
	dels      map[string]struct{} // coll\x00id
	idxByColl map[string][]idxPayload
	metaSeen  bool
	idxSeen   map[string]bool // "coll\x00fields\x00unique"
}

type scannedDoc struct {
	coll    string
	id      string
	off     int64 // offset absoluto del registro
	length  int32
	flags   byte
	nonce   [12]byte
	payload []byte // slice del criptograma crudo (subslice de los datos del archivo — hay que copiarlo)
}

// scanV2 interpreta los registros de data (todos los bytes del archivo, cabecera incluida).
// Solo aplica los registros hasta el último COMMIT; detecta colas cortadas y registros sospechosos.
func scanV2(data []byte, dek []byte) (*scanResult, error) {
	res := &scanResult{
		dels:      map[string]struct{}{},
		idxByColl: map[string][]idxPayload{},
		idxSeen:   map[string]bool{},
	}
	off := int64(headerSize)
	var lastCommit int64 = -1
	// Primera pasada: recorrer todos los registros completos y anotar las fronteras COMMIT.
	type applied struct {
		typ    byte
		flags  byte
		idBlob []byte
		ct     []byte
		nonce  [12]byte
		recOff int64
		recLen int32
	}
	var recs []applied
	for off < int64(len(data)) {
		typ, flags, idBlob, ct, total, err := parseRecordAt(data, int(off))
		if errors.Is(err, errRecTorn) {
			// Los archivos duraderos se escriben de forma atómica hasta COMMIT (reescritura
			// completa) o se añaden después de un COMMIT existente. Un comienzo cortado sin
			// ningún COMMIT previo indica corrupción/truncamiento, no una cola recuperable.
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
		// Sin COMMIT: nada duradero (o primer flush cortado). Truncar a la cabecera.
		res.truncated = int64(headerSize)
		if res.truncated < int64(len(data)) && len(data) > headerSize {
			// mantener la petición de truncado
		}
		return res, nil
	}
	res.commitEnd = lastCommit
	// Segunda pasada: aplicar los registros con recOff < lastCommit.
	for _, r := range recs {
		if r.recOff+int64(r.recLen) > lastCommit {
			break
		}
		if r.flags&flagRecSuspect != 0 {
			res.suspect = true
		}
		switch r.typ {
		case recCOMMIT:
			// solo la frontera
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
			// copiar los bytes de la carga (data podría reutilizarse conceptualmente)
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
			// tipo desconocido: ignorar (compatibilidad futura dentro de la misma versión mayor)
		}
	}
	// Aplicar los DELs sobre los documentos (gana el borrado si la misma clave aparece
	// antes que el "del" en el log — bitcask: gana la última escritura según el orden de
	// escaneo; aquí se aplicó en orden, así que se reaplican los DEL que van después de un DOC).
	pos := map[string]int{}
	for i, d := range res.docs {
		pos[d.coll+"\x00"+d.id] = i
	}
	// Recorrer de nuevo el orden aplicado para intercalar DOC/DEL:
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

// eagerV2 reconstruye un fileData a partir de los registros escaneados y sus cargas
// descifradas, y aplica la semántica de loadFileData (ErrNoID / ErrDuplicate si hay sospechas).
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
	// El orden estable para elegir duplicados de forma determinista lo gestiona
	// loadFileData con un mapa; aquí se ordenan los documentos por id para ser deterministas.
	for _, fc := range fd.Collections {
		sort.SliceStable(fc.Docs, func(i, j int) bool {
			a, _ := fc.Docs[i]["_id"].(string)
			b, _ := fc.Docs[j]["_id"].(string)
			return a < b
		})
	}
	return fd, nil
}

// decodeDocRecord convierte un registro crudo completo (de loadEntry) en un Document.
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

// buildMeta toma una instantánea del esquema y de las definiciones de colecciones para el registro META.
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

// stageDocOp serializa el documento y construye un recOp DOC (marca sospechoso
// cuando el _id o la cobertura de índices parece incorrecta al escribir).
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

// prepareFlush toma una instantánea del estado del almacén bajo s.mu (bloqueo de escritura
// breve para los parámetros criptográficos iniciales y luego R/Lock para el contenido).
// Devuelve nil,nil cuando no hay nada que hacer.
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

	// Reescritura completa cuando el archivo aún no tiene un cuerpo v2 duradero, hay una
	// migración pendiente o Compact/Repair lo forzó. Las instantáneas a un destino nuevo son completas.
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
		s.v1Migration = false // se consume: este flush reescribe como v2
		s.noEvict.Store(false)
	}
	st.full = full

	st.meta = s.buildMetaLocked()

	// Preparar las operaciones bajo el mismo bloqueo (instantánea del contenido).
	st.dels = append([]delItem(nil), s.pendingDels...)
	if full {
		// Los DELs no tienen sentido en una reescritura (cuerpo nuevo); los documentos salen de entries.
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

		// Registros INDEX: serialización completa en cada flush (diseño M1).
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
				// Entrada fría sin registro (no debería ocurrir si está sucia)
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

	// Orden: DELs, DOCs, META, IDXs, COMMIT (los ensambla buildCommit).
	// Ahora mismo las operaciones van IDX y luego DOC intercalados por colección; abajo se reordenan.
	st.ops = orderOpsForCommit(st.dels, st.ops, st.meta, st.gen, st.dek)
	s.mu.Unlock()
	return st, nil
}

// orderOpsForCommit ordena las operaciones preparadas en el orden de escritura duradero y añade META+COMMIT.
func orderOpsForCommit(dels []delItem, ops []recOp, meta metaPayload, gen uint64, dek []byte) []recOp {
	var docOps, idxOps, metaOps, delOps []recOp
	for _, op := range ops {
		switch op.typ {
		case recDOC:
			docOps = append(docOps, op)
		case recIDX:
			idxOps = append(idxOps, op)
		case recMETA, recCOMMIT, recDEL:
			// todavía ninguno
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
	// gen no se usa en la carga; el COMMIT es texto plano vacío.
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

// encodeRecordBytes serializa una operación preparada a los bytes de disco.
func encodeRecordBytes(dek []byte, op recOp) ([]byte, error) {
	var idBlob []byte
	switch op.typ {
	case recDOC, recDEL:
		idBlob = encodeIDBlob(op.coll, op.id)
	case recIDX:
		// blob del id: solo coll (la carga lleva fields/unique); se mantiene coll para depurar
		idBlob = encodeIDBlob(op.coll, "")
	case recMETA, recCOMMIT:
		idBlob = nil
	default:
		idBlob = encodeIDBlob(op.coll, op.id)
	}
	return buildRecord(dek, op.typ, 0, idBlob, op.payload, op.suspect)
}

// buildHeaderV2 construye la cabecera en claro de un archivo v2.
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
		Flags:         0,             // compresión por registro; no hay flag para toda la carga
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

// writeState serializa y escribe st.path (log de registros v2). Sin bloqueo del almacén.
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

// openV2 carga un archivo existente con formatVersion>=2: escanea, quizá hace una carga
// "eager" y construye las entradas. El llamador ya autenticó la cabecera y desenvolvió la dek.
// data son todos los bytes del archivo.
func (s *Store) openV2(data []byte, h *header, dek []byte) error {
	res, err := scanV2(data, dek)
	if err != nil {
		return err
	}
	// Truncar la cola cortada/sin confirmar (autocorrección) — solo cuando somos dueños del
	// path y acabaremos reescribiendo; se hace ya para que el log termine en un COMMIT.
	if res.truncated > 0 && res.truncated < int64(len(data)) {
		if err := truncateFile(s.path, res.truncated); err != nil {
			return err
		}
	}

	suspect := res.suspect
	// Coherencia META vs IDX → reconstrucción "eager".
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
		// Todos los documentos residentes; solo se marca para reescritura si la sospecha o el
		// desajuste lo exigen. Un IDX sospechoso o ausente implica que el estado de índices en
		// disco no es fiable: se mantiene sucio para que el próximo Flush reescriba limpio
		// (también cubre las expectativas de Repair).
		s.dirty = true
		s.dirtyGen++
		s.v1Migration = true // forzar el camino de reescritura completa (se reutiliza el flag)
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

	// Camino normal: entradas frías + definiciones META + filas IDX.
	if res.meta != nil {
		s.schemaVersion = res.meta.SchemaVersion
	} else {
		s.schemaVersion = h.SchemaVersion
	}
	// colecciones desde meta ∪ docs
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

	// Adjuntar las cargas IDX a las colecciones (si hay desajuste estructural, la carga "eager" ya validó las definiciones).
	for coll, list := range res.idxByColl {
		if res.meta != nil {
			if _, ok := res.meta.Collections[coll]; !ok {
				continue // se eliminó tras un flush anterior (M8 DropCollection)
			}
		}
		c := getColl(coll)
		for _, p := range list {
			// Buscar el índice con la definición coincidente o crearlo.
			var target *index
			for _, idx := range c.indexes {
				if sameFields(idx.Fields, p.Fields) && idx.Unique == p.Unique {
					target = idx
					break
				}
			}
			if target == nil {
				// falta la definición en META — problema estructural → carga "eager"
				fd, eerr := s.eagerV2(res, dek)
				if eerr != nil {
					return eerr
				}
				// reiniciar el estado parcial
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

	// Definiciones de índice sin carga IDX → carga "eager" (ya se trata arriba con la comprobación idxSeen).

	// Entradas de documentos (frías). Condicionadas por META: los registros de
	// colecciones ausentes en el último META pertenecen a colecciones eliminadas (M8 DropCollection).
	for _, d := range res.docs {
		if res.meta != nil {
			if _, ok := res.meta.Collections[d.coll]; !ok {
				continue
			}
		}
		c := getColl(d.coll)
		e := newDocEntry(nil) // frío
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

// prepareV1Migration se llama desde el camino de Open v1 después de loadFileData.
func (s *Store) prepareV1Migration() {
	s.v1Migration = true
	s.noEvict.Store(true)
	s.dirty = true
	s.dirtyGen++
	// Todos los documentos de loadFileData están residentes; se les dan entradas válidas
	// (lo hace loadFileData). recOff se queda en -1 hasta la reescritura completa de la migración.
}

// Compact reescribe el archivo como un log v2 compacto (elimina bytes muertos / versiones antiguas).
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
