package db

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// IndexInfo describe un índice sobre una colección (para ListIndexes / persistencia).
type IndexInfo struct {
	Fields []string `json:"fields"`
	Unique bool     `json:"unique"`
}

// ordEntry es una clave de índice distinta en orden ascendente: el valor crudo
// del primer campo (para compareOrdered) más la clave codificada completa (desempate / enlace al hash).
type ordEntry struct {
	val any
	key string
}

// index es un índice en memoria sobre una sola colección: un hash para igualdad
// más un slice "ord" ordenado de forma perezosa para búsquedas por rango/prefijo
// y para ordenar por índice.
// No único: clave → conjunto de _id. Único: clave → un único _id.
// mu protege entries/unique/ord (ensureOrd muta; Find puede tener s.mu.RLock).
type index struct {
	mu       sync.Mutex
	Fields   []string
	Unique   bool
	entries  map[string]map[string]struct{} // clave → ids (no único)
	unique   map[string]string              // clave → id (único)
	ord      []ordEntry                     // claves distintas ordenadas por (valor del primer campo, clave)
	ordDirty bool                           // requiere reordenar antes de la búsqueda binaria
	// matrix es la vista CSR plana de (ord × ids); solo deja de ser nil mientras se
	// sabe que coincide con entries/unique/ord (se fija en loadRows a partir de un CSR
	// válido y se limpia en cada mutación). Evita "map lookups" por clave + ordenación.
	matrix *csrMatrix
}

func newIndex(fields []string, unique bool) *index {
	idx := &index{
		Fields:   append([]string{}, fields...),
		Unique:   unique,
		ordDirty: true, // construcción masiva: agregar todo y ordenar una sola vez en la 1.ª búsqueda
	}
	if unique {
		idx.unique = map[string]string{}
	} else {
		idx.entries = map[string]map[string]struct{}{}
	}
	return idx
}

func (idx *index) info() IndexInfo {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return IndexInfo{Fields: append([]string{}, idx.Fields...), Unique: idx.Unique}
}

// typeRank ordena los tipos para las búsquedas por rango: nil < números < string < bool < otros.
// Entre tipos distintos nunca hay igualdad en rangos (estilo Mongo: sin orden entre tipos).
func typeRank(v any) int {
	if v == nil {
		return 0
	}
	if _, ok := toFloat(v); ok {
		return 1
	}
	if _, ok := v.(string); ok {
		return 2
	}
	if _, ok := v.(bool); ok {
		return 3
	}
	return 4
}

// compareOrdered define un orden total sobre los valores del primer campo del índice:
// primero el rango de tipo y luego compareValues dentro de los rangos numérico/string (bool: false < true).
func compareOrdered(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ra {
	case 0:
		return 0
	case 3:
		ab, _ := a.(bool)
		bb, _ := b.(bool)
		if ab == bb {
			return 0
		}
		if !ab {
			return -1
		}
		return 1
	case 4:
		return 0 // otros: sin orden entre valores; solo desempata la clave
	default:
		return compareValues(a, b)
	}
}

func ordLess(a, b ordEntry) bool {
	if c := compareOrdered(a.val, b.val); c != 0 {
		return c < 0
	}
	return a.key < b.key
}

// ensureOrd ordena ord si está "sucio". El llamador debe tener tomado idx.mu.
func (idx *index) ensureOrd() {
	if !idx.ordDirty {
		return
	}
	sort.Slice(idx.ord, func(i, j int) bool { return ordLess(idx.ord[i], idx.ord[j]) })
	idx.ordDirty = false
}

// addDoc indexa todas las filas de clave del docID. Si hay conflicto de único
// devuelve ErrDuplicate y no aplica nada de forma parcial (el llamador no debe
// haber borrado aún las entradas viejas, o debe hacer rollback).
func (idx *index) addDoc(doc Document, docID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.addDocLocked(doc, docID)
}

