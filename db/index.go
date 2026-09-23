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

// IndexInfo describes one index on a collection (for ListIndexes / persistence).
type IndexInfo struct {
	Fields []string `json:"fields"`
	Unique bool     `json:"unique"`
}

// ordEntry is one distinct index key in sorted order: raw first-field value
// (for compareOrdered) plus the full encoded key (tiebreak / hash link).
type ordEntry struct {
	val any
	key string
}

// index is an in-memory index over one collection: hash for equality
// plus a lazily-sorted ord slice for range/prefix seeks and sort-by-index.
// Non-unique: key → set of _id. Unique: key → single _id.
// mu guards entries/unique/ord (ensureOrd mutates; Find may hold s.mu.RLock).
type index struct {
	mu       sync.Mutex
	Fields   []string
	Unique   bool
	entries  map[string]map[string]struct{} // key → ids (non-unique)
	unique   map[string]string              // key → id (unique)
	ord      []ordEntry                     // distinct keys ordered by (first-field val, key)
	ordDirty bool                           // needs re-sort before binary search
	// matrix is the flat CSR view of (ord × ids), non-nil only while it is
	// known to match entries/unique/ord (set on loadRows from a valid CSR,
	// cleared on every mutation). Used to avoid per-key map lookups + sort.
	matrix *csrMatrix
}

func newIndex(fields []string, unique bool) *index {
	idx := &index{
		Fields:   append([]string{}, fields...),
		Unique:   unique,
		ordDirty: true, // bulk build: append all, sort once on first seek
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

// typeRank orders types for range seeks: nil < numbers < string < bool < other.
// Cross-type never compares equal in ranges (Mongo-like: no cross-type order).
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

// compareOrdered is a total order on index first-field values: type rank first,
// then compareValues within numeric/string ranks (bool: false < true).
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
		return 0 // other: no cross value order; key tiebreak only
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

// ensureOrd sorts ord if dirty. Caller must hold idx.mu.
func (idx *index) ensureOrd() {
	if !idx.ordDirty {
		return
	}
	sort.Slice(idx.ord, func(i, j int) bool { return ordLess(idx.ord[i], idx.ord[j]) })
	idx.ordDirty = false
}

// addDoc indexes all key-rows for docID. On unique conflict returns ErrDuplicate
// and does not partially apply (caller must not have removed old entries yet,
// or must rollback).
func (idx *index) addDoc(doc Document, docID string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.addDocLocked(doc, docID)
}

func (idx *index) addDocLocked(doc Document, docID string) error {
	rows := extractIndexRows(doc, idx.Fields)
	// Stage keys first so unique conflicts abort cleanly.
	keys := make([]string, 0, len(rows))
	vals := make([]any, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		k := encodeIndexKey(row)
		if seen[k] {
			continue // same key twice in one doc (duplicate array elems)
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

// addKey assumes idx.mu held.
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
	// Bulk path (dirty): append and sort lazily on next seek.
	// Steady path (clean): binary-insert to keep ord sorted without a full sort.
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

// removeDoc drops all index entries for docID using the given document values.
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
		// Unsorted: linear scan (avoid forcing a sort mid-batch).
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
	// val mismatch fallback (should not happen): linear scan by key
	for j := range idx.ord {
		if idx.ord[j].key == e.key {
			idx.ord = append(idx.ord[:j], idx.ord[j+1:]...)
			return
		}
	}
}

// lookupIDs returns _id set for equality values on the index (full key when
// len(values)==len(Fields); partial not used in MVP planner).
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

// countKey returns how many docs match the full key without building an ID set.
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

// covers reports whether every index row for doc/docID is present.
// Used at flush time to set the suspect flag when index state is stale.
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

// seekPrefix returns docs whose encoded key starts with the prefix of
// values (values must be a leading subset of Fields). Full-length values
// are an exact key seek; partial seeks narrow via ord on the first field.
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
		// unordered band: full scan of distinct keys
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

// rangeBound describes a pure $gt/$gte/$lt/$lte conjunction on one field.
type rangeBound struct {
	lo, hi       any
	hasLo, hasHi bool
	loIncl       bool
	hiIncl       bool
}

// seekRange returns docs where the first index field value is in [lo,hi]
// (same type-rank band only). IDs are in ascending ord order.
func (idx *index) seekRange(rb rangeBound) []string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.ensureOrd()
	// Determine target type rank from bounds.
	rank := -1
	if rb.hasLo && rb.hasHi {
		ra, rh := typeRank(rb.lo), typeRank(rb.hi)
		if ra != rh {
			return nil // no value can be in both bands
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
		// nil / bool / other are not ordered by compareOp (comparableTypes false)
		return nil
	}

	// Binary search lower edge within [rank] band.
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
			return false // still in band; end advances past band below
		}
		c := compareOrdered(e.val, rb.hi)
		if rb.hiIncl {
			return c > 0
		}
		return c >= 0
	})
	// When only lo bound: end must stop at end of rank band.
	if !rb.hasHi {
		end = sort.Search(len(idx.ord), func(i int) bool {
			return typeRank(idx.ord[i].val) > rank
		})
	}
	// When only hi bound: start must start at beginning of rank band.
	if !rb.hasLo {
		start = sort.Search(len(idx.ord), func(i int) bool {
			r := typeRank(idx.ord[i].val)
			return r >= rank
		})
	}
	return idx.collectRange(start, end)
}

// collectRange gathers ids for ord rows [start,end), preferring the flat CSR
// matrix when it is in sync (M2), else the per-key map path.
// Caller holds idx.mu (and ord must already be sorted if using matrix).
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

// idsSortedForKey returns doc ids for one key in ascending order (deterministic).
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

// orderedIDs walks the whole index in (val, _id) order — sort by index.
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

// idxRow is one persisted index key row (M1: full serialize per flush).
type idxRow struct {
	K   string   `json:"k"`
	V   any      `json:"v,omitempty"`
	IDs []string `json:"ids"`
}

// csrMatrix is the M2 flat CSR view of an index: row i ↔ ord[i],
// Indices[Indptr[i]:Indptr[i+1]] are column ordinals into Ids (docID dict).
// Checksum is CRC32-C over keys|indptr|indices|ids for load-time validation.
type csrMatrix struct {
	Keys     []string `json:"keys"`   // distinct keys, ord order (row labels)
	Vals     []any    `json:"vals"`   // first-field values (row data for ranges)
	Indptr   []int32  `json:"indptr"` // len = len(Keys)+1
	Indices  []int32  `json:"indices"`
	Ids      []string `json:"ids"` // column dictionary: docID
	Checksum uint32   `json:"csum"`
}

// idxPayload is the plaintext of an IDX record.
type idxPayload struct {
	Coll   string     `json:"c"`
	Fields []string   `json:"f"`
	Unique bool       `json:"u"`
	Rows   []idxRow   `json:"rows"`
	CSR    *csrMatrix `json:"csr,omitempty"` // M2: primary load path
}

// serialize snapshots index rows in ord order for persistence, and embeds
// the CSR matrix (M2). Rows remain as a fallback when the CSR checksum fails.
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
	// Built 1:1 from the ord walk — safe as the live flat view until the
	// next mutation (addKey/removeKey clear idx.matrix).
	idx.matrix = m
	p.CSR = m
	return p
}

