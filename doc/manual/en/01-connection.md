# 01 — Connection

How to open and close an MLStoreDB database from Go.

## In-memory database

```go
package main

import "mlstoredb/db"

func main() {
    store := db.New()          // RAM, no file
    defer store.Close()

    _ = store.Insert("greeting", db.Document{"_id": "h1", "msg": "hello"})
}
```

## Encrypted file database

```go
store, err := db.OpenWithLock("data.mlstore", db.Options{
    MasterKey: []byte("my-32-bytes-long-master-key!!"), // KEK
    MachineID: "my-machine",                            // optional
})
if err != nil {
    panic(err)
}
defer store.Close()
```

- `Open` opens/creates the file **without** a process lock.
- `OpenWithLock` also takes an exclusive lock (`mlstoredb.lock`): two
  processes cannot open the same DB at once.
- The file is AES-256-GCM encrypted (no plaintext JSON ever hits disk).

## Useful `Options` fields

| Field | What it does |
|---|---|
| `MasterKey []byte` | Master key (KEK). Required for files. |
| `MachineID string` | Machine identity mixed into the key. |
| `LightKDF bool` | Faster Argon2 (first creation only). |
| `SyncOnWrite bool` | Synchronous flush after every mutation (durability). |
| `AutoFlush time.Duration` | Auto-flush ticker period (default 2s). |
| `CacheBytes int64` | Page cache budget (0=256MB, <0=unlimited). |
| `FindWorkers int` | Goroutines for parallel Find (0=GOMAXPROCS). |
| `MaxPendingWrites int` | Write backpressure during flush. |
| `SessionTTL time.Duration` | RBAC session lifetime (0=24h, <0=never). |

## Users, roles and sessions (RBAC)

```go
// 1) create a role with permissions
store.CreateRole("editor", []db.Permission{
    {Collection: "orders", Read: true, Write: true},
    {Collection: "audit",  Read: true},              // read-only
})
// wildcard role
store.CreateRole("admin", []db.Permission{
    {Collection: "*", Read: true, Write: true},
})

// 2) create a user (RBAC activates with the first user)
store.CreateUser("ana", "secret", []string{"editor"})

// 3) authenticate → session
sess, err := store.Authenticate("ana", "secret")
if err != nil { // ErrUnauthorized
    panic(err)
}
docs, _ := sess.Find("orders", db.Document{"status": "PAID"}, nil)
```

- With no users: the raw `Store` API works unrestricted.
- With ≥1 user: the raw API returns `ErrUnauthorized` and you must use
  the `Session` (including `fieldDeny` redaction).

## Connecting from Mongo clients (Navicat / Compass / mongosh)

Start the wire server:

```bash
go run ./tools/mls-server -addr 127.0.0.1:27017 -path data.mlstore \
    -key "my-32-bytes-long-master-key!!" -db mydb
```

Then connect with `mongodb://127.0.0.1:27017`. Full details in
[09-mongo-server.md](09-mongo-server.md).