func (idx *index) addDocLocked(doc Document, docID string) error {
	rows := extractIndexRows(doc, idx.Fields)
	// Se preparan las claves primero para que los conflictos de "único" aborten limpio.
	keys := make([]string, 0, len(rows))
	vals := make([]any, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		k := encodeIndexKey(row)
		if seen[k] {
			continue // misma clave dos veces en un documento (elementos repetidos de un array)
		}
		seen[k] = true
		if idx.Unique {
			if owner, ok := idx.unique[k]; ok && owner != docID {
				return ErrDuplicate
			}
		}
		keys = append(keys, k)
		v := any(nil)
		if len(row) > 0 {
			v = row[0]
		}
		vals = append(vals, v)
	}
	for i, k := range keys {
		idx.addKey(k, vals[i], docID)
	}
	return nil
}

// addKey asume que idx.mu ya está tomado.
func (idx *index) addKey(k string, val any, docID string) {
	idx.matrix = nil
	if idx.Unique {
		if _, ok := idx.unique[k]; !ok {
			idx.ordAdd(ordEntry{val: val, key: k})
		}
		idx.unique[k] = docID
		return
	}
	set, ok := idx.entries[k]
	if !ok {
		set = map[string]struct{}{}
		idx.entries[k] = set
		idx.ordAdd(ordEntry{val: val, key: k})
	}
	set[docID] = struct{}{}
}

func (idx *index) ordAdd(e ordEntry) {
	// Camino masivo (sucio): agregar y ordenar de forma perezosa en la próxima búsqueda.
	// Camino estable (limpio): insertar con búsqueda binaria para mantener ord ordenado sin ordenar todo.
	if idx.ordDirty {
		idx.ord = append(idx.ord, e)
		return
	}
	i := sort.Search(len(idx.ord), func(i int) bool {
		return !ordLess(idx.ord[i], e)
	})
	idx.ord = append(idx.ord, ordEntry{})
	copy(idx.ord[i+1:], idx.ord[i:])
	idx.ord[i] = e
}

// removeDoc elimina todas las entradas de índice del docID usando los valores del documento dado.
func (idx *index) removeDoc(doc Document, docID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.removeDocLocked(doc, docID)
}

func (idx *index) removeDocLocked(doc Document, docID string) {
	rows := extractIndexRows(doc, idx.Fields)
	for _, row := range rows {
		v := any(nil)
		if len(row) > 0 {
			v = row[0]
		}
		idx.removeKey(encodeIndexKey(row), v, docID)
	}
}

func (idx *index) removeKey(k string, val any, docID string) {
	idx.matrix = nil
	if idx.Unique {
		if owner, ok := idx.unique[k]; ok && owner == docID {
			delete(idx.unique, k)
			idx.ordRemove(ordEntry{val: val, key: k})
		}
		return
	}
	if set, ok := idx.entries[k]; ok {
		delete(set, docID)
		if len(set) == 0 {
			delete(idx.entries, k)
			idx.ordRemove(ordEntry{val: val, key: k})
		}
	}
}

func (idx *index) ordRemove(e ordEntry) {
	if idx.ordDirty {
		// Sin ordenar: recorrido lineal (evita forzar una ordenación a mitad de lote).
		for j := range idx.ord {
			if idx.ord[j].key == e.key {
				idx.ord = append(idx.ord[:j], idx.ord[j+1:]...)
				return
			}
		}
		return
	}
	i := sort.Search(len(idx.ord), func(i int) bool {
		return !ordLess(idx.ord[i], e)
	})
	if i < len(idx.ord) && idx.ord[i].key == e.key {
		idx.ord = append(idx.ord[:i], idx.ord[i+1:]...)
		return
	}
	// Respaldo por desajuste de valor (no debería ocurrir): recorrido lineal por clave
	for j := range idx.ord {
		if idx.ord[j].key == e.key {
			idx.ord = append(idx.ord[:j], idx.ord[j+1:]...)
			return
		}
	}
}

