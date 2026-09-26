package db

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/oklog/ulid/v2"
)

// Las colecciones de aristas viven como colecciones normales llamadas edges.<tipo>
// con documentos {_id, _from, _to, ...props} (M6).
const edgeCollPrefix = "edges."

// Direction selecciona la orientación del recorrido de aristas.
type Direction int

const (
	Outgoing Direction = iota + 1 // _from → _to (por defecto)
	Incoming                      // _to → _from
	Both                          // unión, sin duplicados
)

// Límites anti-atasco de Traverse (M6).
const (
	defaultTraverseDepth = 5
	maxTraverseDepth     = 32
	defaultTraverseLimit = 10000
	maxTraverseLimit     = 100000
	defaultPathDepth     = 6
	maxPathDepth         = 32
)

// graphAdj es la instantánea de adyacencia CSR de un par (tipo de arista, dirección):
// claves ordenadas, rangos indptr e índices dentro de la tabla plana de ids vecinos.
type graphAdj struct {
	keys    []string       // fuentes ordenadas (vértices con al menos 1 arista saliente)
	keyIdx  map[string]int // fuente → fila
	indptr  []int          // len(keys)+1
	indices []int          // columnas vecinas dentro de neigh (CSR clásico)
	neigh   []string       // tabla de ids referenciada por indices
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

// buildAdj construye el CSR a partir de los pares de aristas. reverse intercambia los papeles (from,to).
// Los vecinos de cada fila se deduplican y se ordenan.
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

// AddEdge inserta {_from,_to,...props} en edges.<tipo>; devuelve el _id
// (ULID automático si props no trae ninguno). Incrementa la época del grafo (invalidación de caché).
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

// RemoveEdge elimina el documento de arista por _id de edges.<tipo>.
func (s *Store) RemoveEdge(edgeType string, id string) error {
	if edgeType == "" {
		return fmt.Errorf("db: edge type required")
	}
	return s.Delete(edgeCollName(edgeType), id)
}

// RemoveEdge es Store.RemoveEdge con permiso de escritura sobre edges.<tipo> (admin M9).
func (sess *Session) RemoveEdge(edgeType string, id string) error {
	if edgeType == "" {
		return fmt.Errorf("db: edge type required")
	}
	return sess.Delete(edgeCollName(edgeType), id)
}

// AddEdge es la inserción con sesión (escritura RBAC sobre edges.<tipo>).
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

// Neighbors devuelve los ids de vértices adyacentes (sin duplicados y ordenados).
// dir usa Outgoing por defecto cuando vale 0.
func (s *Store) Neighbors(edgeType string, vertex string, dir Direction) ([]string, error) {
	return s.neighbors(nil, edgeType, vertex, dir)
}

// Neighbors es la lectura con sesión (RBAC sobre edges.<tipo>).
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

// TraverseOptions controla el BFS desde Start (M6).
type TraverseOptions struct {
	EdgeType  string
	Start     string
	Direction Direction // 0 = Outgoing
	MinDepth  int       // devolver nodos con profundidad ≥ MinDepth (se sigue recorriendo hasta MaxDepth)
	MaxDepth  int       // 0 = 5 por defecto; se limita a [0, maxTraverseDepth]
	Limit     int       // máximo de nodos devueltos; 0 = por defecto; se limita a maxTraverseLimit
}

// TraverseNode es un vértice alcanzado por Traverse.
type TraverseNode struct {
	Vertex string `json:"vertex"`
	Depth  int    `json:"depth"`
}

// Traverse ejecuta un BFS desde opts.Start hasta MaxDepth/Limit (anti-atasco).
// Los resultados se ordenan por descubrimiento (BFS). Start se incluye en profundidad 0.
func (s *Store) Traverse(opts TraverseOptions) ([]TraverseNode, error) {
	return s.traverse(nil, opts)
}

// Traverse es el recorrido con sesión (RBAC sobre edges.<tipo>).
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

// ShortestPath devuelve el camino más corto sin pesos [from, ..., to] mediante BFS, o nil
// si no es alcanzable dentro de maxDepth (0 = 6 por defecto, limitado a maxPathDepth).
// Variante con sesión: Session.ShortestPath.
func (s *Store) ShortestPath(edgeType, from, to string, maxDepth int) ([]string, error) {
	return s.shortestPath(nil, edgeType, from, to, maxDepth)
}

// ShortestPath es el camino BFS con sesión (RBAC sobre edges.<tipo>).
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
				// reconstruir
				path := []string{to}
				for x := to; x != ""; {
					x = prev[x]
					if x == "" {
						break
					}
					path = append(path, x)
				}
				// invertir
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

// --- mantenimiento de caché ---------------------------------------------------

// noteEdgeMutation incrementa la época del grafo. El llamador tiene s.mu (escritura).
func (s *Store) noteEdgeMutation(coll string) {
	if isEdgeColl(coll) {
		s.graphEpoch.Add(1)
	}
}

// ensureGraphLocked reconstruye las instantáneas CSR cuando la época ha cambiado.
// Orden de bloqueo: graphMu → s.mu (igual que ensureGraph).
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

// graphFields van adosados a Store (M6).
type graphFields struct {
	graphMu         sync.Mutex
	graphEpoch      atomic.Uint64
	graphBuiltEpoch uint64 // protegido por graphMu
	graphOut        map[string]*graphAdj
	graphIn         map[string]*graphAdj
}
