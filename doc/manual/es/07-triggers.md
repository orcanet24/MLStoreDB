# 07 — Triggers y Hooks

Dos mecanismos: **hooks Go** (código en tu proceso) y **triggers JSON**
(reglas persistidas en la BD, estilo declarativo).

Eventos disponibles:
`before_insert · after_insert · before_update · after_update ·
before_delete · after_delete · before_upsert · after_upsert`

## Hooks Go — `On` / `OnAsync` / `Off`

### Veto con before_* (la mutación no ocurre)

```go
store.On("before_insert", "orders", func(h *db.HookContext) error {
    if total, _ := h.Doc["total"].(float64); total < 0 {
        return errors.New("total no puede ser negativo") // = veto
    }
    return nil
})
```

### Mutar el documento antes de insertar

```go
store.On("before_insert", "orders", func(h *db.HookContext) error {
    h.Doc["creado_en"] = time.Now().Format(time.RFC3339)
    return nil
})
```

### Auditoría después de insertar (síncrono)

```go
store.On("after_insert", "orders", func(h *db.HookContext) error {
    return h.Insert("audit", db.Document{
        "ref":  h.ID,
        "ev":   h.Event,
    })
})
```

### Asíncrono (cola FIFO, no bloquea la mutación)

```go
store.OnAsync("after_insert", "orders", func(h *db.HookContext) error {
    return h.Update("counters", "orders", db.Document{"last": h.ID})
})
store.WaitAsyncHooks()   // barrera: espera la cola (tests/ops)
```

### Payload del hook — `HookContext`

| Campo | Contenido |
|---|---|
| `h.Event` | `"after_insert"`, … |
| `h.Collection` | colección afectada |
| `h.ID` | `_id` del documento |
| `h.Doc` | doc nuevo / patch (o preview merged en before_update) |
| `h.Old` | doc previo (update/delete); `nil` en insert |

Métodos: `h.Get / h.Find / h.Count / h.Insert / h.Upsert / h.Update / h.Delete`
— las mutaciones anidadas **deben** ir por `h.*` (propagan el frame de
profundidad/ciclos).

### Desregistrar

```go
id, _ := store.On("after_insert", "x", fn)
store.Off(id)
```

Protecciones: profundidad máx 8 (`db.ErrHookDepth`), detección de ciclos
(`db.ErrHookCycle`), cola async acotada (1024).

## Triggers JSON — `CreateTrigger`

Reglas **persistidas** (sobreviven reopen) en la colección de sistema
`_triggers`:

```go
store.CreateTrigger(db.Trigger{
    ID:         "audit_paid",
    Event:      "after_insert",
    Collection: "orders",
    Filter:     map[string]any{"status": "PAID"},  // solo si matchea
    Actions: []db.TriggerAction{
        {
            Type:       "insert",
            Collection: "audit",
            Doc: map[string]any{
                "ref":  map[string]any{"$get": "_id"},  // template
                "note": "orden pagada",
            },
        },
    },
})
```

### Acciones disponibles

| Type | Qué hace | Campos |
|---|---|---|
| `set` | merge de campos en el doc | `Fields` |
| `unset` | borra campos (solo `before_*`) | `Names` |
| `insert` | inserta doc en otra colección | `Collection`, `Doc` |
| `update` | patchea un doc existente | `Collection`, `ID`, `Set` |
| `upsert` | crea o reemplaza | `Collection`, `ID`, `Set` |
| `delete` | borra un doc | `Collection`, `ID` |

### Templates `{"$get": ...}`

```go
Doc: map[string]any{
    "ref":      map[string]any{"$get": "_id"},        // del doc del evento
    "precio_v": map[string]any{"$get": "old.total"},  // del doc previo
}
```

### Otros ejemplos

```go
// 1) marcar el doc antes de guardar (before muta el work-copy)
store.CreateTrigger(db.Trigger{
    Event: "before_insert", Collection: "orders",
    Actions: []db.TriggerAction{
        {Type: "set", Fields: map[string]any{"stamped": true}},
    },
})

// 2) contador con upsert
store.CreateTrigger(db.Trigger{
    ID: "cnt_orders", Event: "after_insert", Collection: "orders",
    Actions: []db.TriggerAction{
        {Type: "upsert", Collection: "counters", ID: "orders",
         Set: map[string]any{"last": map[string]any{"$get": "_id"}}},
    },
})

// 3) async (solo after_*)
store.CreateTrigger(db.Trigger{
    ID: "async_notify", Event: "after_update", Collection: "orders", Async: true,
    Actions: []db.TriggerAction{ ... },
})
```

### Administración

```go
store.ListTriggers()          // []db.Trigger
store.DeleteTrigger("cnt_orders")
```

`Open` re-registra los triggers automáticamente al reabrir la BD.

> Los triggers JSON se registran sobre la maquinaria de hooks M5a:
> heredan depth-limit, detección de ciclos y la cola async.