// lookupIDs devuelve el conjunto de _id para valores de igualdad en el índice
// (clave completa cuando len(values)==len(Fields); el planificador MVP no usa coincidencias parciales).
func (idx *index) lookupIDs(values []any) map[string]struct{} {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := map[string]struct{}{}
	if idx.Unique {
		k := encodeIndexKey(values)
		if id, ok := idx.unique[k]; ok {
			out[id] = struct{}{}
		}
		return out
	}
	k := encodeIndexKey(values)
	for id := range idx.entries[k] {
		out[id] = struct{}{}
	}
	return out
}

// countKey devuelve cuántos documentos coinciden con la clave completa sin construir un conjunto de IDs.
func (idx *index) countKey(values []any) int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	k := encodeIndexKey(values)
	if idx.Unique {
		if _, ok := idx.unique[k]; ok {
			return 1
		}
		return 0
	}
	return len(idx.entries[k])
}

// covers indica si están presentes todas las filas de índice de doc/docID.
// Se usa al hacer flush para marcar el flag "sospechoso" cuando el estado del índice está obsoleto.
func (idx *index) covers(doc Document, docID string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	rows := extractIndexRows(doc, idx.Fields)
	for _, row := range rows {
		k := encodeIndexKey(row)
		if idx.Unique {
			if owner, ok := idx.unique[k]; !ok || owner != docID {
				return false
			}
			continue
		}
		if _, ok := idx.entries[k][docID]; !ok {
			return false
		}
	}
	return true
}

// seekPrefix devuelve los documentos cuya clave codificada empieza por el prefijo
// de values (values debe ser un subconjunto inicial de Fields). Con la longitud
// completa es una búsqueda exacta por clave; las parciales se acotan usando ord sobre el primer campo.
func (idx *index) seekPrefix(values []any) map[string]struct{} {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	out := map[string]struct{}{}
	if len(values) == 0 {
		return out
	}
	if len(values) == len(idx.Fields) {
		return idx.lookupIDsLocked(values)
	}
	prefix := encodeIndexKey(values) + "\x1f"
	target := values[0]
	rank := typeRank(target)
	if rank == 0 || rank == 3 || rank == 4 {
		// banda sin orden: recorrido completo de las claves distintas
		idx.collectPrefixScan(prefix, out)
		return out
	}
	idx.ensureOrd()
	start := sort.Search(len(idx.ord), func(i int) bool {
		e := idx.ord[i]
		r := typeRank(e.val)
		if r != rank {
			return r > rank
		}
		return compareOrdered(e.val, target) >= 0
	})
	end := sort.Search(len(idx.ord), func(i int) bool {
		e := idx.ord[i]
		r := typeRank(e.val)
		if r != rank {
			return r > rank
		}
		return compareOrdered(e.val, target) > 0
	})
	for i := start; i < end; i++ {
		k := idx.ord[i].key
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if idx.Unique {
			if id, ok := idx.unique[k]; ok {
				out[id] = struct{}{}
			}
			continue
		}
		for id := range idx.entries[k] {
			out[id] = struct{}{}
		}
	}
	return out
}

func (idx *index) lookupIDsLocked(values []any) map[string]struct{} {
	out := map[string]struct{}{}
	if idx.Unique {
		k := encodeIndexKey(values)
		if id, ok := idx.unique[k]; ok {
			out[id] = struct{}{}
		}
		return out
	}
	k := encodeIndexKey(values)
	for id := range idx.entries[k] {
		out[id] = struct{}{}
	}
	return out
}

func (idx *index) collectPrefixScan(prefix string, out map[string]struct{}) {
	if idx.Unique {
		for k, id := range idx.unique {
			if strings.HasPrefix(k, prefix) {
				out[id] = struct{}{}
			}
		}
		return
	}
	for k, set := range idx.entries {
		if strings.HasPrefix(k, prefix) {
			for id := range set {
				out[id] = struct{}{}
			}
		}
	}
}

