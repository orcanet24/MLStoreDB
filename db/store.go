package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gofrs/flock"
	"github.com/oklog/ulid/v2"
)

// Document es un objeto JSON con un campo _id de tipo string obligatorio.
type Document map[string]any

type collection struct {
	entries   map[string]*docEntry
	indexes   []*index
	sensitive []string
}

// Store es un almacén de documentos JSON en memoria; opcionalmente respaldado
// por un archivo cifrado (M4). Seguro para uso concurrente.
type Store struct {
	mu            sync.RWMutex
	collections   map[string]*collection
	schemaVersion uint64

	// respaldo en archivo (path vacío = solo RAM)
	path    string
	opts    Options
	dek     []byte
	created uint64
	dirty   bool
	closed  bool

	kdfSalt [16]byte
	kdfTime uint32
	kdfMem  uint32
	kdfPar  uint32

	// runtime de M5
	lock      *flock.Flock
	flushStop chan struct{}
	flushDone chan struct{}

	// flush no bloqueante (punto 3)
	flushMu  sync.Mutex // serializa writeState (E/S) sin tener tomado mu
	dirtyGen uint64     // lo incrementa markDirty; el flush limpia dirty solo si no cambió
	machine  []byte     // id de máquina para la KEK resuelto en Open (persiste entre Flush)

	// registro de log / caché de M1
	sawFile         bool // existe un cuerpo v2 duradero (habilita append + DELs)
	v1Migration     bool // se cargó un estado v1 o sospechoso → el próximo flush es reescritura completa
	compactForce    bool // el próximo prepareFlush es una reescritura completa
	flushInProgress bool // instantánea tomada; los borrados deben encolar DELs
	pendingDels     []delItem
	cacheMu         sync.Mutex
	cache           map[*docEntry]struct{}
	residentBytes   int64
	cacheEvictions  uint64
	noEvict         atomicBool // retiene la expulsión (ventana de migración v1)
	kek             []byte     // KEK en caché (Argon2 una vez por credenciales)

	// contrapresión de M3: mutaciones desde la última instantánea de flush duradera.
	writeSeq   uint64 // lo incrementa markDirty
	durableSeq uint64 // writeSeq incluido en el último writeState correcto
	bpCond     *sync.Cond

	// RBAC de M4: token → Session viva. Protegido por mu. Vacío hasta Authenticate.
	sessions map[string]*Session

	// hooks de M5a: registros + worker FIFO asíncrono acotado.
	hooksMu  sync.Mutex
	hooks    []*hookReg
	hookSeq  uint64
	hookQ    chan hookJob
	hookStop chan struct{}
	hookDone chan struct{}

	// triggers declarativos de M5b: _triggers → registro de hook de M5a.
	trigMu         sync.Mutex
	triggersLoaded bool
	triggerHooks   map[string]uint64 // _id del trigger → id de hook de M5a

	// grafo de M6: instantáneas de adyacencia CSR para las colecciones edges.*.
	graphFields
}

// atomicBool envuelve atomic.Bool para el flag de retención de expulsión.
type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) Load() bool   { return b.v.Load() }
func (b *atomicBool) Store(x bool) { b.v.Store(x) }

// New crea un almacén en memoria vacío (schemaVersion 0 = necesita migraciones).
func New() *Store {
	s := &Store{
		collections: map[string]*collection{},
		sessions:    map[string]*Session{},
	}
	s.bpCond = sync.NewCond(&s.mu)
	return s
}

func (s *Store) coll(name string) *collection {
	c, ok := s.collections[name]
	if !ok {
		c = &collection{entries: map[string]*docEntry{}}
		s.collections[name] = c
	}
	return c
}

// EnsureIndex crea (o no hace nada si ya es idéntico) un índice sobre coll.
// fields es 1..N (compuesto). unique impone un documento por combinación de claves.
// En una colección no vacía se construye desde los documentos existentes; los conflictos de único → ErrDuplicate.
func (s *Store) EnsureIndex(coll string, fields []string, unique bool) error {
	if err := s.ensureIndex(coll, fields, unique); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) ensureIndex(coll string, fields []string, unique bool) error {
	if len(fields) == 0 {
		return fmt.Errorf("%w: EnsureIndex needs at least one field", ErrBadFilter)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.coll(coll)
	for _, idx := range c.indexes {
		if sameFields(idx.Fields, fields) {
			if idx.Unique == unique {
				return nil
			}
			return fmt.Errorf("%w: index %v already exists with different uniqueness", ErrDuplicate, fields)
		}
	}
	idx := newIndex(fields, unique)
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		doc, err := s.resident(c, id)
		if err != nil {
			return err
		}
		if err := idx.addDoc(doc, id); err != nil {
			return err
		}
	}
	c.indexes = append(c.indexes, idx)
	s.markDirty()
	return nil
}

// DropIndex elimina el índice que coincide con fields (sensible al orden).
func (s *Store) DropIndex(coll string, fields []string) error {
	if err := s.dropIndex(coll, fields); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) dropIndex(coll string, fields []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	for i, idx := range c.indexes {
		if sameFields(idx.Fields, fields) {
			c.indexes = append(c.indexes[:i], c.indexes[i+1:]...)
			s.markDirty()
			return nil
		}
	}
	return ErrNotFound
}

// ListIndexes devuelve los metadatos de los índices de coll (slice vacío si no hay ninguno o no existe).
func (s *Store) ListIndexes(coll string) ([]IndexInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collRO(coll)
	if !ok {
		return []IndexInfo{}, nil
	}
	out := make([]IndexInfo, 0, len(c.indexes))
	for _, idx := range c.indexes {
		out = append(out, idx.info())
	}
	return out, nil
}

