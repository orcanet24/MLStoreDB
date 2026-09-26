package db

import (
	"fmt"
)

// Eventos de hook (M5a). Ids de tipo string para que los triggers JSON de M5b puedan reutilizarlos.
const (
	BeforeInsert = "before_insert"
	AfterInsert  = "after_insert"
	BeforeUpdate = "before_update"
	AfterUpdate  = "after_update"
	BeforeDelete = "before_delete"
	AfterDelete  = "after_delete"
	BeforeUpsert = "before_upsert"
	AfterUpsert  = "after_upsert"
)

const (
	// maxHookDepth acota las mutaciones anidadas disparadas por hooks (M5a).
	maxHookDepth = 8
	// hookQueueSize es la FIFO acotada de trabajos de OnAsync.
	hookQueueSize = 1024
)

// HookFunc se ejecuta para un evento coincidente. Los hooks before-* corren de forma
// síncrona y pueden vetar la mutación devolviendo un error. Los hooks after-* corren
// después de aplicar la mutación (On: en línea, el error se ignora; OnAsync: en cola).
// Las mutaciones anidadas DEBEN pasar por los métodos de HookContext (guardas de profundidad/ciclo).
type HookFunc func(h *HookContext) error

// HookContext se pasa a un hook: carga útil del evento + acceso al Store con alcance.
type HookContext struct {
	Event      string
	Collection string
	ID         string
	Doc        Document // documento nuevo / patch (insert/update/upsert); nil para delete
	Old        Document // imagen previa (update/delete/upsert); nil para insert

	frame *hookFrame
	store *Store
}

// Get/Find/Count son ayudantes de lectura (se ejecutan como principal de confianza del hook).
func (h *HookContext) Get(coll string, id string) (Document, error) {
	return h.store.getDoc(hookBypass, coll, id)
}

func (h *HookContext) Find(coll string, filter Document, opts *FindOptions) ([]Document, error) {
	return h.store.findDocs(hookBypass, coll, filter, opts)
}

func (h *HookContext) Count(coll string, filter Document) (int, error) {
	return h.store.countDocs(hookBypass, coll, filter)
}

// Insert/Upsert/Update/Delete propagan el marco del hook (comprobaciones de profundidad + ciclo).
// Se ejecutan como hookBypass (de confianza; el RBAC no controla el código de los hooks).
func (h *HookContext) Insert(coll string, doc Document) error {
	return h.store.insertDocFrame(h.frame, hookBypass, coll, doc)
}

func (h *HookContext) Upsert(coll string, id string, doc Document) error {
	return h.store.upsertDocFrame(h.frame, hookBypass, coll, id, doc)
}

func (h *HookContext) Update(coll string, id string, patch Document) error {
	return h.store.updateDocFrame(h.frame, hookBypass, coll, id, patch, nil)
}

func (h *HookContext) Delete(coll string, id string) error {
	return h.store.deleteDocFrame(h.frame, hookBypass, coll, id)
}

// hookBypass es el principal de confianza para las mutaciones iniciadas por hooks.
// system=true ⇒ válido sin estar en el mapa de sesiones; el RBAC lo salta.
// Las operaciones del Store siguen rechazando las colecciones del sistema para todos.
var hookBypass = &Session{system: true, user: "_hook"}

// hookFrame enlaza una invocación de hook con sus ancestros (recorrido de profundidad + ciclo).
type hookFrame struct {
	parent *hookFrame
	hookID uint64
	depth  int
}

type hookReg struct {
	id    uint64
	event string
	coll  string // "" o "*" = todas las colecciones
	fn    HookFunc
	async bool
}

type hookJob struct {
	frame   *hookFrame
	reg     *hookReg
	event   string
	coll    string
	id      string
	doc     Document
	old     Document
	barrier chan struct{} // centinela de WaitAsyncHooks (reg == nil)
}

// On registra un hook síncrono. coll "" o "*" coincide con todas las colecciones.
// Los hooks before-* pueden vetar (devolver error); los errores de after-* se ignoran.
func (s *Store) On(event string, coll string, fn HookFunc) (uint64, error) {
	return s.addHook(event, coll, fn, false)
}

// OnAsync registra un hook after-* en la cola FIFO acotada del worker.
// Encolar bloquea cuando la cola está llena (contrapresión). Los before-* se rechazan:
// los hooks asíncronos no pueden vetar.
func (s *Store) OnAsync(event string, coll string, fn HookFunc) (uint64, error) {
	switch event {
	case AfterInsert, AfterUpdate, AfterDelete, AfterUpsert:
	default:
		return 0, fmt.Errorf("db: OnAsync requires after-* event, got %q", event)
	}
	return s.addHook(event, coll, fn, true)
}

func (s *Store) addHook(event string, coll string, fn HookFunc, async bool) (uint64, error) {
	if !validHookEvent(event) {
		return 0, fmt.Errorf("db: unknown hook event %q", event)
	}
	if fn == nil {
		return 0, fmt.Errorf("db: nil hook func")
	}
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.hookSeq++
	id := s.hookSeq
	s.hooks = append(s.hooks, &hookReg{id: id, event: event, coll: coll, fn: fn, async: async})
	return id, nil
}

// Off elimina un hook por id; indica si existía.
func (s *Store) Off(id uint64) bool {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	for i, r := range s.hooks {
		if r.id == id {
			s.hooks = append(s.hooks[:i], s.hooks[i+1:]...)
			return true
		}
	}
	return false
}

