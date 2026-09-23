# 06 — Joins: combinar colecciones y armar JSON anidado

MLStoreDB no tiene `JOIN` de SQL, pero hay **cuatro patrones** para
relacionar colecciones y producir JSON con datos anidados de otras
colecciones.

Datos de ejemplo:

```go
store.Insert("customers", db.Document{"_id": "c1", "nombre": "Ana", "city": "Lima"})
store.Insert("customers", db.Document{"_id": "c2", "nombre": "Bo",  "city": "Bogotá"})
store.Insert("orders", db.Document{"_id": "o1", "total": 100, "buyer_id": "c1"})
store.Insert("orders", db.Document{"_id": "o2", "total": 250, "buyer_id": "c1"})
store.Insert("orders", db.Document{"_id": "o3", "total":  80, "buyer_id": "c2"})
```

## Patrón A — Embeber un documento (1 a 1)

```go
order, _ := store.Get("orders", "o1")
cust, _  := store.Get("customers", order["buyer_id"].(string))
order["customer"] = cust   // ← JSON anidado

// order = { _id: "o1", total: 100, buyer_id: "c1",
//           customer: { _id: "c1", nombre: "Ana", city: "Lima" } }
```

## Patrón B — Batch con `$in` (N documentos, 2 queries)

El patrón clásico para listados: 1 find de orders + 1 find de customers:

```go
orders, _ := store.Find("orders", nil, nil)

// 1) recolectar ids únicos de buyers
idSet := map[string]bool{}
var ids []any
for _, o := range orders {
    id := o["buyer_id"].(string)
    if !idSet[id] {
        idSet[id] = true
        ids = append(ids, id)
    }
}
// 2) traer todos los customers de golpe (usa índice si existe)
customers, _ := store.Find("customers",
    db.Document{"_id": db.Document{"$in": ids}}, nil)
byID := map[string]db.Document{}
for _, c := range customers {
    byID[c["_id"].(string)] = c
}
// 3) anidar
for _, o := range orders {
    o["customer"] = byID[o["buyer_id"].(string)]
}
```

Con índice en `_id`/`buyer_id` esto es O(orders + customers), sin N+1.

## Patrón C — `$lookup` con aggregate (wire / mongosh / Compass)

El servidor Mongo (M8) implementa `$lookup` + `$unwind`:

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

Resultado (un doc por order, con el customer anidado):

```json
[
  { "_id": "o2", "total": 250, "buyer_id": "c1",
    "customer": { "_id": "c1", "nombre": "Ana", "city": "Lima" } },
  { "_id": "o1", "total": 100, "buyer_id": "c1",
    "customer": { "_id": "c1", "nombre": "Ana", "city": "Lima" } }
]
```

## Patrón D — Relación como grafo (edges)

Cuando la relación es un dato de primera clase (con propiedades propias),
guárdala como arista:

```go
store.AddEdge("compro", "c1", "o1", db.Document{"canal": "web"})
store.AddEdge("compro", "c1", "o2", db.Document{"canal": "app"})

// todas las orders de Ana:
orderIDs, _ := store.Neighbors("compro", "c1", db.Outgoing)
```

Las aristas viven en la colección `edges.compro` como documentos
`{_id, _from, _to, ...props}` — puedes consultarlas con `Find` normal:

```go
store.Find("edges.compro", db.Document{"canal": "web"}, nil)
```

Ver [08-grafos.md](08-grafos.md) para recorridos y rutas.

## ¿Qué patrón usar?

| Necesitas | Patrón |
|---|---|
| Un doc + su padre (detalle) | **A** (Get + Get) |
| Listado con padre embebido | **B** (`$in` batch) |
| Consulta desde el cliente GUI | **C** (`$lookup` aggregate) |
| Relación con propiedades / recorridos | **D** (edges/grafo) |
