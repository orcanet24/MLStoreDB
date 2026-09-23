# 03 — Consultar (SELECT)

`Find`, `Get` y `Count` con filtros estilo MongoDB.

## Por _id

```go
doc, err := store.Get("clientes", "c1")
// doc = db.Document{"_id": "c1", "nombre": "Ana", ...}
// err == db.ErrNotFound si no existe
```

## Find con filtro

```go
docs, err := store.Find("orders",
    db.Document{"status": "PAID"},   // igualdad
    nil,                              // sin opciones
)
```

## Operadores de comparación

```go
db.Document{"total": db.Document{"$gte": 100}}   // >= 100
db.Document{"total": db.Document{"$gt": 100}}    // >  100
db.Document{"total": db.Document{"$lt": 500}}    // <  500
db.Document{"total": db.Document{"$lte": 500}}   // <= 500
db.Document{"total": db.Document{"$ne": 0}}      // != 0 (missing → true)
db.Document{"st":    db.Document{"$in": []any{"A", "C"}}}
db.Document{"st":    db.Document{"$nin": []any{"X"}}}
```

## Regex y existencia

```go
db.Document{"tag": db.Document{"$regex": "^a", "$options": "i"}}  // i=case-insensitive
db.Document{"campo": db.Document{"$exists": true}}                 // presente
db.Document{"campo": nil}                                          // null o ausente
```

## Operadores lógicos

```go
db.Document{"$and": []any{
    db.Document{"status": "PAID"},
    db.Document{"total": db.Document{"$gte": 100}},
}}
db.Document{"$or":  []any{ ... }}
db.Document{"$nor": []any{ ... }}
db.Document{"$not": db.Document{"status": "PAID"}}   // niega el sub-filtro
```

## Campos anidados (paths punteados)

```go
db.Document{"shipping.mode": "air"}                       // objeto anidado
db.Document{"payload.shipping.mode": db.Document{"$ne": "sea"}}
```

## Sort, Limit, Skip y Projection

```go
docs, err := store.Find("orders",
    db.Document{"total": db.Document{"$gte": 100}},
    &db.FindOptions{
        Sort:       map[string]int{"total": -1}, // 1 asc, -1 desc
        Limit:      10,
        Skip:       20,                          // paginación
        Projection: []string{"total", "buyer_id"}, // solo estos campos (_id siempre)
    },
)
```

> Multi-clave: el sort aplica las columnas en orden alfabético de campos
> (limitación documentada). Para un sort de una columna es exacto.

## Count

```go
n, err := store.Count("orders", db.Document{"status": "PAID"})
n, _ = store.Count("orders", nil)  // todos
```

## Índices

Los filtros de igualdad/`$in`/rango usan el índice automáticamente si
existe uno que cubra el campo:

```go
store.EnsureIndex("orders", []string{"status"}, false)     // simple
store.EnsureIndex("orders", []string{"buyer_id", "status"}, false) // compuesto
store.EnsureIndex("orders", []string{"order_id"}, true)    // unique
```

## Desde mongosh / Navicat

```javascript
db.orders.find({ total: { $gte: 100 } })
          .sort({ total: -1 })
          .limit(10)
          .skip(20)

db.orders.find({ status: "PAID" }, { total: 1, buyer_id: 1 })

db.orders.countDocuments({ status: "PAID" })

db.orders.findOne({ _id: "o100" })
```

Operadores soportados por el motor:
`$eq $ne $gt $gte $lt $lte $in $nin $regex $options $exists $and $or $not $nor`.
No soportados (devuelven error): `$elemMatch $size $type $mod $all $where $expr`.
