# 02 — Insertar

Cómo crear documentos.

## Insert básico

```go
err := store.Insert("clientes", db.Document{
    "_id":   "c1",
    "nombre": "Ana",
    "ciudad": "Lima",
})
```

- La colección se crea sola la primera vez que insertas.
- `_id` **debe ser string**. Si lo omites, se genera un ULID automático:

```go
store.Insert("clientes", db.Document{"nombre": "Bo"})  // _id = ULID
```

## Documentos anidados y arrays

```go
store.Insert("orders", db.Document{
    "_id":   "o100",
    "total": 250.0,
    "buyer_id": "c1",
    "items": []any{
        db.Document{"sku": "A1", "qty": 2, "precio": 100.0},
        db.Document{"sku": "B2", "qty": 1, "precio": 50.0},
    },
    "shipping": db.Document{
        "mode": "air",
        "addr": db.Document{"city": "Lima", "zip": "15001"},
    },
})
```

Los valores admiten cualquier JSON: números, strings, bool, null,
arrays (`[]any`) y objetos anidados (`db.Document`).

## Upsert (crear o reemplazar por _id)

```go
// inserta o REEMPLAZA el doc c1 completo
store.Upsert("clientes", "c1", db.Document{
    "_id": "c1", "nombre": "Ana", "nivel": "gold",
})
```

## Índice unique + duplicados

```go
store.EnsureIndex("clientes", []string{"email"}, true) // unique

err := store.Insert("clientes", db.Document{"_id": "c2", "email": "ana@x.com"})
// err == db.ErrDuplicate si el email ya existe
```

## Desde mongosh / Navicat (servidor wire)

```javascript
db.clientes.insertOne({ _id: "c1", nombre: "Ana", ciudad: "Lima" })

db.orders.insertMany([
  { _id: "o1", total: 100, buyer_id: "c1" },
  { _id: "o2", total: 250, buyer_id: "c1" }
])
```

> Nota: por el wire, un `_id` ObjectId o numérico se normaliza a su
> representación string (hex del ObjectId) dentro del motor.

## Límites

- Tamaño máximo por documento: **1 MB** (`db.ErrTooLarge`).
- Colecciones de sistema (`_users`, `_roles`, `_triggers`) no aceptan
  inserts directos: `db.ErrForbidden`.
