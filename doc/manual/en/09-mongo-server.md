# 09 — Mongo-compatible server (M8)

`mls-server` exposes the DB over TCP speaking the **MongoDB wire
protocol** (an `OP_MSG` + legacy `OP_QUERY` subset), so you can use
Navicat, Compass or mongosh without writing code.

## Starting it

```bash
# in-memory (quick testing)
go run ./tools/mls-server -addr 127.0.0.1:28917 -db demo

# encrypted file
go run ./tools/mls-server -addr 127.0.0.1:28917 \
    -path data.mlstore \
    -key "my-32-bytes-long-master-key!!" \
    -db mydb

# with SCRAM-SHA-256 authentication (optional)
go run ./tools/mls-server -addr 127.0.0.1:28917 -path data.mlstore \
    -key "my-key" -db mydb -user admin -pass secret
```

| Flag | Default | Description |
|---|---|---|
| `-addr` | `127.0.0.1:28917` | Listen address (MLStoreDB's own port; no clash with a real MongoDB on 27017) |
| `-path` | *(memory)* | `.mlstore` file |
| `-db` | file name | Database name clients see; without `-path`, if `<dbdir>/<name>/<name>.mlstore` exists it serves that file |
| `-web` | off | Web admin console (e.g. `127.0.0.1:28918`) |
| `-dbdir` | `databases` | Directory of databases discovered/created by the web console |
| `-key` | — | Master key (required for encrypted files) |
| `-machine` | default | KEK machine ID |
| `-user` / `-pass` | — | Enables SCRAM-SHA-256 on the wire (must be an existing database user when the DB has RBAC active) |
| `-light-kdf` | off | Fast Argon2 (first creation only) |
| `-reset-auth` | off | Wipes users/roles/sessions without touching data, then exits |

## Connecting

| Client | Connection string / settings |
|---|---|
| mongosh | `mongosh mongodb://127.0.0.1:28917` |
| Compass | `mongodb://127.0.0.1:28917` |
| Navicat | Host `127.0.0.1`, port `28917`, no authentication |
| With auth | `mongodb://admin:secret@127.0.0.1:28917` |

> Data model: the server exposes **one database** (the `-db` flag)
> containing every collection in the store. Commands are accepted on
> any `$db`, but they all point to the same store.

## Supported commands

| Group | Commands |
|---|---|
| Handshake | `hello`, `isMaster`, `ping`, `buildInfo`, `getLog`, `connectionStatus`, `whatsmyuri`, `serverStatus` |
| Sessions | `startSession`, `endSessions`, `logout` |
| CRUD | `insert`, `find`, `getMore`, `killCursors`, `count`, `countDocuments`, `estimatedDocumentCount`, `distinct`, `update`, `delete`, `findAndModify` |
| Aggregate | `$match $sort $skip $limit $count $group $unwind $project $lookup` |
| Admin | `listDatabases`, `listCollections`, `create`, `drop`, `createIndexes`, `listIndexes`, `dropIndexes`, `dbStats`, `collStats` |
| Auth | `saslStart` / `saslContinue` (SCRAM-SHA-256) |

## Authentication

- Without `-user`: no wire auth. Clients connecting without credentials work
  (unless the **engine** has RBAC active, ≥1 user: then every operation returns
  `Unauthorized`). A client that *tries* to authenticate gets `Authentication
  failed` — shown by Compass/Navicat/mongosh as *invalid credentials* — and the
  server logs it, stating that `-user`/`-pass` is missing.
- With `-user`: the wire requires SCRAM-SHA-256. After authenticating, if the
  engine has RBAC active, an engine `Session` is created for that user
  (permissions and `fieldDeny` apply just like from Go): `-user`/`-pass` must be
  an **existing database user** (`_users`, web console → Users), not a separate
  credential.
- Startup diagnostics: the server prints which store the wire serves, which
  `-dbdir` databases are web-console-only (and how to serve them: `-db <name>`),
  and whether `-user` exists in the database (listing valid users).

## Subset specifics

- `_id`: ObjectIds and numbers are normalized to strings (ObjectId hex).
- Numbers: BSON int32/int64/double — the engine is JSON-numeric
  (5 and 5.0 compare equal when filtering).
- Multi-key sort: applied in alphabetical field order.
- Dotted projections and `$elemMatch/$size/$type/$mod/$all/$expr` are
  not supported (`BadValue` / `CommandNotFound` errors).
- Compression (`OP_COMPRESSED`) not advertised; OP_MSG checksums verified.
- Cursors have a 10-minute idle TTL; default batch 101 docs.
- `maxBsonObjectSize` = 1 MB (the engine's real limit).

## Full session example (mongosh)

```javascript
// connect: mongosh mongodb://127.0.0.1:28917
use mydb

db.createCollection("customers")
db.customers.insertOne({ _id: "c1", nombre: "Ana", email: "ana@x.com" })
db.customers.createIndex({ email: 1 }, { unique: true })

db.customers.find({ nombre: /^A/ })
db.customers.aggregate([
  { $group: { _id: "$city", n: { $sum: 1 } } }
])

db.customers.updateOne({ _id: "c1" }, { $set: { level: "gold" } })
db.customers.deleteOne({ _id: "c1" })
db.customers.drop()
```
