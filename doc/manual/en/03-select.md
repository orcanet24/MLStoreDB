# 03 — Select

`Find`, `Get` and `Count` with MongoDB-style filters.

## By _id

```go
doc, err := store.Get("customers", "c1")
// doc = db.Document{"_id": "c1", "nombre": "Ana", ...}
// err == db.ErrNotFound when missing
```

## Find with a filter

```go
docs, err := store.Find("orders",
    db.Document{"status": "PAID"},   // equality
    nil,                             // no options
)
```

## Comparison operators

```go
db.Document{"total": db.Document{"$gte": 100}}   // >= 100
db.Document{"total": db.Document{"$gt": 100}}    // >  100
db.Document{"total": db.Document{"$lt": 500}}    // <  500
db.Document{"total": db.Document{"$lte": 500}}   // <= 500
db.Document{"total": db.Document{"$ne": 0}}      // != 0 (missing → true)
db.Document{"st":    db.Document{"$in": []any{"A", "C"}}}
db.Document{"st":    db.Document{"$nin": []any{"X"}}}
```

## Regex and existence

```go
db.Document{"tag":   db.Document{"$regex": "^a", "$options": "i"}} // case-insensitive
db.Document{"field": db.Document{"$exists": true}}                 // present
db.Document{"field": nil}                                          // null or missing
```

## Logical operators

```go
db.Document{"$and": []any{
    db.Document{"status": "PAID"},
    db.Document{"total": db.Document{"$gte": 100}},
}}
db.Document{"$or":  []any{ ... }}
db.Document{"$nor": []any{ ... }}
db.Document{"$not": db.Document{"status": "PAID"}}   // negates the sub-filter
```

## Nested fields (dotted paths)

```go
db.Document{"shipping.mode": "air"}                          // nested object
db.Document{"payload.shipping.mode": db.Document{"$ne": "sea"}}
```

## Sort, Limit, Skip and Projection

```go
docs, err := store.Find("orders",
    db.Document{"total": db.Document{"$gte": 100}},
    &db.FindOptions{
        Sort:       map[string]int{"total": -1},   // 1 asc, -1 desc
        Limit:      10,
        Skip:       20,                            // pagination
        Projection: []string{"total", "buyer_id"}, // only these (_id always)
    },
)
```

> Multi-key: sort applies columns in alphabetical field order (documented
> limitation). Single-column sort is exact.

## Count

```go
n, err := store.Count("orders", db.Document{"status": "PAID"})
n, _ = store.Count("orders", nil)  // all
```

## Indexes

Equality/`$in`/range filters automatically use an index when one covers
the field:

```go
store.EnsureIndex("orders", []string{"status"}, false)          // simple
store.EnsureIndex("orders", []string{"buyer_id", "status"}, false) // compound
store.EnsureIndex("orders", []string{"order_id"}, true)         // unique
```

## From mongosh / Navicat

```javascript
db.orders.find({ total: { $gte: 100 } })
          .sort({ total: -1 })
          .limit(10)
          .skip(20)

db.orders.find({ status: "PAID" }, { total: 1, buyer_id: 1 })

db.orders.countDocuments({ status: "PAID" })

db.orders.findOne({ _id: "o100" })
```

Operators supported by the engine:
`$eq $ne $gt $gte $lt $lte $in $nin $regex $options $exists $and $or $not $nor`.
Not supported (returns an error): `$elemMatch $size $type $mod $all $where $expr`.
