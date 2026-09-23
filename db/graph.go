package db

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/oklog/ulid/v2"
)

// Edge collections live as normal collections named edges.<type>
// with docs {_id, _from, _to, ...props} (M6).
const edgeCollPrefix = "edges."

// Direction selects edge traversal orientation.
type Direction int

const (
	Outgoing Direction = iota + 1 // _from → _to (default)
	Incoming                      // _to → _from
	Both                          // union, deduped
)

// Traverse anti-choke limits (M6).
const (
	defaultTraverseDepth = 5
	maxTraverseDepth     = 32
	defaultTraverseLimit = 10000
	maxTraverseLimit     = 100000
	defaultPathDepth     = 6
	maxPathDepth         = 32
)

// graphAdj is the CSR adjacency snapshot for one (edgeType, direction):
// keys sorted, indptr ranges, indices into the flat neigh id table.
type graphAdj struct {
	keys    []string       // sorted sources (vertices with ≥1 out-edge)
	keyIdx  map[string]int // source → row
	indptr  []int          // len(keys)+1
	indices []int          // neighbor columns into neigh (classic CSR)
	neigh   []string       // id table referenced by indices
}

func (g *graphAdj) neighborsOf(v string) []string {
	if g == nil {
		return nil
	}
	row, ok := g.keyIdx[v]
	if !ok {
		return nil
	}
	start, end := g.indptr[row], g.indptr[row+1]
	out := make([]string, 0, end-start)
	for _, col := range g.indices[start:end] {
		out = append(out, g.neigh[col])
	}
	return out
}

