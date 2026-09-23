# 02 — Insert

How to create documents.

## Basic insert

```go
err := store.Insert("customers", db.Document{
    "_id":    "c1",
    "nombre": "Ana",
    "city":   "Lima",
})
```

- The collection is created automatically on first insert.
- `_id` **must be a string**. If omitted, an automatic ULID is generated:

```go
store.Insert("customers", db.Document{"nombre": "Bo"})  // _id = ULID
```

## Nested documents and arrays

```go
store.Insert("orders", db.Document{
    "_id":      "o100",
    "total":    250.0,
    "buyer_id": "c1",
    "items": []any{
        db.Document{"sku": "A1", "qty": 2, "price": 100.0},
        db.Document{"sku": "B2", "qty": 1, "price": 50.0},
    },
    "shipping": db.Document{
        "mode": "air",
        "addr": db.Document{"city": "Lima", "zip": "15001"},
    },
})
```

Values can be any JSON: numbers, strings, bools, null, arrays
(`[]any`) and nested objects (`db.Document`).

## Upsert (create or replace by _id)

```go
// inserts or fully REPLACES doc c1
store.Upsert("customers", "c1", db.Document{
    "_id": "c1", "nombre": "Ana", "level": "gold",
})
```

## Unique index + duplicates

```go
store.EnsureIndex("customers", []string{"email"}, true) // unique

err := store.Insert("customers", db.Document{"_id": "c2", "email": "ana@x.com"})
// err == db.ErrDuplicate if the email already exists
```

## From mongosh / Navicat (wire server)

```javascript
db.customers.insertOne({ _id: "c1", nombre: "Ana", city: "Lima" })

db.orders.insertMany([
  { _id: "o1", total: 100, buyer_id: "c1" },
  { _id: "o2", total: 250, buyer_id: "c1" }
])
```

> Note: through the wire, an ObjectId or numeric `_id` is normalized to
> its string representation (ObjectId hex) inside the engine.

## Limits

- Max document size: **1 MB** (`db.ErrTooLarge`).
- System collections (`_users`, `_roles`, `_triggers`) reject direct
  inserts: `db.ErrForbidden`.
