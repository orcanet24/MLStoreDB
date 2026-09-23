package db

import (
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// triggersColl holds declarative trigger definitions (M5b system coll).
const triggersColl = "_triggers"

// TriggerAction is one declarative step. Types:
//   set     — merge Fields into the live doc (before: mutate Doc; after: self-Update)
//   unset   — remove Names from the doc (before_* events only)
//   insert  — insert Doc (with $get templates) into Collection (auto _id if absent)
//   update  — patch Set into target id (target must exist)
//   upsert  — replace/patch Set at target id
//   delete  — delete target id
//
// Template: any value {"$get":"field"} resolves against the event Doc;
// {"$get":"old.field"} resolves against Old.
type TriggerAction struct {
	Type       string         `json:"type"`
	Collection string         `json:"collection,omitempty"`
	ID         any            `json:"id,omitempty"` // string or {"$get":...}
	Doc        map[string]any `json:"doc,omitempty"`
	Fields     map[string]any `json:"fields,omitempty"`
	Names      []string       `json:"names,omitempty"`
	Set        map[string]any `json:"set,omitempty"`
}

// Trigger is a declarative rule stored in _triggers and registered as an
// M5a hook (reuses depth/cycle/queue machinery).
type Trigger struct {
	ID         string         `json:"_id"`
	Event      string         `json:"event"`
	Collection string         `json:"collection"` // "" or "*" = all
	Enabled    *bool          `json:"enabled,omitempty"`
	Async      bool           `json:"async,omitempty"` // after_* only: run on FIFO queue
	Filter     map[string]any `json:"filter,omitempty"`
	Actions    []TriggerAction `json:"actions"`
}

func (t Trigger) enabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// CreateTrigger validates, persists into _triggers and registers the
// underlying M5a hook. Returns the trigger _id.
func (s *Store) CreateTrigger(t Trigger) (string, error) {
	if err := validateTrigger(t); err != nil {
		return "", err
	}
	if t.ID == "" {
		t.ID = ulid.Make().String()
	}
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	s.ensureTriggersLoadedLocked()
	doc, err := triggerToDoc(t)
	if err != nil {
		return "", err
	}
	if err := s.adminInsert(triggersColl, doc); err != nil {
		return "", err
	}
	if t.enabled() {
		if err := s.registerTriggerHook(t); err != nil {
			return "", err
		}
	}
	return t.ID, nil
}

// DeleteTrigger removes the _triggers doc and unhooks it.
func (s *Store) DeleteTrigger(id string) error {
	if id == "" {
		return ErrNotFound
	}
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	if err := s.adminDelete(triggersColl, id); err != nil {
		return err
	}
	if hookID, ok := s.triggerHooks[id]; ok {
		s.Off(hookID)
		delete(s.triggerHooks, id)
	}
	return nil
}

// ListTriggers returns all stored trigger definitions.
func (s *Store) ListTriggers() ([]Trigger, error) {
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	s.ensureTriggersLoadedLocked()
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collections[triggersColl]
	if !ok {
		return nil, nil
	}
	out := make([]Trigger, 0, len(c.entries))
	for id := range c.entries {
		d, err := s.resident(c, id)
		if err != nil {
			continue
		}
		var t Trigger
		if err := docToTrigger(d, &t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func validateTrigger(t Trigger) error {
	if !validHookEvent(t.Event) {
		return fmt.Errorf("db: unknown trigger event %q", t.Event)
	}
	if t.Async {
		switch t.Event {
		case AfterInsert, AfterUpdate, AfterDelete, AfterUpsert:
		default:
			return fmt.Errorf("db: async trigger requires after-* event, got %q", t.Event)
		}
	}
	if len(t.Actions) == 0 {
		return fmt.Errorf("db: trigger needs at least one action")
	}
	for i, a := range t.Actions {
		if err := validateTriggerAction(a, t.Event); err != nil {
			return fmt.Errorf("db: trigger action %d: %w", i, err)
		}
	}
	return nil
}

func validateTriggerAction(a TriggerAction, event string) error {
	switch a.Type {
	case "set":
		if len(a.Fields) == 0 {
			return fmt.Errorf("set requires fields")
		}
	case "unset":
		if len(a.Names) == 0 {
			return fmt.Errorf("unset requires names")
		}
		switch event {
		case BeforeInsert, BeforeUpdate, BeforeUpsert, BeforeDelete:
		default:
			return fmt.Errorf("unset only valid on before-* events (got %q)", event)
		}
	case "insert":
		if a.Collection == "" || a.Collection == "*" {
			return fmt.Errorf("insert requires target collection")
		}
		if len(a.Doc) == 0 {
			return fmt.Errorf("insert requires doc")
		}
	case "update", "upsert":
		if a.Collection == "" || a.Collection == "*" {
			return fmt.Errorf("%s requires target collection", a.Type)
		}
		if a.ID == nil {
			return fmt.Errorf("%s requires id", a.Type)
		}
		if len(a.Set) == 0 {
			return fmt.Errorf("%s requires set", a.Type)
		}
	case "delete":
		if a.Collection == "" || a.Collection == "*" {
			return fmt.Errorf("delete requires target collection")
		}
		if a.ID == nil {
			return fmt.Errorf("delete requires id")
		}
	default:
		return fmt.Errorf("unknown action type %q", a.Type)
	}
	return nil
}

// registerTriggerHook wires one Trigger onto the M5a registry.
// Caller holds trigMu (and has finished ensureTriggersLoadedLocked).
func (s *Store) registerTriggerHook(t Trigger) error {
	fn := func(h *HookContext) error {
		return s.runTrigger(&t, h)
	}
	var (
		hookID uint64
		err    error
	)
	if t.Async {
		hookID, err = s.OnAsync(t.Event, t.Collection, fn)
	} else {
		hookID, err = s.On(t.Event, t.Collection, fn)
	}
	if err != nil {
		return err
	}
	s.triggerHooks[t.ID] = hookID
	return nil
}

// ensureTriggersLoaded registers hooks for persisted triggers once (idempotent).
func (s *Store) ensureTriggersLoaded() {
	s.trigMu.Lock()
	defer s.trigMu.Unlock()
	s.ensureTriggersLoadedLocked()
}

// ensureTriggersLoadedLocked registers hooks for persisted triggers once.
// Caller holds trigMu.
func (s *Store) ensureTriggersLoadedLocked() {
	if s.triggersLoaded {
		return
	}
	s.triggersLoaded = true
	if s.triggerHooks == nil {
		s.triggerHooks = map[string]uint64{}
	}
	s.mu.RLock()
	var docs []Document
	if c, ok := s.collections[triggersColl]; ok {
		for id := range c.entries {
			if d, err := s.resident(c, id); err == nil {
				docs = append(docs, clone(d))
			}
		}
	}
	s.mu.RUnlock()
	for _, d := range docs {
		var t Trigger
		if err := docToTrigger(d, &t); err != nil {
			continue
		}
		if !t.enabled() || t.ID == "" {
			continue
		}
		_ = s.registerTriggerHook(t)
	}
}

// runTrigger evaluates filter + actions inside an M5a HookContext.
func (s *Store) runTrigger(t *Trigger, h *HookContext) error {
	target := h.Doc
	if target == nil {
		target = h.Old
	}
	if len(t.Filter) > 0 {
		if target == nil {
			return nil
		}
		ok, err := match(target, Document(t.Filter))
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	for _, a := range t.Actions {
		if err := s.runTriggerAction(a, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) runTriggerAction(a TriggerAction, h *HookContext) error {
	switch a.Type {
	case "set":
		fields := resolveMap(a.Fields, h.Doc, h.Old)
		isBefore := len(h.Event) >= 6 && h.Event[:6] == "before"
		if isBefore {
			if h.Doc == nil {
				return nil
			}
			for k, v := range fields {
				if k == "_id" {
					continue
				}
				h.Doc[k] = v
			}
			return nil
		}
		// after_*: apply as self-update (Doc may be a patch for update events)
		if h.ID == "" {
			return nil
		}
		return h.Update(h.Collection, h.ID, fields)

	case "unset":
		if h.Doc == nil {
			return nil
		}
		for _, n := range a.Names {
			delete(h.Doc, n)
		}
		return nil

	case "insert":
		doc := resolveMap(a.Doc, h.Doc, h.Old)
		out := make(Document, len(doc))
		for k, v := range doc {
			out[k] = v
		}
		if _, ok := out["_id"]; !ok {
			out["_id"] = ulid.Make().String()
		}
		return h.Insert(a.Collection, out)

	case "update":
		id, err := resolveID(a.ID, h.Doc, h.Old)
		if err != nil {
			return err
		}
		return h.Update(a.Collection, id, resolveMap(a.Set, h.Doc, h.Old))

	case "upsert":
		id, err := resolveID(a.ID, h.Doc, h.Old)
		if err != nil {
			return err
		}
		fields := resolveMap(a.Set, h.Doc, h.Old)
		doc := Document{"_id": id}
		for k, v := range fields {
			doc[k] = v
		}
		return h.Upsert(a.Collection, id, doc)

	case "delete":
		id, err := resolveID(a.ID, h.Doc, h.Old)
		if err != nil {
			return err
		}
		return h.Delete(a.Collection, id)
	}
	return fmt.Errorf("db: unknown trigger action %q", a.Type)
}

// resolveMap deep-resolves {"$get":...} templates against doc/old.
func resolveMap(m map[string]any, doc, old Document) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = resolveVal(v, doc, old)
	}
	return out
}

func resolveVal(v any, doc, old Document) any {
	switch t := v.(type) {
	case map[string]any:
		if g, ok := t["$get"].(string); ok && len(t) == 1 {
			return getRef(g, doc, old)
		}
		return resolveMap(t, doc, old)
	case Document:
		if g, ok := t["$get"].(string); ok && len(t) == 1 {
			return getRef(g, doc, old)
		}
		return resolveMap(t, doc, old)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = resolveVal(e, doc, old)
		}
		return out
	default:
		return v
	}
}

func getRef(ref string, doc, old Document) any {
	src := doc
	if len(ref) > 4 && ref[:4] == "old." {
		src = old
		ref = ref[4:]
	}
	if src == nil {
		return nil
	}
	return src[ref]
}

func resolveID(v any, doc, old Document) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case map[string]any:
		if g, ok := t["$get"].(string); ok {
			if r := getRef(g, doc, old); r != nil {
				if s, ok := r.(string); ok {
					return s, nil
				}
			}
			return "", fmt.Errorf("db: trigger id template %q did not resolve to string", g)
		}
	}
	return "", fmt.Errorf("db: trigger id must be string or $get")
}

func triggerToDoc(t Trigger) (Document, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return d, nil
}

func docToTrigger(d Document, t *Trigger) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, t)
}

// adminInsert writes a system-coll doc without RBAC (admin APIs only).
// Does not fire hooks (trigger definitions are not events).
func (s *Store) adminInsert(coll string, doc Document) error {
	id, err := docID(doc)
	if err != nil {
		return err
	}
	if err := checkDocSize(doc); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	return nil
}

// adminDelete removes a system-coll doc without RBAC (admin APIs only).
func (s *Store) adminDelete(coll string, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.cacheMu.Lock()
	if _, in := s.cache[e]; in {
		s.evictEntryLocked(e)
	}
	s.cacheMu.Unlock()
	delete(c.entries, id)
	s.markDirty()
	if s.sawFile || s.flushInProgress {
		s.pendingDels = append(s.pendingDels, delItem{coll: coll, id: id, gen: s.dirtyGen})
	}
	return nil
}