// buildAdj constructs CSR from edge pairs. reverse swaps (from,to) roles.
// Neighbors per row are deduped and sorted.
func buildAdj(pairs [][2]string, reverse bool) *graphAdj {
	bucket := map[string]map[string]struct{}{}
	verts := map[string]struct{}{}
	for _, p := range pairs {
		k, v := p[0], p[1]
		if reverse {
			k, v = v, k
		}
		if bucket[k] == nil {
			bucket[k] = map[string]struct{}{}
		}
		bucket[k][v] = struct{}{}
		verts[k] = struct{}{}
		verts[v] = struct{}{}
	}
	if len(bucket) == 0 {
		return &graphAdj{keyIdx: map[string]int{}}
	}
	keys := make([]string, 0, len(bucket))
	for k := range bucket {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vertIdx := make(map[string]int, len(verts))
	adj := &graphAdj{
		keyIdx: make(map[string]int, len(keys)),
		indptr: make([]int, 0, len(keys)+1),
	}
	adj.indptr = append(adj.indptr, 0)
	for _, k := range keys {
		adj.keyIdx[k] = len(adj.keys)
		adj.keys = append(adj.keys, k)
		ns := make([]string, 0, len(bucket[k]))
		for v := range bucket[k] {
			ns = append(ns, v)
		}
		sort.Strings(ns)
		for _, v := range ns {
			col, ok := vertIdx[v]
			if !ok {
				col = len(adj.neigh)
				vertIdx[v] = col
				adj.neigh = append(adj.neigh, v)
			}
			adj.indices = append(adj.indices, col)
		}
		adj.indptr = append(adj.indptr, len(adj.indices))
	}
	return adj
}

func isEdgeColl(name string) bool {
	return strings.HasPrefix(name, edgeCollPrefix) && len(name) > len(edgeCollPrefix)
}

func edgeCollName(edgeType string) string {
	return edgeCollPrefix + edgeType
}

func validateEdge(edgeType, from, to string) error {
	if edgeType == "" {
		return fmt.Errorf("db: edge type required")
	}
	if strings.ContainsAny(edgeType, " \t\n") {
		return fmt.Errorf("db: invalid edge type %q", edgeType)
	}
	if from == "" || to == "" {
		return fmt.Errorf("db: edge requires non-empty _from and _to")
	}
	return nil
}

// AddEdge inserts {_from,_to,...props} into edges.<type>; returns _id
// (auto ULID when props has none). Bumps the graph epoch (cache invalidation).
func (s *Store) AddEdge(edgeType string, from, to string, props Document) (string, error) {
	if err := validateEdge(edgeType, from, to); err != nil {
		return "", err
	}
	doc := Document{}
	if len(props) > 0 {
		doc = clone(props)
	}
	doc["_from"] = from
	doc["_to"] = to
	if _, ok := doc["_id"]; !ok {
		doc["_id"] = ulid.Make().String()
	}
	id, ok := doc["_id"].(string)
	if !ok || id == "" {
		return "", ErrNoID
	}
	if err := s.Insert(edgeCollName(edgeType), doc); err != nil {
		return "", err
	}
	return id, nil
}

// RemoveEdge deletes the edge doc by _id from edges.<type>.
func (s *Store) RemoveEdge(edgeType string, id string) error {
	if edgeType == "" {
		return fmt.Errorf("db: edge type required")
	}
	return s.Delete(edgeCollName(edgeType), id)
}

// AddEdge is the Session-scoped insert (RBAC write on edges.<type>).
func (sess *Session) AddEdge(edgeType string, from, to string, props Document) (string, error) {
	if err := validateEdge(edgeType, from, to); err != nil {
		return "", err
	}
	doc := Document{}
	if len(props) > 0 {
		doc = clone(props)
	}
	doc["_from"] = from
	doc["_to"] = to
	if _, ok := doc["_id"]; !ok {
		doc["_id"] = ulid.Make().String()
	}
	id, _ := doc["_id"].(string)
	if id == "" {
		return "", ErrNoID
	}
	return id, sess.Insert(edgeCollName(edgeType), doc)
}

// Neighbors returns adjacent vertex ids (deduped, sorted).
// dir defaults to Outgoing when 0.
func (s *Store) Neighbors(edgeType string, vertex string, dir Direction) ([]string, error) {
	return s.neighbors(nil, edgeType, vertex, dir)
}

// Neighbors is the Session-scoped read (RBAC on edges.<type>).
func (sess *Session) Neighbors(edgeType string, vertex string, dir Direction) ([]string, error) {
	return sess.store.neighbors(sess, edgeType, vertex, dir)
}

func (s *Store) neighbors(sess *Session, edgeType string, vertex string, dir Direction) ([]string, error) {
	if edgeType == "" {
		return nil, fmt.Errorf("db: edge type required")
	}
	if dir == 0 {
		dir = Outgoing
	}
	coll := edgeCollName(edgeType)
	s.mu.RLock()
	err := s.checkReadLocked(sess, coll)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if vertex == "" {
		return []string{}, nil
	}
	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	s.ensureGraphLocked()
	out, inn := s.graphOut[edgeType], s.graphIn[edgeType]
	switch dir {
	case Incoming:
		return append([]string{}, inn.neighborsOf(vertex)...), nil
	case Both:
		set := map[string]struct{}{}
		for _, v := range out.neighborsOf(vertex) {
			set[v] = struct{}{}
		}
		for _, v := range inn.neighborsOf(vertex) {
			set[v] = struct{}{}
		}
		res := make([]string, 0, len(set))
		for v := range set {
			res = append(res, v)
		}
		sort.Strings(res)
		return res, nil
	default:
		return append([]string{}, out.neighborsOf(vertex)...), nil
	}
}

// TraverseOptions controls BFS from Start (M6).
type TraverseOptions struct {
	EdgeType  string
	Start     string
	Direction Direction // 0 = Outgoing
	MinDepth  int       // return nodes with depth ≥ MinDepth (still traverses to MaxDepth)
	MaxDepth  int       // 0 = default 5; clamped to [0, maxTraverseDepth]
	Limit     int       // max nodes returned; 0 = default; clamped to maxTraverseLimit
}

// TraverseNode is one vertex reached by Traverse.
type TraverseNode struct {
	Vertex string `json:"vertex"`
	Depth  int    `json:"depth"`
}

// Traverse runs BFS from opts.Start up to MaxDepth/Limit (anti-choke).
// Results ordered by discovery (BFS). Start included at depth 0.
func (s *Store) Traverse(opts TraverseOptions) ([]TraverseNode, error) {
	return s.traverse(nil, opts)
}

// Traverse is the Session-scoped graph walk (RBAC on edges.<type>).
func (sess *Session) Traverse(opts TraverseOptions) ([]TraverseNode, error) {
	return sess.store.traverse(sess, opts)
}

func (s *Store) traverse(sess *Session, opts TraverseOptions) ([]TraverseNode, error) {
	if opts.EdgeType == "" {
		return nil, fmt.Errorf("db: edge type required")
	}
	if opts.Start == "" {
		return nil, fmt.Errorf("db: traverse start required")
	}
	if opts.Direction == 0 {
		opts.Direction = Outgoing
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultTraverseDepth
	}
	if opts.MaxDepth > maxTraverseDepth {
		opts.MaxDepth = maxTraverseDepth
	}
	if opts.MinDepth < 0 {
		opts.MinDepth = 0
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultTraverseLimit
	}
	if opts.Limit > maxTraverseLimit {
		opts.Limit = maxTraverseLimit
	}
	coll := edgeCollName(opts.EdgeType)
	s.mu.RLock()
	err := s.checkReadLocked(sess, coll)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	s.ensureGraphLocked()
	out, inn := s.graphOut[opts.EdgeType], s.graphIn[opts.EdgeType]
	adjFor := func(v string) []string {
		switch opts.Direction {
		case Incoming:
			return inn.neighborsOf(v)
		case Both:
			set := map[string]struct{}{}
			for _, n := range out.neighborsOf(v) {
				set[n] = struct{}{}
			}
			for _, n := range inn.neighborsOf(v) {
				set[n] = struct{}{}
			}
			res := make([]string, 0, len(set))
			for n := range set {
				res = append(res, n)
			}
			sort.Strings(res)
			return res
		default:
			return out.neighborsOf(v)
		}
	}

	visited := map[string]struct{}{opts.Start: {}}
	type item struct {
		v string
		d int
	}
	queue := []item{{opts.Start, 0}}
	var result []TraverseNode
	head := 0
	for head < len(queue) {
		cur := queue[head]
		head++
		if cur.d >= opts.MinDepth {
			result = append(result, TraverseNode{Vertex: cur.v, Depth: cur.d})
			if len(result) >= opts.Limit {
				break
			}
		}
		if cur.d >= opts.MaxDepth {
			continue
		}
		for _, n := range adjFor(cur.v) {
			if _, seen := visited[n]; seen {
				continue
			}
			visited[n] = struct{}{}
			queue = append(queue, item{n, cur.d + 1})
		}
	}
	return result, nil
}

// ShortestPath returns the unweighted shortest vertex path [from, ..., to]
// via BFS, or nil when unreachable within maxDepth (0 = default 6,
// clamped to maxPathDepth). Session-scoped variant: Session.ShortestPath.
func (s *Store) ShortestPath(edgeType, from, to string, maxDepth int) ([]string, error) {
	return s.shortestPath(nil, edgeType, from, to, maxDepth)
}

// ShortestPath is the Session-scoped BFS path (RBAC on edges.<type>).
func (sess *Session) ShortestPath(edgeType, from, to string, maxDepth int) ([]string, error) {
	return sess.store.shortestPath(sess, edgeType, from, to, maxDepth)
}

func (s *Store) shortestPath(sess *Session, edgeType, from, to string, maxDepth int) ([]string, error) {
	if edgeType == "" {
		return nil, fmt.Errorf("db: edge type required")
	}
	if from == "" || to == "" {
		return nil, fmt.Errorf("db: path endpoints required")
	}
	if from == to {
		return []string{from}, nil
	}
	if maxDepth <= 0 {
		maxDepth = defaultPathDepth
	}
	if maxDepth > maxPathDepth {
		maxDepth = maxPathDepth
	}
	coll := edgeCollName(edgeType)
	s.mu.RLock()
	err := s.checkReadLocked(sess, coll)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}

	s.graphMu.Lock()
	defer s.graphMu.Unlock()
	s.ensureGraphLocked()
	out := s.graphOut[edgeType]
	if out == nil {
		return nil, nil
	}
	prev := map[string]string{from: ""}
	type item struct {
		v string
		d int
	}
	queue := []item{{from, 0}}
	head := 0
	for head < len(queue) {
		cur := queue[head]
		head++
		if cur.d >= maxDepth {
			continue
		}
		for _, n := range out.neighborsOf(cur.v) {
			if _, seen := prev[n]; seen {
				continue
			}
			prev[n] = cur.v
			if n == to {
				// reconstruct
				path := []string{to}
				for x := to; x != ""; {
					x = prev[x]
					if x == "" {
						break
					}
					path = append(path, x)
				}
				// reverse
				for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				return path, nil
			}
			queue = append(queue, item{n, cur.d + 1})
		}
	}
	return nil, nil
}