// applyIndexes tras un cambio de documento: quita las entradas antiguas (si old no es nil)
// y agrega las nuevas. Si falla un único al agregar, restaura las entradas antiguas.
func applyIndexes(c *collection, old, newDoc Document, id string) error {
	for _, idx := range c.indexes {
		if old != nil {
			idx.removeDoc(old, id)
		}
	}
	var firstErr error
	for _, idx := range c.indexes {
		if err := idx.addDoc(newDoc, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		// rollback: eliminar lo agregado para newDoc y restaurar old
		for _, idx := range c.indexes {
			idx.removeDoc(newDoc, id)
		}
		if old != nil {
			for _, idx := range c.indexes {
				_ = idx.addDoc(old, id)
			}
		}
		return firstErr
	}
	return nil
}

func (s *Store) collRO(name string) (*collection, bool) {
	c, ok := s.collections[name]
	return c, ok
}

// maxDocBytes es NF1: 1 MB por documento (DESIGN §3.1).
const maxDocBytes = 1 << 20

// docID extrae el _id del documento. Si falta o está vacío ⇒ ULID automático (DESIGN §3.1).
// Muta el documento cuando lo genera.
func docID(doc Document) (string, error) {
	v, ok := doc["_id"]
	if !ok || v == nil {
		id := ulid.Make().String()
		doc["_id"] = id
		return id, nil
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", ErrNoID
	}
	return id, nil
}

// checkDocSize rechaza documentos de más de 1MB (NF1) antes de insert/upsert.
func checkDocSize(doc Document) error {
	// camino rápido: serializar solo si el mapa es grande; comprobar siempre por seguridad
	b, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadFilter, err)
	}
	if len(b) > maxDocBytes {
		return ErrTooLarge
	}
	return nil
}