// rangeBound describe una conjunción pura de $gt/$gte/$lt/$lte sobre un único campo.
type rangeBound struct {
	lo, hi       any
	hasLo, hasHi bool
	loIncl       bool
	hiIncl       bool
}

// seekRange devuelve los documentos cuyo primer campo del índice está en [lo,hi]
// (solo dentro de la misma banda de rango de tipo). Los IDs van en orden ascendente de ord.
func (idx *index) seekRange(rb rangeBound) []string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.ensureOrd()
	// Se determina el rango de tipo objetivo a partir de los límites.
	rank := -1
	if rb.hasLo && rb.hasHi {
		ra, rh := typeRank(rb.lo), typeRank(rb.hi)
		if ra != rh {
			return nil // ningún valor puede estar en ambas bandas
		}
		rank = ra
	} else if rb.hasLo {
		rank = typeRank(rb.lo)
	} else if rb.hasHi {
		rank = typeRank(rb.hi)
	} else {
		return nil
	}
	if rank == 0 || rank == 3 || rank == 4 {
		// nil / bool / otros no se ordenan con compareOp (comparableTypes es false)
		return nil
	}

	// Búsqueda binaria del borde inferior dentro de la banda [rank].
	start := sort.Search(len(idx.ord), func(i int) bool {
		e := idx.ord[i]
		r := typeRank(e.val)
		if r != rank {
			return r > rank
		}
		if !rb.hasLo {
			return true
		}
		c := compareOrdered(e.val, rb.lo)
		if rb.loIncl {
			return c >= 0
		}
		return c > 0
	})
	end := sort.Search(len(idx.ord), func(i int) bool {
		e := idx.ord[i]
		r := typeRank(e.val)
		if r != rank {
			return r > rank
		}
		if !rb.hasHi {
			return false // aún dentro de la banda; "end" avanza más allá abajo
		}
		c := compareOrdered(e.val, rb.hi)
		if rb.hiIncl {
			return c > 0
		}
		return c >= 0
	})
	// Con solo límite lo: "end" debe parar al final de la banda de rango.
	if !rb.hasHi {
		end = sort.Search(len(idx.ord), func(i int) bool {
			return typeRank(idx.ord[i].val) > rank
		})
	}
	// Con solo límite hi: "start" debe empezar al inicio de la banda de rango.
	if !rb.hasLo {
		start = sort.Search(len(idx.ord), func(i int) bool {
			r := typeRank(idx.ord[i].val)
			return r >= rank
		})
	}
	return idx.collectRange(start, end)
}

// collectRange recoge los ids de las filas de ord [start,end), prefiriendo la matrix
// CSR plana cuando está sincronizada (M2) y, si no, el camino por mapas de clave.
// El llamador tiene idx.mu (y ord ya debe estar ordenado si se usa la matrix).
func (idx *index) collectRange(start, end int) []string {
	if m := idx.matrix; m != nil && !idx.ordDirty && len(m.Indptr) == len(idx.ord)+1 {
		var out []string
		for i := start; i < end; i++ {
			lo, hi := int(m.Indptr[i]), int(m.Indptr[i+1])
			for _, col := range m.Indices[lo:hi] {
				out = append(out, m.Ids[col])
			}
		}
		return out
	}
	var out []string
	for i := start; i < end; i++ {
		k := idx.ord[i].key
		if idx.Unique {
			if id, ok := idx.unique[k]; ok {
				out = append(out, id)
			}
			continue
		}
		out = append(out, idx.idsSortedForKey(k)...)
	}
	return out
}

