# 07 — Triggers and Hooks

Two mechanisms: **Go hooks** (code in your process) and **JSON triggers**
(rules persisted in the DB, declarative style).

Available events:
`before_insert · after_insert · before_update · after_update ·
before_delete · after_delete · before_upsert · after_upsert`

## Go hooks — `On` / `OnAsync` / `Off`

### Veto with before_* (the mutation never happens)

```go
store.On("before_insert", "orders", func(h *db.HookContext) error {
    if total, _ := h.Doc["total"].(float64); total < 0 {
        return errors.New("total cannot be negative") // = veto
    }
    return nil
})
```

### Mutate the document before insert

```go
store.On("before_insert", "orders", func(h *db.HookContext) error {
    h.Doc["created_at"] = time.Now().Format(time.RFC3339)
    return nil
})
```

### Audit after insert (synchronous)

```go
store.On("after_insert", "orders", func(h *db.HookContext) error {
    return h.Insert("audit", db.Document{
        "ref": h.ID,
        "ev":  h.Event,
    })
})
```

### Asynchronous (FIFO queue, does not block the mutation)

```go
store.OnAsync("after_insert", "orders", func(h *db.HookContext) error {
    return h.Update("counters", "orders", db.Document{"last": h.ID})
})
store.WaitAsyncHooks()   // barrier: drains the queue (tests/ops)
```

### Hook payload — `HookContext`

| Field | Contents |
|---|---|
| `h.Event` | `"after_insert"`, … |
| `h.Collection` | affected collection |
| `h.ID` | document `_id` |
| `h.Doc` | new doc / patch (or merged preview on before_update) |
| `h.Old` | previous doc (update/delete); `nil` on insert |

Methods: `h.Get / h.Find / h.Count / h.Insert / h.Upsert / h.Update / h.Delete`
— nested mutations **must** go through `h.*` (they propagate the
depth/cycle frame).

### Unregister

```go
id, _ := store.On("after_insert", "x", fn)
store.Off(id)
```

Protections: max depth 8 (`db.ErrHookDepth`), cycle detection
(`db.ErrHookCycle`), bounded async queue (1024).

## JSON triggers — `CreateTrigger`

Rules **persisted** (they survive reopen) in the `_triggers` system
collection:

```go
store.CreateTrigger(db.Trigger{
    ID:         "audit_paid",
    Event:      "after_insert",
    Collection: "orders",
    Filter:     map[string]any{"status": "PAID"}, // only when it matches
    Actions: []db.TriggerAction{
        {
            Type:       "insert",
            Collection: "audit",
            Doc: map[string]any{
                "ref":  map[string]any{"$get": "_id"},  // template
                "note": "paid order",
            },
        },
    },
})
```

### Available actions

| Type | What it does | Fields |
|---|---|---|
| `set` | merge fields into the doc | `Fields` |
| `unset` | remove fields (`before_*` only) | `Names` |
| `insert` | insert a doc into another collection | `Collection`, `Doc` |
| `update` | patch an existing doc | `Collection`, `ID`, `Set` |
| `upsert` | create or replace | `Collection`, `ID`, `Set` |
| `delete` | delete a doc | `Collection`, `ID` |

### Templates `{"$get": ...}`

```go
Doc: map[string]any{
    "ref":       map[string]any{"$get": "_id"},       // from the event doc
    "prev_total": map[string]any{"$get": "old.total"}, // from the previous doc
}
```

### More examples

```go
// 1) stamp the doc before saving (before mutates the work-copy)
store.CreateTrigger(db.Trigger{
    Event: "before_insert", Collection: "orders",
    Actions: []db.TriggerAction{
        {Type: "set", Fields: map[string]any{"stamped": true}},
    },
})

// 2) counter with upsert
store.CreateTrigger(db.Trigger{
    ID: "cnt_orders", Event: "after_insert", Collection: "orders",
    Actions: []db.TriggerAction{
        {Type: "upsert", Collection: "counters", ID: "orders",
         Set: map[string]any{"last": map[string]any{"$get": "_id"}}},
    },
})

// 3) async (after_* only)
store.CreateTrigger(db.Trigger{
    ID: "async_notify", Event: "after_update", Collection: "orders", Async: true,
    Actions: []db.TriggerAction{ ... },
})
```

### Administration

```go
store.ListTriggers()          // []db.Trigger
store.DeleteTrigger("cnt_orders")
```

`Open` re-registers triggers automatically when the DB is reopened.

> JSON triggers are registered on top of the M5a hook machinery: they
> inherit depth limits, cycle detection and the async queue.