// clone copia en profundidad el documento para que quien llama no pueda mutar los documentos guardados (también los anidados).
func clone(doc Document) Document {
	out := make(Document, len(doc))
	for k, v := range doc {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneShallow copia solo el primer nivel. Es seguro para el rollback de Update y para
// la instantánea de índices: Update reemplaza claves de primer nivel y nunca muta
// valores anidados in situ, así que las referencias anidadas compartidas siguen intactas para extractIndexRows(old).
func cloneShallow(doc Document) Document {
	out := make(Document, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	return out
}

// cloneValue copia en profundidad valores tipo JSON (mapas/slices); los escalares se devuelven tal cual.
// map[string]any se mantiene como map[string]any (no Document) para que las aserciones de tipo sigan funcionando.
func cloneValue(v any) any {
	switch t := v.(type) {
	case Document:
		out := make(Document, len(t))
		for k, e := range t {
			out[k] = cloneValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = cloneValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = cloneValue(e)
		}
		return out
	case []Document:
		out := make([]Document, len(t))
		for i, d := range t {
			out[i] = clone(d)
		}
		return out
	case []string:
		return append([]string{}, t...)
	case string, bool, float64, float32, int, int32, int64, uint, uint64, nil:
		return v
	default:
		return v
	}
}

// Insert agrega el documento; falla si falta el _id, ya existe, hay conflicto de único o supera 1MB.
func (s *Store) Insert(coll string, doc Document) error {
	if err := s.insertDoc(nil, coll, doc); err != nil {
		return err
	}
	return s.afterMutation()
}

// insertDoc es Insert/Session.Insert con sesión RBAC opcional (M4).
// sess nil = API cruda del Store (permitida solo mientras no existan usuarios).
func (s *Store) insertDoc(sess *Session, coll string, doc Document) error {
	return s.insertDocFrame(nil, sess, coll, doc)
}

// insertDocFrame ejecuta los hooks before/after alrededor de la aplicación con bloqueo (M5a).
func (s *Store) insertDocFrame(frame *hookFrame, sess *Session, coll string, doc Document) error {
	id, err := docID(doc)
	if err != nil {
		return err
	}
	if err := checkDocSize(doc); err != nil {
		return err
	}
	if before := s.matchHooks(BeforeInsert, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		if err := s.previewMissing(coll, id, ErrDuplicate); err != nil {
			return err
		}
		work := clone(doc)
		if err := s.runBeforeHooks(frame, before, BeforeInsert, coll, id, work, nil); err != nil {
			return err
		}
		doc = work
	}
	if err := s.insertApply(sess, coll, id, doc); err != nil {
		return err
	}
	s.queueAfterHook(frame, AfterInsert, coll, id, doc, nil)
	return nil
}

func (s *Store) insertApply(sess *Session, coll string, id string, doc Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c := s.coll(coll)
	if _, exists := c.entries[id]; exists {
		return ErrDuplicate
	}
	stored := clone(doc)
	if err := applyIndexes(c, nil, stored, id); err != nil {
		return err
	}
	e := newDocEntry(stored)
	s.markEntryMutated(e)
	c.entries[id] = e
	s.cacheAdmit(e)
	s.noteEdgeMutation(coll)
	return nil
}

// previewWrite comprueba los permisos bajo RLock (fase de hook before; sin efectos secundarios).
func (s *Store) previewWrite(sess *Session, coll string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkWriteLocked(sess, coll)
}

// previewMissing devuelve errIfPresent cuando el id ya existe (fase previa a los hooks).
func (s *Store) previewMissing(coll, id string, errIfPresent error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.collections[coll]; ok {
		if _, exists := c.entries[id]; exists {
			return errIfPresent
		}
	}
	return nil
}

// previewDoc clona el documento actual o devuelve ErrNotFound (fase de hook before).
func (s *Store) previewDoc(coll, id string) (Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collRO(coll)
	if !ok {
		return nil, ErrNotFound
	}
	d, err := s.resident(c, id)
	if err != nil {
		return nil, err
	}
	return clone(d), nil
}

// Upsert reemplaza o crea el documento con el _id indicado (el id gana sobre doc["_id"]).
func (s *Store) Upsert(coll string, id string, doc Document) error {
	if err := s.upsertDoc(nil, coll, id, doc); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) upsertDoc(sess *Session, coll string, id string, doc Document) error {
	return s.upsertDocFrame(nil, sess, coll, id, doc)
}

func (s *Store) upsertDocFrame(frame *hookFrame, sess *Session, coll string, id string, doc Document) error {
	if id == "" {
		return ErrNoID
	}
	if err := checkDocSize(doc); err != nil {
		return err
	}
	var old Document
	if before := s.matchHooks(BeforeUpsert, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		old, _ = s.previewDoc(coll, id) // nil cuando se está creando
		work := clone(doc)
		if err := s.runBeforeHooks(frame, before, BeforeUpsert, coll, id, work, old); err != nil {
			return err
		}
		doc = work
	}
	if err := s.upsertApply(sess, coll, id, doc); err != nil {
		return err
	}
	s.queueAfterHook(frame, AfterUpsert, coll, id, doc, old)
	return nil
}

func (s *Store) upsertApply(sess *Session, coll string, id string, doc Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c := s.coll(coll)
	var old Document
	if _, ok := c.entries[id]; ok {
		d, err := s.resident(c, id)
		if err == nil {
			old = d
		}
	}
	stored := clone(doc)
	stored["_id"] = id
	if err := applyIndexes(c, old, stored, id); err != nil {
		return err
	}
	if e, ok := c.entries[id]; ok {
		e.docP.Store(&stored)
		s.markEntryMutated(e)
		s.cacheAdmit(e)
	} else {
		e := newDocEntry(stored)
		s.markEntryMutated(e)
		c.entries[id] = e
		s.cacheAdmit(e)
	}
	s.noteEdgeMutation(coll)
	return nil
}

// Update fusiona superficialmente las claves de primer nivel de patch en el documento existente.
func (s *Store) Update(coll string, id string, patch Document) error {
	if err := s.updateDoc(nil, coll, id, patch); err != nil {
		return err
	}
	return s.afterMutation()
}

// UpdateFields es Update más eliminaciones explícitas de campos de primer nivel (wire M8:
// $unset de Mongo / diferencias de reemplazo). Las eliminaciones se aplican a la vista
// previa fusionada antes de ejecutar los hooks (los hooks ven la forma final) y acaban en
// el mismo camino updateApply, así que los triggers/hooks asíncronos se comportan igual que en un Update.
func (s *Store) UpdateFields(coll string, id string, patch Document, remove []string) error {
	if err := s.updateDocFrame(nil, nil, coll, id, patch, remove); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) updateDoc(sess *Session, coll string, id string, patch Document) error {
	return s.updateDocFrame(nil, sess, coll, id, patch, nil)
}

func (s *Store) updateDocFrame(frame *hookFrame, sess *Session, coll string, id string, patch Document, preRemove []string) error {
	var old Document
	var remove []string
	if before := s.matchHooks(BeforeUpdate, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		var err error
		old, err = s.previewDoc(coll, id)
		if err != nil {
			return err
		}
		// Los hooks ven la vista previa fusionada (old + patch) para que set/unset actúen
		// sobre la forma final; después se calcula la diferencia de vuelta a patch + eliminaciones.
		merged := clone(old)
		for k, v := range patch {
			if k == "_id" {
				continue
			}
			merged[k] = cloneValue(v)
		}
		for _, k := range preRemove {
			if k != "_id" {
				delete(merged, k)
			}
		}
		if err := s.runBeforeHooks(frame, before, BeforeUpdate, coll, id, merged, old); err != nil {
			return err
		}
		patch = Document{}
		for k, v := range merged {
			if k == "_id" {
				continue
			}
			ov, existed := old[k]
			if !existed || !equalJSON(ov, v) {
				patch[k] = cloneValue(v)
			}
		}
		for k := range old {
			if k == "_id" {
				continue
			}
			if _, ok := merged[k]; !ok {
				remove = append(remove, k)
			}
		}
	} else if len(preRemove) > 0 {
		for _, k := range preRemove {
			if k != "_id" {
				remove = append(remove, k)
			}
		}
	}
	if err := s.updateApply(sess, coll, id, patch, remove); err != nil {
		return err
	}
	if s.hasAfter(AfterUpdate, coll) {
		if fresh, err := s.previewDoc(coll, id); err == nil {
			s.queueAfterHook(frame, AfterUpdate, coll, id, fresh, old)
		} else {
			s.queueAfterHook(frame, AfterUpdate, coll, id, patch, old)
		}
		return nil
	}
	s.queueAfterHook(frame, AfterUpdate, coll, id, patch, old)
	return nil
}

func (s *Store) hasAfter(event, coll string) bool {
	return len(s.matchHooks(event, coll)) > 0
}

func (s *Store) updateApply(sess *Session, coll string, id string, patch Document, remove []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	if _, ok := c.entries[id]; !ok {
		return ErrNotFound
	}
	doc, err := s.resident(c, id)
	if err != nil {
		return err
	}
	// COW: se construye el nuevo mapa aparte para que un fallo de tamaño/índice deje
	// intacto el mapa residente vivo (sin rollback in situ).
	old := cloneShallow(doc)
	next := cloneShallow(doc)
	for k, v := range patch {
		if k == "_id" {
			continue
		}
		next[k] = cloneValue(v)
	}
	for _, k := range remove {
		delete(next, k)
	}
	if err := checkDocSize(next); err != nil {
		return err
	}
	if err := applyIndexes(c, old, next, id); err != nil {
		return err
	}
	e := c.entries[id]
	e.docP.Store(&next)
	s.markEntryMutated(e)
	s.noteEdgeMutation(coll)
	return nil
}

// Delete elimina el documento por _id.
func (s *Store) Delete(coll string, id string) error {
	if err := s.deleteDoc(nil, coll, id); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) deleteDoc(sess *Session, coll string, id string) error {
	return s.deleteDocFrame(nil, sess, coll, id)
}

func (s *Store) deleteDocFrame(frame *hookFrame, sess *Session, coll string, id string) error {
	var old Document
	if before := s.matchHooks(BeforeDelete, coll); len(before) > 0 {
		if err := s.previewWrite(sess, coll); err != nil {
			return err
		}
		var err error
		old, err = s.previewDoc(coll, id)
		if err != nil {
			return err
		}
		if err := s.runBeforeHooks(frame, before, BeforeDelete, coll, id, nil, old); err != nil {
			return err
		}
	}
	if err := s.deleteApply(sess, coll, id); err != nil {
		return err
	}
	s.queueAfterHook(frame, AfterDelete, coll, id, nil, old)
	return nil
}

func (s *Store) deleteApply(sess *Session, coll string, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	s.waitBackpressureLocked()
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	e, ok := c.entries[id]
	if !ok {
		return ErrNotFound
	}
	doc, err := s.loadEntry(e)
	if err != nil && !isNotFound(err) {
		return err
	}
	if doc != nil {
		for _, idx := range c.indexes {
			idx.removeDoc(doc, id)
		}
	}
	// Expulsar del registro de caché.
	s.cacheMu.Lock()
	if _, in := s.cache[e]; in {
		s.evictEntryLocked(e)
	}
	s.cacheMu.Unlock()
	delete(c.entries, id)
	s.markDirty()
	s.noteEdgeMutation(coll)
	// Guarda contra resurrección: registrar un DEL cuando existe un cuerpo duradero
	// o la instantánea de un flush podría incluir ya este documento.
	if s.sawFile || s.flushInProgress {
		s.pendingDels = append(s.pendingDels, delItem{coll: coll, id: id, gen: s.dirtyGen})
	}
	return nil
}

func isNotFound(err error) bool { return err == ErrNotFound }

// waitBackpressureLocked bloquea mientras hay un flush en curso y el número de
// mutaciones desde la última instantánea duradera supera Options.MaxPendingWrites.
// El llamador tiene s.mu (escritura). Wait libera s.mu para que el flush pueda terminar.
// No hace nada cuando MaxPendingWrites es 0 (comportamiento heredado, sin límite).
func (s *Store) waitBackpressureLocked() {
	limit := s.opts.maxPendingWrites()
	if limit <= 0 || s.bpCond == nil {
		return
	}
	for s.flushInProgress && int(s.writeSeq-s.durableSeq) >= limit {
		s.bpCond.Wait()
	}
}

// Get devuelve una copia del documento, o ErrNotFound.
// Cuando existe al menos 1 usuario (M4), las lecturas crudas del Store devuelven
// ErrUnauthorized: usa Authenticate → Session.Get.
func (s *Store) Get(coll string, id string) (Document, error) {
	return s.getDoc(nil, coll, id)
}

func (s *Store) getDoc(sess *Session, coll string, id string) (Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return nil, err
	}
	c, ok := s.collRO(coll)
	if !ok {
		return nil, ErrNotFound
	}
	doc, err := s.resident(c, id)
	if err != nil {
		return nil, err
	}
	out := clone(doc)
	if sess != nil {
		redactDoc(out, sess.fieldDenySet(coll))
	}
	return out, nil
}

// FindOptions controla la forma del resultado (sort/skip/limit desde M1; proyección M2).
type FindOptions struct {
	Sort       map[string]int // 1 ascendente, -1 descendente
	Limit      int
	Skip       int
	Projection []string // campos de primer nivel a devolver (+ _id siempre); vacío = todos
}

// Find usa una búsqueda por índice cuando es posible (igualdad/$in sobre un campo indexado);
// si no, recorre toda la colección coll. Un filtro nil/vacío devuelve todos los documentos.
// Operadores de filtro: ver filter.go ($eq $ne $gt $gte $lt $lte $in $nin $regex $exists $and $or $not $nor).
// Los resultados se ordenan por _id cuando no se indica Sort (determinista).
// Solo se clona la página devuelta (las referencias se mantienen durante sort/skip/limit).
// Cuando existe al menos 1 usuario (M4), las lecturas crudas del Store devuelven ErrUnauthorized.
func (s *Store) Find(coll string, filter Document, opts *FindOptions) ([]Document, error) {
	return s.findDocs(nil, coll, filter, opts)
}

func (s *Store) findDocs(sess *Session, coll string, filter Document, opts *FindOptions) ([]Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return nil, err
	}
	// fieldDeny de la sesión: prohíbe filtros que sondean campos ocultos (M4).
	var deny map[string]bool
	if sess != nil {
		deny = sess.fieldDenySet(coll)
		if len(deny) > 0 && filterTouchesFields(deny, filter) {
			return nil, ErrForbidden
		}
	}
	c, ok := s.collRO(coll)
	if !ok {
		return nil, nil
	}

	// Se mantienen punteros al almacén durante el match/sort; solo se clona la página de salida.
	var refs []Document
	plan := planCandidates(c, filter)
	if plan.used {
		var idList []string
		if plan.order != nil {
			idList = plan.order
		} else {
			idList = make([]string, 0, len(plan.cand))
			for id := range plan.cand {
				idList = append(idList, id)
			}
		}
		// exact ⇒ los candidatos ya cumplen el filtro (sin volver a evaluar).
		// !exact ⇒ se vuelve a evaluar cada documento cargado (rango / prefijo parcial).
		var err error
		refs, err = s.materializeRefs(c, idList, filter, !plan.exact)
		if err != nil {
			return nil, err
		}
	} else {
		ids := make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var err error
		refs, err = s.materializeRefs(c, ids, filter, true)
		if err != nil {
			return nil, err
		}
	}

	// Orden: atajo con el orden del índice cuando el campo de ordenación == campo del índice de rango.
	sortedByIndex := false
	if opts != nil && opts.Sort != nil && len(opts.Sort) == 1 && plan.order != nil {
		for sf, dir := range opts.Sort {
			if sf == plan.orderField {
				if dir < 0 {
					reverseRefs(refs)
				}
				sortedByIndex = true
			}
		}
	}
	// Orden base determinista por _id solo cuando no hay Sort de usuario (o se hará una ordenación completa después).
	if !sortedByIndex && (opts == nil || opts.Sort == nil || len(opts.Sort) == 0) {
		sortRefsByID(refs)
	}

	if opts != nil {
		if opts.Sort != nil && len(opts.Sort) > 0 && !sortedByIndex {
			need := 0
			if opts.Limit > 0 {
				need = opts.Skip + opts.Limit
			}
			sortByPartial(refs, opts.Sort, need)
		}
		if opts.Skip > 0 {
			if opts.Skip >= len(refs) {
				return []Document{}, nil
			}
			refs = refs[opts.Skip:]
		}
		if opts.Limit > 0 && opts.Limit < len(refs) {
			refs = refs[:opts.Limit]
		}
	}

	// Se clona solo lo que recibe quien llama (la página, no todo el conjunto de coincidencias).
	out := make([]Document, len(refs))
	for i, doc := range refs {
		if opts != nil && len(opts.Projection) > 0 {
			out[i] = project(doc, opts.Projection)
		} else {
			out[i] = clone(doc)
		}
	}
	if len(deny) > 0 {
		for _, d := range out {
			redactDoc(d, deny)
		}
	}
	return out, nil
}

func sortRefsByID(refs []Document) {
	sort.Slice(refs, func(i, j int) bool {
		return refs[i]["_id"].(string) < refs[j]["_id"].(string)
	})
}

func reverseRefs(refs []Document) {
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
}

// sortByPartial ordena los documentos según sortSpec. Si need>0 y need<n y la especificación
// tiene una sola clave, usa un max-heap acotado de tamaño need (O(n log k)); si no, ordenación completa.
// Tras una ordenación parcial, solo los primeros min(need,n) elementos quedan correctamente ordenados.
func sortByPartial(docs []Document, spec map[string]int, need int) {
	if need <= 0 || need >= len(docs) || len(spec) != 1 {
		sortBy(docs, spec)
		return
	}
	var field string
	var dir int
	for k, d := range spec {
		field, dir = k, d
	}
	// max-heap de (clave, doc) que conserva los "need" mejores según dir
	h := &docHeap{dir: dir}
	for _, d := range docs {
		h.push(d, d[field])
		if h.Len() > need {
			h.pop()
		}
	}
	// se extraen en orden a docs[0:need]; el resto queda como estaba (cola desordenada)
	n := h.Len()
	for i := n - 1; i >= 0; i-- {
		docs[i] = h.pop()
	}
}

// docHeap conserva los "need" mejores documentos: para asc (dir>0) expulsa el mayor; para desc, el menor.
type docHeap struct {
	keys []any
	docs []Document
	dir  int // 1 ascendente, -1 descendente
}

func (h *docHeap) Len() int { return len(h.docs) }

// betterKey indica si la clave a es preferible a b (a debe conservarse en lugar de b).
// Ascendente: la clave menor es mejor; Descendente: la clave mayor es mejor.
func (h *docHeap) betterKey(a, b any) bool {
	cmp := compareValues(a, b)
	if h.dir < 0 {
		return cmp > 0
	}
	return cmp < 0
}

// worstIdx devuelve el índice del peor elemento conservado (candidato a expulsar).
func (h *docHeap) worstIdx() int {
	wi := 0
	for i := 1; i < len(h.keys); i++ {
		// si keys[wi] es mejor que keys[i], entonces i es peor
		if h.betterKey(h.keys[wi], h.keys[i]) {
			wi = i
		}
	}
	return wi
}

func (h *docHeap) push(d Document, k any) {
	h.docs = append(h.docs, d)
	h.keys = append(h.keys, k)
}

// pop elimina y devuelve el peor elemento conservado.
func (h *docHeap) pop() Document {
	wi := h.worstIdx()
	d := h.docs[wi]
	h.docs = append(h.docs[:wi], h.docs[wi+1:]...)
	h.keys = append(h.keys[:wi], h.keys[wi+1:]...)
	return d
}

// planResult describe cómo usó el planificador los índices para un filtro.
type planResult struct {
	cand       map[string]struct{}
	order      []string // IDs en orden de índice cuando los produjo una búsqueda por rango
	orderField string   // campo indexado correspondiente a order (ordenar por índice)
	exact      bool     // no hace falta volver a evaluar
	used       bool     // se usó al menos una búsqueda por índice
	index      []string // campos de índice usados (Explain)
	covered    []string // campos de filtro de primer nivel cubiertos por búsquedas de igualdad
}

// fieldPlan es un campo de filtro de primer nivel clasificado por el planificador.
type fieldPlan struct {
	field   string
	eqVals  []any
	isEq    bool
	rb      rangeBound
	isRange bool
	blocked bool // operación lógica o condición no soportada
}

// planCandidates busca índices para igualdad/$in (simple + prefijo compuesto) y
// rangos sobre el primer campo indexado. Los rangos siempre ponen exact=false para
// que Find vuelva a evaluar (tipos cruzados / predicados residuales). Las operaciones
// lógicas fuerzan la reevaluación pero no bloquean las búsquedas de otros campos.
func planCandidates(c *collection, filter Document) planResult {
	res := planResult{exact: true}
	if len(filter) == 0 || len(c.indexes) == 0 {
		return planResult{exact: len(filter) == 0, used: false}
	}

	// Clasificar los campos de primer nivel.
	plans := make([]fieldPlan, 0, len(filter))
	for field, cond := range filter {
		if isLogicalOp(field) {
			plans = append(plans, fieldPlan{field: field, blocked: true})
			continue
		}
		if vals, ok := equalityValues(cond); ok {
			plans = append(plans, fieldPlan{field: field, eqVals: vals, isEq: true})
			continue
		}
		if rb, ok := rangeValues(cond); ok {
			plans = append(plans, fieldPlan{field: field, rb: rb, isRange: true})
			continue
		}
		plans = append(plans, fieldPlan{field: field, blocked: true})
	}

	// Marcar los campos consumidos por una búsqueda de igualdad compuesta de varios campos.
	consumed := map[string]bool{}

	// Paso 1: índices compuestos — igualdades iniciales de un solo valor.
	for _, idx := range c.indexes {
		if len(idx.Fields) < 2 {
			continue
		}
		vals := make([]any, 0, len(idx.Fields))
		var coveredFields []string
		for _, f := range idx.Fields {
			v, ok := singleEqualityFor(plans, f)
			if !ok {
				break
			}
			vals = append(vals, v)
			coveredFields = append(coveredFields, f)
		}
		if len(coveredFields) == 0 {
			continue
		}
		got := idx.seekPrefix(vals)
		if res.cand == nil {
			res.cand = got
		} else {
			intersectIDs(res.cand, got)
		}
		res.used = true
		res.index = idx.Fields
		for _, f := range coveredFields {
			consumed[f] = true
			res.covered = append(res.covered, f)
		}
	}
	// Exacto solo si todos los campos cubiertos están servidos por igualdad; los rangos/bloqueados limpian exact más abajo.

	// Paso 2: igualdad y rangos restantes de un solo campo.
	for _, fp := range plans {
		if fp.blocked {
			res.exact = false
			continue
		}
		if consumed[fp.field] {
			continue
		}
		if fp.isEq {
			got, idxFields, ok := seekEqualityField(c, fp.field, fp.eqVals)
			if !ok {
				res.exact = false
				continue
			}
			if res.cand == nil {
				res.cand = got
			} else {
				intersectIDs(res.cand, got)
			}
			res.used = true
			res.index = idxFields
			res.covered = append(res.covered, fp.field)
			continue
		}
		if fp.isRange {
			idx := findIndexForRange(c, fp.field)
			if idx == nil {
				res.exact = false
				continue
			}
			ids := idx.seekRange(fp.rb)
			got := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				got[id] = struct{}{}
			}
			if res.cand == nil {
				res.cand = got
				res.order = ids
				res.orderField = fp.field
			} else {
				// Conservar el orden del rango filtrado por los candidatos existentes.
				filtered := make([]string, 0, len(ids))
				for _, id := range ids {
					if _, ok := res.cand[id]; ok {
						filtered = append(filtered, id)
					}
				}
				intersectIDs(res.cand, got)
				res.order = filtered
				res.orderField = fp.field
			}
			res.used = true
			res.index = idx.Fields
			res.exact = false // los rangos siempre vuelven a evaluar
			continue
		}
		res.exact = false
	}

	if !res.used {
		return planResult{exact: false, used: false}
	}
	// Reconciliar los IDs ordenados con el conjunto final de candidatos (la igualdad pudo quitar algunos).
	if res.order != nil && res.cand != nil {
		filtered := res.order[:0]
		for _, id := range res.order {
			if _, ok := res.cand[id]; ok {
				filtered = append(filtered, id)
			}
		}
		res.order = filtered
	}
	// Todos los campos de primer nivel deben estar cubiertos por igualdad para que el plan sea exacto.
	if len(res.covered) != len(filter) {
		res.exact = false
	}
	return res
}

