# 05 — Eliminar (DELETE)

## Por _id

```go
err := store.Delete("clientes", "c1")
// err == db.ErrNotFound si no existe
```

## Por filtro (find + delete)

El motor borra por `_id`; para borrar por condición, primero busca y
luego borra cada id:

```go
docs, _ := store.Find("orders", db.Document{"status": "CANCELLED"}, nil)
for _, d := range docs {
    _ = store.Delete("orders", d["_id"].(string))
}
```

## Eliminar campo (no el documento)

```go
store.UpdateFields("orders", "o100", nil, []string{"temp"})
```

## Eliminar colección completa

```go
store.DropCollection("orders")    // borra docs + índices; err si no existe
```

`DropCollection` pasa cada documento por el path de delete normal
(registros DEL, hooks, caché) y el scan del archivo está protegido para
que la colección no resucite al reabrir.

## Desde mongosh / Navicat

```javascript
db.clientes.deleteOne({ _id: "c1" })

db.orders.deleteMany({ status: "CANCELLED" })   // limit 0 = todos los matches

db.orders.deleteMany({ status: "PAID" , _id: "o100" }) // limit implícito por _id

db.orders.drop()   // DropCollection
```

> Los hooks `before_delete` / `after_delete` (y triggers sobre ellos)
> se disparan en cada borrado, igual que desde Go.