// idsSortedForKey devuelve los ids de documento de una clave en orden ascendente (determinista).
func (idx *index) idsSortedForKey(k string) []string {
	set := idx.entries[k]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// orderedIDs recorre todo el índice en orden (valor, _id) — ordenar por índice.
func (idx *index) orderedIDs() []string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.ensureOrd()
	if m := idx.matrix; m != nil && !idx.ordDirty && len(m.Indptr) == len(idx.ord)+1 {
		out := make([]string, 0, len(m.Ids))
		for _, col := range m.Indices {
			out = append(out, m.Ids[col])
		}
		return out
	}
	var out []string
	for _, e := range idx.ord {
		if idx.Unique {
			if id, ok := idx.unique[e.key]; ok {
				out = append(out, id)
			}
			continue
		}
		out = append(out, idx.idsSortedForKey(e.key)...)
	}
	return out
}

// idxRow es una fila de clave de índice persistida (M1: serialización completa en cada flush).
type idxRow struct {
	K   string   `json:"k"`
	V   any      `json:"v,omitempty"`
	IDs []string `json:"ids"`
}

// csrMatrix es la vista CSR plana (M2) de un índice: la fila i ↔ ord[i],
// Indices[Indptr[i]:Indptr[i+1]] son ordinales de columna dentro de Ids (diccionario de docID).
// Checksum es CRC32-C sobre keys|indptr|indices|ids para validar al cargar.
type csrMatrix struct {
	Keys     []string `json:"keys"`   // claves distintas en orden de ord (etiquetas de fila)
	Vals     []any    `json:"vals"`   // valores del primer campo (datos de fila para rangos)
	Indptr   []int32  `json:"indptr"` // len = len(Keys)+1
	Indices  []int32  `json:"indices"`
	Ids      []string `json:"ids"` // diccionario de columnas: docID
	Checksum uint32   `json:"csum"`
}

// idxPayload es el texto plano de un registro IDX.
type idxPayload struct {
	Coll   string     `json:"c"`
	Fields []string   `json:"f"`
	Unique bool       `json:"u"`
	Rows   []idxRow   `json:"rows"`
	CSR    *csrMatrix `json:"csr,omitempty"` // M2: camino de carga principal
}

// serialize toma una instantánea de las filas del índice en orden de ord para
// persistirlas y embebe la matrix CSR (M2). Rows queda como respaldo si falla el checksum del CSR.
func (idx *index) serialize() idxPayload {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.ensureOrd()
	p := idxPayload{
		Coll:   "",
		Fields: append([]string{}, idx.Fields...),
		Unique: idx.Unique,
		Rows:   make([]idxRow, 0, len(idx.ord)),
	}
	m := &csrMatrix{
		Keys:    make([]string, 0, len(idx.ord)),
		Vals:    make([]any, 0, len(idx.ord)),
		Indptr:  make([]int32, 1, len(idx.ord)+1),
		Indices: []int32{},
		Ids:     []string{},
	}
	idCol := map[string]int32{}
	for _, e := range idx.ord {
		row := idxRow{K: e.key, V: e.val}
		var ids []string
		if idx.Unique {
			if id, ok := idx.unique[e.key]; ok {
				ids = []string{id}
			} else {
				ids = []string{}
			}
		} else {
			ids = idx.idsSortedForKey(e.key)
			if ids == nil {
				ids = []string{}
			}
		}
		row.IDs = ids
		p.Rows = append(p.Rows, row)

		m.Keys = append(m.Keys, e.key)
		m.Vals = append(m.Vals, e.val)
		for _, id := range ids {
			col, ok := idCol[id]
			if !ok {
				col = int32(len(m.Ids))
				idCol[id] = col
				m.Ids = append(m.Ids, id)
			}
			m.Indices = append(m.Indices, col)
		}
		m.Indptr = append(m.Indptr, int32(len(m.Indices)))
	}
	m.Checksum = m.computeChecksum()
	// Construida 1:1 desde el recorrido de ord — sirve como vista plana viva hasta la
	// siguiente mutación (addKey/removeKey limpian idx.matrix).
	idx.matrix = m
	p.CSR = m
	return p
}