// singleEqualityFor devuelve un único valor literal de igualdad para un campo a partir de los planes.
func singleEqualityFor(plans []fieldPlan, field string) (any, bool) {
	for _, fp := range plans {
		if fp.field == field && fp.isEq && len(fp.eqVals) == 1 {
			return fp.eqVals[0], true
		}
	}
	return nil, false
}

// seekEqualityField busca $eq/literal/$in sobre field mediante un índice de un solo campo
// o como primer campo de un índice compuesto.
func seekEqualityField(c *collection, field string, vals []any) (map[string]struct{}, []string, bool) {
	if idx := findIndexForField(c, field); idx != nil {
		got := map[string]struct{}{}
		for _, v := range vals {
			for id := range idx.lookupIDs([]any{v}) {
				got[id] = struct{}{}
			}
		}
		return got, idx.Fields, true
	}
	// Índice compuesto con field como componente inicial ($in → unión de búsquedas por prefijo).
	for _, idx := range c.indexes {
		if len(idx.Fields) > 1 && idx.Fields[0] == field {
			got := map[string]struct{}{}
			for _, v := range vals {
				for id := range idx.seekPrefix([]any{v}) {
					got[id] = struct{}{}
				}
			}
			return got, idx.Fields, true
		}
	}
	return nil, nil, false
}

