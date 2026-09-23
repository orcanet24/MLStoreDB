# 04 — Update

`Update` applies a **shallow merge** over the live document; it never
rewrites what you don't touch.

## Update with a patch (merge)

```go
err := store.Update("customers", "c1", db.Document{
    "level": "gold",       // added/updated
})
```

- Only the keys in the patch change; the rest of the doc stays intact.
- **Copy-on-write**: if validation fails (size, unique index), the live
  document is never left half-patched.

## UpdateFields: patch + remove fields

```go
err := store.UpdateFields("customers", "c1",
    db.Document{"level": "gold"}, // set
    []string{"temp", "cache"},    // unset (remove)
)
```

## Updating nested fields

The merge is *shallow* (top level). To touch a nested object without
losing its siblings, read-modify-write:

```go
doc, _ := store.Get("orders", "o100")
ship := doc["shipping"].(db.Document)
ship["mode"] = "sea"
store.Update("orders", "o100", db.Document{"shipping": ship})
```

## Upsert

```go
// inserts or fully REPLACES the doc with that _id
store.Upsert("customers", "c1", db.Document{"_id": "c1", "nombre": "Ana"})
```

## From mongosh / Navicat (wire server)

The wire translates Mongo operators into engine operations:

```javascript
db.customers.updateOne(
  { _id: "c1" },
  { $set: { level: "gold", "profile.vip": true } }   // dotted paths OK
)

db.customers.updateOne({ _id: "c1" }, { $unset: { temp: "" } })

db.orders.updateMany(
  { status: "PAID" },
  { $inc: { attempts: 1 } }
)

db.orders.updateOne(
  { _id: "o100" },
  { $push: { items: { sku: "C3", qty: 1 } } }
)

db.orders.updateOne({ _id: "o100" }, { $pull: { items: { sku: "B2" } } })

// full document replacement (keeps _id)
db.customers.replaceOne({ _id: "c1" }, { nombre: "Ana", level: "gold" })

// upsert: insert when no match
db.counters.updateOne(
  { coll: "orders" },
  { $inc: { n: 1 } },
  { upsert: true }
)

// returns the modified document
db.orders.findAndModify({
  query: { _id: "o100" },
  update: { $set: { locked: true } },
  new: true
})
```

Operators supported by the wire:
`$set $unset $inc $mul $min $max $rename $push $addToSet $pop $pull
$pullAll $currentDate $setOnInsert` + replacement; pipelines not supported.
