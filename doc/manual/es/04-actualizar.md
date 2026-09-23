# 04 — Actualizar (UPDATE)

`Update` aplica un **merge shallow** sobre el documento vivo; nunca
reescribe lo que no tocas.

## Update con patch (merge)

```go
err := store.Update("clientes", "c1", db.Document{
    "nivel": "gold",        // se añade/actualiza
})
```

- Solo cambian las claves del patch; el resto del doc queda intacto.
- **Copy-on-write**: si la validación falla (tamaño, índice unique), el
  documento vivo no queda a medias.

## UpdateFields: patch + borrar campos

```go
err := store.UpdateFields("clientes", "c1",
    db.Document{"nivel": "gold"}, // set
    []string{"temp", "cache"},    // unset (borrar)
)
```

## Actualizar campos anidados

El merge es *shallow* (nivel superior). Para tocar un anidado sin perder
el resto del sub-objeto, lee-modifica-escribe:

```go
doc, _ := store.Get("orders", "o100")
ship := doc["shipping"].(db.Document)
ship["mode"] = "sea"
store.Update("orders", "o100", db.Document{"shipping": ship})
```

## Upsert

```go
// inserta o REEMPLAZA completo el doc con ese _id
store.Upsert("clientes", "c1", db.Document{"_id": "c1", "nombre": "Ana"})
```

## Desde mongosh / Navicat (servidor wire)

El wire traduce los operadores Mongo a las operaciones del motor:

```javascript
db.clientes.updateOne(
  { _id: "c1" },
  { $set: { nivel: "gold", "perfil.vip": true } }   // paths punteados OK
)

db.clientes.updateOne({ _id: "c1" }, { $unset: { temp: "" } })

db.orders.updateMany(
  { status: "PAID" },
  { $inc: { intentos: 1 } }
)

db.orders.updateOne(
  { _id: "o100" },
  { $push: { items: { sku: "C3", qty: 1 } } }
)

db.orders.updateOne({ _id: "o100" }, { $pull: { items: { sku: "B2" } } })

// reemplazo completo del documento (conserva _id)
db.clientes.replaceOne({ _id: "c1" }, { nombre: "Ana", nivel: "gold" })

// upsert: inserta si no hay match
db.counters.updateOne(
  { coll: "orders" },
  { $inc: { n: 1 } },
  { upsert: true }
)

// devuelve el documento modificado
db.orders.findAndModify({
  query: { _id: "o100" },
  update: { $set: { locked: true } },
  new: true
})
```

Operadores soportados por el wire:
`$set $unset $inc $mul $min $max $rename $push $addToSet $pop $pull
$pullAll $currentDate $setOnInsert` + reemplazo + pipeline NO soportado.