func findIndexForRange(c *collection, field string) *index {
	for _, idx := range c.indexes {
		if idx.Fields[0] == field {
			return idx
		}
	}
	return nil
}

func intersectIDs(dst, src map[string]struct{}) {
	for id := range dst {
		if _, ok := src[id]; !ok {
			delete(dst, id)
		}
	}
}

// rangeValues extrae una conjunción pura de rango (solo $gt/$gte/$lt/$lte)
// de la condición de un campo. Si se mezcla con otros operadores → no indexable como rango.
func rangeValues(cond any) (rangeBound, bool) {
	var ops Document
	switch t := cond.(type) {
	case Document:
		ops = t
	case map[string]any:
		ops = Document(t)
	default:
		return rangeBound{}, false
	}
	if !hasOpKeys(ops) {
		return rangeBound{}, false
	}
	var rb rangeBound
	n := 0
	for op, arg := range ops {
		switch op {
		case "$gt":
			if rb.hasLo {
				return rangeBound{}, false
			}
			rb.hasLo, rb.lo, rb.loIncl = true, arg, false
			n++
		case "$gte":
			if rb.hasLo {
				return rangeBound{}, false
			}
			rb.hasLo, rb.lo, rb.loIncl = true, arg, true
			n++
		case "$lt":
			if rb.hasHi {
				return rangeBound{}, false
			}
			rb.hasHi, rb.hi, rb.hiIncl = true, arg, false
			n++
		case "$lte":
			if rb.hasHi {
				return rangeBound{}, false
			}
			rb.hasHi, rb.hi, rb.hiIncl = true, arg, true
			n++
		default:
			return rangeBound{}, false
		}
	}
	if n == 0 {
		return rangeBound{}, false
	}
	return rb, true
}