// computeChecksum es CRC32-C sobre los campos estructurales de la matrix.
func (m *csrMatrix) computeChecksum() uint32 {
	h := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	for _, k := range m.Keys {
		binary.Write(h, binary.LittleEndian, uint32(len(k)))
		h.Write([]byte(k))
	}
	for _, v := range m.Vals {
		jv, _ := json.Marshal(v)
		binary.Write(h, binary.LittleEndian, uint32(len(jv)))
		h.Write(jv)
	}
	for _, n := range m.Indptr {
		binary.Write(h, binary.LittleEndian, n)
	}
	for _, n := range m.Indices {
		binary.Write(h, binary.LittleEndian, n)
	}
	for _, id := range m.Ids {
		binary.Write(h, binary.LittleEndian, uint32(len(id)))
		h.Write([]byte(id))
	}
	return h.Sum32()
}

// valid comprueba la coherencia estructural y que el checksum coincida.
func (m *csrMatrix) valid() bool {
	if m == nil || len(m.Indptr) == 0 {
		return false
	}
	if int(m.Indptr[0]) != 0 || len(m.Indptr) != len(m.Keys)+1 {
		return false
	}
	if len(m.Vals) != len(m.Keys) {
		return false
	}
	last := int(m.Indptr[len(m.Indptr)-1])
	if last != len(m.Indices) {
		return false
	}
	for i := 1; i < len(m.Indptr); i++ {
		if m.Indptr[i] < m.Indptr[i-1] {
			return false
		}
	}
	for _, col := range m.Indices {
		if col < 0 || int(col) >= len(m.Ids) {
			return false
		}
	}
	return m.Checksum == m.computeChecksum()
}

// loadRows reconstruye el índice en memoria. M2: prefiere la matrix CSR cuando su
// checksum valida; si no, recurre a Rows; vacío/corrupto → error (camino "eager").
// Filas únicas con más de 1 id → ErrDuplicate (el camino de reparación "eager" lo resuelve).
func (idx *index) loadRows(p idxPayload) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !sameFields(idx.Fields, p.Fields) || idx.Unique != p.Unique {
		return errIdxMismatch
	}
	if p.CSR != nil && p.CSR.valid() {
		return idx.loadCSR(p.CSR)
	}
	idx.matrix = nil
	idx.entries = map[string]map[string]struct{}{}
	idx.unique = map[string]string{}
	idx.ord = make([]ordEntry, 0, len(p.Rows))
	idx.ordDirty = false
	for _, r := range p.Rows {
		if idx.Unique {
			if len(r.IDs) > 1 {
				return ErrDuplicate
			}
			if len(r.IDs) == 1 {
				idx.unique[r.K] = r.IDs[0]
				idx.ord = append(idx.ord, ordEntry{val: r.V, key: r.K})
			}
			continue
		}
		if len(r.IDs) == 0 {
			continue
		}
		set := make(map[string]struct{}, len(r.IDs))
		for _, id := range r.IDs {
			set[id] = struct{}{}
		}
		idx.entries[r.K] = set
		idx.ord = append(idx.ord, ordEntry{val: r.V, key: r.K})
	}
	return nil
}

// loadCSR materializa entries/unique/ord desde una matrix ya validada y mantiene
// idx.matrix viva para búsquedas planas por rango/orden. El llamador tiene idx.mu.
func (idx *index) loadCSR(m *csrMatrix) error {
	idx.entries = map[string]map[string]struct{}{}
	idx.unique = map[string]string{}
	idx.ord = make([]ordEntry, 0, len(m.Keys))
	idx.ordDirty = false
	for i, k := range m.Keys {
		lo, hi := int(m.Indptr[i]), int(m.Indptr[i+1])
		var ids []string
		for _, col := range m.Indices[lo:hi] {
			ids = append(ids, m.Ids[col])
		}
		if idx.Unique {
			if len(ids) > 1 {
				return ErrDuplicate
			}
			if len(ids) == 1 {
				idx.unique[k] = ids[0]
				idx.ord = append(idx.ord, ordEntry{val: m.Vals[i], key: k})
			}
			continue
		}
		if len(ids) == 0 {
			continue
		}
		set := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			set[id] = struct{}{}
		}
		idx.entries[k] = set
		idx.ord = append(idx.ord, ordEntry{val: m.Vals[i], key: k})
	}
	// Se conserva la matrix como vista plana — las filas de ord se alinean 1:1 con
	// m.Keys solo si no se saltaron claves vacías. Se reajusta indptr guardando una
	// matrix cuyas Keys coincidan con el ord conservado (descartando huecos únicos vacíos).
	idx.matrix = m.alignToOrd(idx.ord)
	return nil
}