// computeChecksum is CRC32-C over the structural fields of the matrix.
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

// valid reports structural sanity + checksum match.
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

// loadRows rebuilds the in-memory index. M2: prefers the CSR matrix when its
// checksum validates; falls back to Rows; empty/corrupt → error (eager path).
// Unique rows with >1 id → ErrDuplicate (eager repair path handles repair).
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

// loadCSR materializes entries/unique/ord from a validated matrix and keeps
// idx.matrix live for flat range/sort seeks. Caller holds idx.mu.
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
	// Keep the matrix as the flat view — ord rows align 1:1 with m.Keys
	// only if no empty keys were skipped. Rebuild indptr alignment by
	// storing a matrix whose Keys match the kept ord (drop empty unique gaps).
	idx.matrix = m.alignToOrd(idx.ord)
	return nil
}

// alignToOrd returns a matrix whose row count matches ord (drops keys that
// loadCSR skipped: empty unique rows / empty non-unique sets).
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

// errIdxMismatch signals META index def vs IDX payload disagreement (eager fallback).
var errIdxMismatch = fmt.Errorf("db: index definition mismatch")

// extractIndexRows builds one []any row per index-key combination.
// Top-level arrays expand (each element indexes); missing/null → nil sentinel.
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
				// empty array indexes as nil (Mongo-ish: no elements)
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

// encodeIndexKey joins field values with \x1f (DESIGN §4.3 compound key).
func encodeIndexKey(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = encodeIndexValue(v)
	}
	return strings.Join(parts, "\x1f")
}

func encodeIndexValue(v any) string {
	if v == nil {
		return "u:" // missing/null sentinel
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

// sameFields reports whether two field lists are identical (order-sensitive).
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