// ExplainResult informa de cómo ejecutaría Find un filtro (punto 5).
type ExplainResult struct {
	Collection     string   `json:"collection"`
	Plan           string   `json:"plan"` // COLLSCAN | IXSCAN
	Index          []string `json:"index,omitempty"`
	Covered        []string `json:"covered,omitempty"`
	Candidates     int      `json:"candidates"`
	Exact          bool     `json:"exact"`
	ReMatch        bool     `json:"re_match"`
	FilterFields   []string `json:"filter_fields"`
	CollectionSize int      `json:"collection_size"`
}

// Explain devuelve el plan de consulta del filtro sin ejecutar el match sobre los documentos.
func (s *Store) Explain(coll string, filter Document) ExplainResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := ExplainResult{Collection: coll, Plan: "COLLSCAN"}
	for f := range filter {
		res.FilterFields = append(res.FilterFields, f)
	}
	sort.Strings(res.FilterFields)
	c, ok := s.collRO(coll)
	if !ok {
		res.ReMatch = true
		return res
	}
	res.CollectionSize = len(c.entries)
	if len(filter) == 0 {
		res.Plan = "COLLSCAN"
		res.Candidates = len(c.entries)
		res.Exact = true
		res.ReMatch = false
		return res
	}
	plan := planCandidates(c, filter)
	if plan.used {
		res.Plan = "IXSCAN"
		res.Index = plan.index
		res.Covered = plan.covered
		res.Candidates = len(plan.cand)
		res.Exact = plan.exact
		res.ReMatch = !plan.exact
	} else {
		res.Candidates = len(c.entries)
		res.ReMatch = true
	}
	return res
}