func validHookEvent(e string) bool {
	switch e {
	case BeforeInsert, AfterInsert, BeforeUpdate, AfterUpdate,
		BeforeDelete, AfterDelete, BeforeUpsert, AfterUpsert:
		return true
	}
	return false
}

func collMatch(pattern, coll string) bool {
	return pattern == "" || pattern == "*" || pattern == coll
}

// matchHooks devuelve los registros de event+coll. El llamador no debe tener hooksMu.
func (s *Store) matchHooks(event, coll string) []*hookReg {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	var out []*hookReg
	for _, r := range s.hooks {
		if r.event == event && collMatch(r.coll, coll) {
			out = append(out, r)
		}
	}
	return out
}

// beginHookErr valida profundidad + ciclo y construye el contexto hijo.
func (s *Store) beginHookErr(parent *hookFrame, r *hookReg) (*HookContext, error) {
	depth := 1
	if parent != nil {
		depth = parent.depth + 1
	}
	if depth > maxHookDepth {
		return nil, ErrHookDepth
	}
	for f := parent; f != nil; f = f.parent {
		if f.hookID == r.id {
			return nil, ErrHookCycle
		}
	}
	return &HookContext{
		frame: &hookFrame{parent: parent, hookID: r.id, depth: depth},
		store: s,
	}, nil
}

// runBeforeHooks ejecuta los registros before-* coincidentes; el primer error veta.
func (s *Store) runBeforeHooks(frame *hookFrame, regs []*hookReg, event, coll, id string, doc, old Document) error {
	for _, r := range regs {
		h, err := s.beginHookErr(frame, r)
		if err != nil {
			return err
		}
		h.Event, h.Collection, h.ID, h.Doc, h.Old = event, coll, id, doc, old
		if err := r.fn(h); err != nil {
			return err
		}
	}
	return nil
}

// runAfterHooksInline ejecuta los registros after-* síncronos (los errores se ignoran).
func (s *Store) runAfterHooksInline(frame *hookFrame, regs []*hookReg, event, coll, id string, doc, old Document) {
	for _, r := range regs {
		h, err := s.beginHookErr(frame, r)
		if err != nil {
			return
		}
		h.Event, h.Collection, h.ID, h.Doc, h.Old = event, coll, id, doc, old
		_ = r.fn(h)
	}
}

// queueAfterHook despacha los registros after-*: síncronos en línea, asíncronos a la cola FIFO.
// No hace nada si no coincide ningún registro (coste cero cuando no hay hooks).
func (s *Store) queueAfterHook(frame *hookFrame, event, coll, id string, doc, old Document) {
	regs := s.matchHooks(event, coll)
	if len(regs) == 0 {
		return
	}
	for _, r := range regs {
		if !r.async {
			s.runAfterHooksInline(frame, []*hookReg{r}, event, coll, id, doc, old)
			continue
		}
		s.ensureHookWorker()
		job := hookJob{
			frame: frame,
			reg:   r,
			event: event,
			coll:  coll,
			id:    id,
			doc:   cloneDocSafe(doc),
			old:   cloneDocSafe(old),
		}
		s.hooksMu.Lock()
		q, stop := s.hookQ, s.hookStop
		s.hooksMu.Unlock()
		if q == nil {
			return
		}
		select {
		case q <- job: // bloquea cuando está llena (contrapresión de la FIFO acotada)
		case <-stop:
			return
		}
	}
}

func cloneDocSafe(d Document) Document {
	if d == nil {
		return nil
	}
	return clone(d)
}

// ensureHookWorker arranca de forma perezosa el worker asíncrono de la FIFO.
func (s *Store) ensureHookWorker() {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	if s.hookQ != nil {
		return
	}
	q := make(chan hookJob, hookQueueSize)
	stop := make(chan struct{})
	done := make(chan struct{})
	s.hookQ, s.hookStop, s.hookDone = q, stop, done
	go s.hookWorker(q, stop, done)
}

// hookWorker drena la FIFO acotada. Los canales se pasan como argumentos (no se releen
// de los campos) para que stopHookWorker pueda anular los campos sin carrera.
func (s *Store) hookWorker(q <-chan hookJob, stop <-chan struct{}, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case job := <-q:
			if job.barrier != nil {
				close(job.barrier)
				continue
			}
			h, err := s.beginHookErr(job.frame, job.reg)
			if err != nil {
				continue // ciclo/profundidad en el camino asíncrono: descartar
			}
			h.Event, h.Collection, h.ID, h.Doc, h.Old =
				job.event, job.coll, job.id, job.doc, job.old
			_ = job.reg.fn(h)
		}
	}
}

// stopHookWorker detiene el worker asíncrono (Close). Descarta los trabajos no entregados.
func (s *Store) stopHookWorker() {
	s.hooksMu.Lock()
	stop, done := s.hookStop, s.hookDone
	s.hookQ, s.hookStop, s.hookDone = nil, nil, nil
	s.hooksMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// WaitAsyncHooks bloquea hasta que todos los trabajos asíncronos en cola terminan (barrera).
// Ayudante para tests/operaciones — no lo llames desde dentro de un hook (deadlock).
func (s *Store) WaitAsyncHooks() {
	s.hooksMu.Lock()
	q, stop := s.hookQ, s.hookStop
	s.hooksMu.Unlock()
	if q == nil {
		return
	}
	b := make(chan struct{})
	select {
	case q <- hookJob{barrier: b}:
	case <-stop:
		return
	}
	<-b
}
