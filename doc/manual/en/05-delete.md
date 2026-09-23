# 05 — Delete

## By _id

```go
err := store.Delete("customers", "c1")
// err == db.ErrNotFound when missing
```

## By filter (find + delete)

The engine deletes by `_id`; to delete by condition, find first and then
delete each id:

```go
docs, _ := store.Find("orders", db.Document{"status": "CANCELLED"}, nil)
for _, d := range docs {
    _ = store.Delete("orders", d["_id"].(string))
}
```

## Delete a field (not the document)

```go
store.UpdateFields("orders", "o100", nil, []string{"temp"})
```

## Drop an entire collection

```go
store.DropCollection("orders")    // removes docs + indexes; error if missing
```

`DropCollection` routes every document through the normal delete path
(DEL records, hooks, cache) and the file scan is protected so the
collection cannot resurrect on reopen.

## From mongosh / Navicat

```javascript
db.customers.deleteOne({ _id: "c1" })

db.orders.deleteMany({ status: "CANCELLED" })   // limit 0 = all matches

db.orders.deleteMany({ status: "PAID", _id: "o100" }) // limit implied by _id

db.orders.drop()   // DropCollection
```

> `before_delete` / `after_delete` hooks (and triggers on them) fire on
> every delete, exactly like from Go.