// equalityValues devuelve valores literales para field: value, { $eq: v } o { $in: [...] }.
func equalityValues(cond any) ([]any, bool) {
	if ops, ok := cond.(Document); ok && hasOpKeys(ops) {
		if eq, has := ops["$eq"]; has && len(ops) == 1 {
			return []any{eq}, true
		}
		if in, has := ops["$in"]; has && len(ops) == 1 {
			list, err := toAnyList(in)
			if err != nil {
				return nil, false
			}
			return list, true
		}
		return nil, false
	}
	if ops, ok := cond.(map[string]any); ok && hasOpKeys(Document(ops)) {
		return equalityValues(Document(ops))
	}
	return []any{cond}, true
}

func findIndexForField(c *collection, field string) *index {
	for _, idx := range c.indexes {
		if len(idx.Fields) == 1 && idx.Fields[0] == field {
			return idx
		}
	}
	return nil
}

// project conserva solo _id + los campos de primer nivel indicados (copia en profundidad de los valores).
func project(doc Document, fields []string) Document {
	out := Document{}
	if v, ok := doc["_id"]; ok {
		out["_id"] = v
	}
	for _, f := range fields {
		if f == "_id" {
			continue
		}
		if v, ok := doc[f]; ok {
			out[f] = cloneValue(v)
		}
	}
	return out
}

// Count devuelve el número de documentos que coinciden sin clonarlos.
// Igualdad/$in puros sobre campos indexados ⇒ O(1)/O(k) desde el índice (sin recorrido).
// Cuando existe al menos 1 usuario (M4), las lecturas crudas del Store devuelven ErrUnauthorized.
func (s *Store) Count(coll string, filter Document) (int, error) {
	return s.countDocs(nil, coll, filter)
}

func (s *Store) countDocs(sess *Session, coll string, filter Document) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return 0, err
	}
	if sess != nil {
		deny := sess.fieldDenySet(coll)
		if len(deny) > 0 && filterTouchesFields(deny, filter) {
			return 0, ErrForbidden
		}
	}
	c, ok := s.collRO(coll)
	if !ok {
		return 0, nil
	}
	if len(filter) > 0 {
		// Camino rápido: igualdad/$in de un solo campo totalmente cubierto por un índice.
		if n, hit := countIndexed(c, filter); hit {
			return n, nil
		}
	}
	plan := planCandidates(c, filter)
	if plan.used {
		if plan.exact {
			return len(plan.cand), nil
		}
		var idList []string
		if plan.order != nil {
			idList = plan.order
		} else {
			idList = make([]string, 0, len(plan.cand))
			for id := range plan.cand {
				idList = append(idList, id)
			}
		}
		return s.countMatches(c, idList, filter, true)
	}
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return s.countMatches(c, ids, filter, true)
}

