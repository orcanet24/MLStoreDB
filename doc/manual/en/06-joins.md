# 06 — Joins: combining collections into nested JSON

MLStoreDB has no SQL `JOIN`, but there are **four patterns** to relate
collections and produce JSON with nested data from other collections.

Sample data:

```go
store.Insert("customers", db.Document{"_id": "c1", "nombre": "Ana", "city": "Lima"})
store.Insert("customers", db.Document{"_id": "c2", "nombre": "Bo",  "city": "Bogotá"})
store.Insert("orders", db.Document{"_id": "o1", "total": 100, "buyer_id": "c1"})
store.Insert("orders", db.Document{"_id": "o2", "total": 250, "buyer_id": "c1"})
store.Insert("orders", db.Document{"_id": "o3", "total":  80, "buyer_id": "c2"})
```

## Pattern A — Embed one document (1-to-1)

```go
order, _ := store.Get("orders", "o1")
cust, _  := store.Get("customers", order["buyer_id"].(string))
order["customer"] = cust   // ← nested JSON

// order = { _id: "o1", total: 100, buyer_id: "c1",
//           customer: { _id: "c1", "nombre": "Ana", city: "Lima" } }
```

## Pattern B — Batch with `$in` (N documents, 2 queries)

The classic pattern for listings: 1 find on orders + 1 find on customers:

```go
orders, _ := store.Find("orders", nil, nil)

// 1) collect unique buyer ids
idSet := map[string]bool{}
var ids []any
for _, o := range orders {
    id := o["buyer_id"].(string)
    if !idSet[id] {
        idSet[id] = true
        ids = append(ids, id)
    }
}
// 2) fetch all customers at once (uses the index if present)
customers, _ := store.Find("customers",
    db.Document{"_id": db.Document{"$in": ids}}, nil)
byID := map[string]db.Document{}
for _, c := range customers {
    byID[c["_id"].(string)] = c
}
// 3) nest
for _, o := range orders {
    o["customer"] = byID[o["buyer_id"].(string)]
}
```

With an index on `_id`/`buyer_id` this is O(orders + customers), no N+1.

## Pattern C — `$lookup` with aggregate (wire / mongosh / Compass)

The Mongo server (M8) implements `$lookup` + `$unwind`:

```javascript
db.orders.aggregate([
  { $match: { total: { $gte: 100 } } },
  { $lookup: {
      from:         "customers",
      localField:   "buyer_id",
      foreignField: "_id",
      as:           "customer"
  }},
  { $unwind: "$customer" },
  { $sort: { total: -1 } }
])
```

Result (one doc per order, with the customer nested):

```json
[
  { "_id": "o2", "total": 250, "buyer_id": "c1",
    "customer": { "_id": "c1", "nombre": "Ana", "city": "Lima" } },
  { "_id": "o1", "total": 100, "buyer_id": "c1",
    "customer": { "_id": "c1", "nombre": "Ana", "city": "Lima" } }
]
```

## Pattern D — Relationship as a graph (edges)

When the relationship is a first-class datum (with its own properties),
store it as an edge:

```go
store.AddEdge("bought", "c1", "o1", db.Document{"channel": "web"})
store.AddEdge("bought", "c1", "o2", db.Document{"channel": "app"})

// all of Ana's orders:
orderIDs, _ := store.Neighbors("bought", "c1", db.Outgoing)
```

Edges live in the `edges.bought` collection as documents
`{_id, _from, _to, ...props}` — you can query them with plain `Find`:

```go
store.Find("edges.bought", db.Document{"channel": "web"}, nil)
```

See [08-graphs.md](08-graphs.md) for traversals and paths.

## Which pattern?

| You need | Pattern |
|---|---|
| One doc + its parent (detail view) | **A** (Get + Get) |
| Listing with parent embedded | **B** (`$in` batch) |
| Query from a GUI client | **C** (`$lookup` aggregate) |
| Relationship with properties / traversals | **D** (edges/graph) |