// alignToOrd devuelve una matrix cuyo número de filas coincide con ord (descarta
// las claves que loadCSR saltó: filas únicas vacías / conjuntos no únicos vacíos).
func (m *csrMatrix) alignToOrd(ord []ordEntry) *csrMatrix {
	if len(ord) == len(m.Keys) {
		return m
	}
	keep := map[string]int{} // key → row index in m
	for i, k := range m.Keys {
		keep[k] = i
	}
	out := &csrMatrix{
		Keys:    make([]string, 0, len(ord)),
		Vals:    make([]any, 0, len(ord)),
		Indptr:  make([]int32, 1, len(ord)+1),
		Indices: []int32{},
		Ids:     m.Ids,
	}
	for _, e := range ord {
		i, ok := keep[e.key]
		if !ok {
			continue
		}
		out.Keys = append(out.Keys, m.Keys[i])
		out.Vals = append(out.Vals, m.Vals[i])
		out.Indices = append(out.Indices, m.Indices[m.Indptr[i]:m.Indptr[i+1]]...)
		out.Indptr = append(out.Indptr, int32(len(out.Indices)))
	}
	out.Checksum = out.computeChecksum()
	return out
}

// errIdxMismatch indica que la definición del índice en META y la carga IDX no coinciden (respaldo "eager").
var errIdxMismatch = fmt.Errorf("db: index definition mismatch")

// extractIndexRows construye una fila []any por cada combinación de claves del índice.
// Los arrays de primer nivel se expanden (se indexa cada elemento); ausente/null → centinela nil.
func extractIndexRows(doc Document, fields []string) [][]any {
	rows := [][]any{{}}
	for _, f := range fields {
		v, exists := lookup(doc, f)
		if !exists {
			v = nil
		}
		next := make([][]any, 0, len(rows))
		if arr, ok := v.([]any); ok {
			if len(arr) == 0 {
				// un array vacío se indexa como nil (estilo Mongo: no hay elementos)
				for _, row := range rows {
					nr := append(append([]any{}, row...), nil)
					next = append(next, nr)
				}
			} else {
				for _, elem := range arr {
					for _, row := range rows {
						nr := append(append([]any{}, row...), elem)
						next = append(next, nr)
					}
				}
			}
		} else {
			for _, row := range rows {
				nr := append(append([]any{}, row...), v)
				next = append(next, nr)
			}
		}
		rows = next
	}
	return rows
}

// encodeIndexKey une los valores de campo con \x1f (clave compuesta, DESIGN §4.3).
func encodeIndexKey(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = encodeIndexValue(v)
	}
	return strings.Join(parts, "\x1f")
}

func encodeIndexValue(v any) string {
	if v == nil {
		return "u:" // centinela de ausente/null
	}
	switch t := v.(type) {
	case string:
		return "s:" + strconv.Itoa(len(t)) + ":" + t
	case bool:
		if t {
			return "b:true"
		}
		return "b:false"
	case Document:
		b, _ := json.Marshal(t)
		return "j:" + string(b)
	case map[string]any:
		b, _ := json.Marshal(t)
		return "j:" + string(b)
	default:
		if f, ok := toFloat(v); ok {
			return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("x:%v", v)
		}
		return "j:" + string(b)
	}
}

// sameFields indica si dos listas de campos son idénticas (sensible al orden).
func sameFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