// countIndexed resuelve Count cuando el filtro es un único campo de primer nivel con
// solo igualdad o $in, y ese campo tiene un índice de un solo campo.
func countIndexed(c *collection, filter Document) (int, bool) {
	if len(filter) != 1 {
		return 0, false
	}
	for field, cond := range filter {
		if isLogicalOp(field) {
			return 0, false
		}
		vals, ok := equalityValues(cond)
		if !ok {
			return 0, false
		}
		idx := findIndexForField(c, field)
		if idx == nil {
			return 0, false
		}
		// $in / igualdad multivalor: sumar por valor (las claves únicas no se solapan entre valores)
		n := 0
		for _, v := range vals {
			n += idx.countKey([]any{v})
		}
		return n, true
	}
	return 0, false
}

// Collections lista los nombres de colecciones (ordenados). Las colecciones del sistema
// (_users/_roles, M4) quedan ocultas.
func (s *Store) Collections() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.collections))
	for n := range s.collections {
		if isSystemColl(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SchemaVersion devuelve la versión del esquema del almacén.
func (s *Store) SchemaVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schemaVersion
}

// Path devuelve la ruta del archivo de respaldo ("" en almacenes en memoria). Admin M9.
func (s *Store) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// CreateCollection registra una colección vacía (wire M8). Sobrevive a flush/reopen
// mediante el registro META incluso sin documentos. Devuelve ErrExists cuando la
// colección ya existe.
func (s *Store) CreateCollection(coll string) error {
	return s.createCollection(nil, coll)
}

func (s *Store) createCollection(sess *Session, coll string) error {
	if coll == "" || strings.HasPrefix(coll, "$") || strings.ContainsRune(coll, 0) {
		return ErrForbidden
	}
	if isSystemColl(coll) {
		return ErrForbidden
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkWriteLocked(sess, coll); err != nil {
		return err
	}
	if _, ok := s.collections[coll]; ok {
		return ErrExists
	}
	s.collections[coll] = &collection{entries: map[string]*docEntry{}}
	s.markDirty()
	return nil
}

// DropCollection elimina una colección y todos sus documentos (wire M8).
// Cada documento pasa por el camino normal de borrado (registros DEL, hooks,
// expulsión de caché); después la definición de la colección sale del mapa y el
// siguiente META del flush deja de listarla. Los registros DOC/IDX obsoletos de
// commits anteriores se ignoran al reabrir (el escaneo v2 está condicionado por META).
// Los hooks pueden vetar borrados individuales, dejando la colección parcialmente eliminada.
func (s *Store) DropCollection(coll string) error {
	return s.dropCollection(nil, coll)
}

func (s *Store) dropCollection(sess *Session, coll string) error {
	if coll == "" || isSystemColl(coll) {
		return ErrForbidden
	}
	if err := s.previewWrite(sess, coll); err != nil {
		return err
	}
	s.mu.RLock()
	var ids []string
	if c, ok := s.collections[coll]; ok {
		ids = make([]string, 0, len(c.entries))
		for id := range c.entries {
			ids = append(ids, id)
		}
	} else {
		s.mu.RUnlock()
		return ErrNotFound
	}
	s.mu.RUnlock()
	sort.Strings(ids)
	for _, id := range ids {
		if err := s.deleteDoc(sess, coll, id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if c, ok := s.collections[coll]; ok {
		s.purgeCollCacheLocked(c)
		delete(s.collections, coll)
		s.markDirty()
	}
	s.mu.Unlock()
	if isEdgeColl(coll) {
		s.graphMu.Lock()
		s.graphEpoch.Add(1)
		s.graphMu.Unlock()
	}
	return s.afterMutation()
}

// matchEqual se eliminó — lo sustituye match() en filter.go (M2).

func equalJSON(a, b any) bool {
	// Los números que vienen de JSON son float64; se acepta comparación cruzada int/float.
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

func sortBy(docs []Document, sortSpec map[string]int) {
	if len(docs) < 2 {
		return
	}
	// Desempate por _id primero (prioridad más baja — las ordenaciones estables posteriores lo mantienen para claves iguales).
	// Después las claves de usuario en orden alfabético inverso para que gane la primera clave alfabética.
	sort.SliceStable(docs, func(a, b int) bool {
		return docs[a]["_id"].(string) < docs[b]["_id"].(string)
	})

	ordered := sortedKeys(sortSpec)
	for i := len(ordered) - 1; i >= 0; i-- {
		k := ordered[i]
		dir := sortSpec[k]
		// precalcular las claves de esta pasada
		ks := make([]any, len(docs))
		for di, d := range docs {
			ks[di] = d[k]
		}
		sort.SliceStable(docs, func(a, b int) bool {
			cmp := compareValues(ks[a], ks[b])
			if dir < 0 {
				return cmp > 0
			}
			return cmp < 0
		})
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func compareValues(a, b any) int {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	as, aIsStr := a.(string)
	bs, bIsStr := b.(string)
	if aIsStr && bIsStr {
		switch {
		case as < bs:
			return -1
		case as > bs:
			return 1
		default:
			return 0
		}
	}
	// null/ausente al final: nil < todo
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	if b == nil {
		return 1
	}
	return 0
}