// --- cache maintenance ---------------------------------------------------

// noteEdgeMutation bumps the graph epoch. Caller holds s.mu (write).
func (s *Store) noteEdgeMutation(coll string) {
	if isEdgeColl(coll) {
		s.graphEpoch.Add(1)
	}
}

// ensureGraphLocked rebuilds CSR snapshots when the epoch moved.
// Lock order: graphMu → s.mu (same as ensureGraph).
func (s *Store) ensureGraphLocked() {
	cur := s.graphEpoch.Load()
	if s.graphOut != nil && s.graphBuiltEpoch == cur {
		return
	}
	out := map[string]*graphAdj{}
	inn := map[string]*graphAdj{}
	s.mu.RLock()
	for name, c := range s.collections {
		if !isEdgeColl(name) {
			continue
		}
		et := strings.TrimPrefix(name, edgeCollPrefix)
		pairs := make([][2]string, 0, len(c.entries))
		for id := range c.entries {
			d, err := s.resident(c, id)
			if err != nil || d == nil {
				continue
			}
			from, _ := d["_from"].(string)
			to, _ := d["_to"].(string)
			if from == "" || to == "" {
				continue
			}
			pairs = append(pairs, [2]string{from, to})
		}
		out[et] = buildAdj(pairs, false)
		inn[et] = buildAdj(pairs, true)
	}
	s.graphBuiltEpoch = s.graphEpoch.Load()
	s.mu.RUnlock()
	s.graphOut, s.graphIn = out, inn
}

// graphFields are attached to Store (M6).
type graphFields struct {
	graphMu         sync.Mutex
	graphEpoch      atomic.Uint64
	graphBuiltEpoch uint64 // guarded by graphMu
	graphOut        map[string]*graphAdj
	graphIn         map[string]*graphAdj
}
