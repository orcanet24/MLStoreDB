package db

import (
	"errors"
	"strings"
	"testing"
)

func TestTriggerBeforeSetAndVetoByFilter(t *testing.T) {
	s := New()
	// stamp every insert
	if _, err := s.CreateTrigger(Trigger{
		Event:      BeforeInsert,
		Collection: "orders",
		Actions: []TriggerAction{
			{Type: "set", Fields: map[string]any{"stamped": true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("orders", Document{"_id": "1", "status": "NEW"}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.Get("orders", "1")
	if err != nil {
		t.Fatal(err)
	}
	if doc["stamped"] != true {
		t.Errorf("set action lost: %v", doc)
	}

	// filter: only PAID gets vip; veto path via set on filtered subset
	if _, err := s.CreateTrigger(Trigger{
		Event:      BeforeInsert,
		Collection: "orders",
		Filter:     map[string]any{"status": "PAID"},
		Actions: []TriggerAction{
			{Type: "set", Fields: map[string]any{"vip": true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("orders", Document{"_id": "2", "status": "NEW"})
	_ = s.Insert("orders", Document{"_id": "3", "status": "PAID"})
	d2, _ := s.Get("orders", "2")
	d3, _ := s.Get("orders", "3")
	if _, ok := d2["vip"]; ok {
		t.Error("filter matched non-PAID")
	}
	if d3["vip"] != true {
		t.Errorf("PAID not stamped: %v", d3)
	}
}

func TestTriggerAfterInsertAuditWithTemplates(t *testing.T) {
	s := New()
	if _, err := s.CreateTrigger(Trigger{
		Event:      AfterInsert,
		Collection: "orders",
		Actions: []TriggerAction{
			{Type: "insert", Collection: "audit", Doc: map[string]any{
				"src":  "orders",
				"ref":  map[string]any{"$get": "_id"},
				"note": "created",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("orders", Document{"_id": "o-1", "buyer": "ana"}); err != nil {
		t.Fatal(err)
	}
	// after_insert is sync inline → audit exists immediately
	docs, err := s.Find("audit", nil, nil)
	if err != nil || len(docs) != 1 {
		t.Fatalf("audit = %v, %v", docs, err)
	}
	a := docs[0]
	if a["ref"] != "o-1" || a["src"] != "orders" || a["note"] != "created" {
		t.Errorf("audit doc = %v", a)
	}
	if a["_id"] == nil || a["_id"] == "" {
		t.Error("audit auto _id missing")
	}
}

func TestTriggerUnsetBeforeDelete(t *testing.T) {
	s := New()
	if err := s.Insert("q", Document{"_id": "1", "temp": "x", "keep": "y"}); err != nil {
		t.Fatal(err)
	}
	// before_delete mutates Old view only (doc about to vanish) — use before_update instead for unset demo
	if _, err := s.CreateTrigger(Trigger{
		Event:      BeforeUpdate,
		Collection: "q",
		Actions: []TriggerAction{
			{Type: "unset", Names: []string{"temp"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update("q", "1", Document{"keep": "z"}); err != nil {
		t.Fatal(err)
	}
	doc, _ := s.Get("q", "1")
	if _, ok := doc["temp"]; ok {
		t.Error("unset failed")
	}
	if doc["keep"] != "z" {
		t.Errorf("patch lost: %v", doc)
	}
}

func TestTriggerUpdateTargetAndOldRef(t *testing.T) {
	s := New()
	// counter doc
	if err := s.Insert("counters", Document{"_id": "orders", "n": float64(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTrigger(Trigger{
		Event:      AfterInsert,
		Collection: "orders",
		Actions: []TriggerAction{
			{Type: "upsert", Collection: "counters", ID: "orders",
				Set: map[string]any{"last": map[string]any{"$get": "_id"}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert("orders", Document{"_id": "o-9"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get("counters", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if c["last"] != "o-9" {
		t.Errorf("counter = %v", c)
	}
}

func TestTriggerOldRefBeforeUpdate(t *testing.T) {
	s := New()
	_ = s.Insert("q", Document{"_id": "1", "state": "open"})
	if _, err := s.CreateTrigger(Trigger{
		Event:      BeforeUpdate,
		Collection: "q",
		Filter:     map[string]any{"state": "closed"},
		Actions: []TriggerAction{
			{Type: "insert", Collection: "log", Doc: map[string]any{
				"prev": map[string]any{"$get": "old.state"},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// filter matches new doc (patch merged view? we match against patch/Doc=patch) —
	// before_update Doc is the PATCH not full doc; filter on patch field:
	// our patch is {"state":"closed"} → filter matches → old.state = "open"
	if err := s.Update("q", "1", Document{"state": "closed"}); err != nil {
		t.Fatal(err)
	}
	logs, err := s.Find("log", nil, nil)
	if err != nil || len(logs) != 1 {
		t.Fatalf("log = %v, %v", logs, err)
	}
	if logs[0]["prev"] != "open" {
		t.Errorf("old ref = %v", logs[0])
	}
}

func TestTriggerDeleteAction(t *testing.T) {
	s := New()
	_ = s.Insert("tmp", Document{"_id": "t1"})
	_ = s.Insert("orders", Document{"_id": "1"})
	if _, err := s.CreateTrigger(Trigger{
		Event:      AfterInsert,
		Collection: "orders",
		Actions: []TriggerAction{
			{Type: "delete", Collection: "tmp", ID: "t1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("orders", Document{"_id": "2"})
	if _, err := s.Get("tmp", "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("tmp t1 should be deleted: %v", err)
	}
}

func TestTriggerValidation(t *testing.T) {
	s := New()
	cases := []Trigger{
		{Event: "nope", Collection: "q", Actions: []TriggerAction{{Type: "set", Fields: map[string]any{"a": 1}}}},
		{Event: BeforeInsert, Collection: "q", Actions: nil},
		{Event: BeforeInsert, Collection: "q", Actions: []TriggerAction{{Type: "wat"}}},
		{Event: AfterInsert, Collection: "q", Actions: []TriggerAction{{Type: "unset", Names: []string{"x"}}}}, // unset not after
		{Event: BeforeInsert, Async: true, Collection: "q", Actions: []TriggerAction{{Type: "set", Fields: map[string]any{"a": 1}}}}, // async before
		{Event: AfterInsert, Collection: "q", Actions: []TriggerAction{{Type: "insert", Collection: "", Doc: map[string]any{"a": 1}}}},
	}
	for i, c := range cases {
		if _, err := s.CreateTrigger(c); err == nil {
			t.Errorf("case %d must fail", i)
		}
	}
}

func TestTriggerDeleteAndList(t *testing.T) {
	s := New()
	id, err := s.CreateTrigger(Trigger{
		Event:      BeforeInsert,
		Collection: "q",
		Actions: []TriggerAction{{Type: "set", Fields: map[string]any{"t": true}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTriggers()
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %v, %v", list, err)
	}
	_ = s.Insert("q", Document{"_id": "1"})
	if err := s.DeleteTrigger(id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTrigger(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete = %v", err)
	}
	list, _ = s.ListTriggers()
	if len(list) != 0 {
		t.Errorf("list after delete = %v", list)
	}
	_ = s.Insert("q", Document{"_id": "2"})
	d2, _ := s.Get("q", "2")
	if _, ok := d2["t"]; ok {
		t.Error("trigger still fires after delete")
	}
	// system coll hidden + protected
	for _, n := range s.Collections() {
		if n == triggersColl {
			t.Error("_triggers leaked")
		}
	}
	if err := s.Insert(triggersColl, Document{"_id": "evil"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("Insert _triggers = %v", err)
	}
}

func TestTriggerDisabledNotRegistered(t *testing.T) {
	s := New()
	off := false
	if _, err := s.CreateTrigger(Trigger{
		Event:      BeforeInsert,
		Collection: "q",
		Enabled:    &off,
		Actions: []TriggerAction{{Type: "set", Fields: map[string]any{"t": true}}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Insert("q", Document{"_id": "1"})
	d, _ := s.Get("q", "1")
	if _, ok := d["t"]; ok {
		t.Error("disabled trigger fired")
	}
}

func TestTriggerPersistsReopen(t *testing.T) {
	dir := t.TempDir()
	path := strings.Join([]string{dir, "db.mlstore"}, "/")
	opts := testOpts()
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTrigger(Trigger{
		Event:      BeforeInsert,
		Collection: "orders",
		Actions: []TriggerAction{{Type: "set", Fields: map[string]any{"stamped": true}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Insert("orders", Document{"_id": "1"}); err != nil {
		t.Fatal(err)
	}
	d, err := s2.Get("orders", "1")
	if err != nil {
		t.Fatal(err)
	}
	if d["stamped"] != true {
		t.Errorf("trigger not reloaded after reopen: %v", d)
	}
	list, _ := s2.ListTriggers()
	if len(list) != 1 {
		t.Errorf("list after reopen = %v", list)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTriggerAsync(t *testing.T) {
	s := New()
	if _, err := s.CreateTrigger(Trigger{
		Event:      AfterInsert,
		Collection: "q",
		Async:      true,
		Actions: []TriggerAction{
			{Type: "insert", Collection: "audit", Doc: map[string]any{
				"ref": map[string]any{"$get": "_id"},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := s.Insert("q", Document{"_id": string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	s.WaitAsyncHooks()
	docs, err := s.Find("audit", nil, nil)
	if err != nil || len(docs) != 10 {
		t.Errorf("async audit = %d docs, %v", len(docs), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
