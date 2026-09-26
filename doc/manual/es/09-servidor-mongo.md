# 09 — Servidor Mongo-compatible (M8)

`mls-server` expone la BD por TCP hablando el **wire protocol de
MongoDB** (subset `OP_MSG` + legado `OP_QUERY`), para usar Navicat,
Compass o mongosh sin escribir código.

## Arrancar

```bash
# in-memory (pruebas rápidas)
go run ./tools/mls-server -addr 127.0.0.1:28917 -db demo

# archivo cifrado
go run ./tools/mls-server -addr 127.0.0.1:28917 \
    -path datos.mlstore \
    -key "mi-clave-maestra-de-32-bytes!!" \
    -db midb

# con autenticación SCRAM-SHA-256 (opcional)
go run ./tools/mls-server -addr 127.0.0.1:28917 -path datos.mlstore \
    -key "mi-clave" -db midb -user admin -pass secreto
```

| Flag | Default | Descripción |
|---|---|---|
| `-addr` | `127.0.0.1:28917` | Dirección de escucha (puerto propio de MLStoreDB, no colisiona con un MongoDB real en 27017) |
| `-path` | *(memoria)* | Archivo `.mlstore` |
| `-db` | nombre del archivo | Nombre de la BD que ven los clientes; sin `-path`, si existe `<dbdir>/<nombre>/<nombre>.mlstore` sirve ese archivo |
| `-web` | off | Consola web de administración (p. ej. `127.0.0.1:28918`) |
| `-dbdir` | `databases` | Directorio de bases descubiertas/creadas por la consola web |
| `-key` | — | Master key (requerida para archivos cifrados) |
| `-machine` | default | Machine ID del KEK |
| `-user` / `-pass` | — | Habilita SCRAM-SHA-256 en el wire (debe ser un usuario existente de la base si ésta tiene RBAC activo) |
| `-light-kdf` | off | Argon2 rápido (solo primera creación) |
| `-reset-auth` | off | Borra usuarios/roles/sesiones sin tocar datos y sale |

## Conectar

| Cliente | Cadena / configuración |
|---|---|
| mongosh | `mongosh mongodb://127.0.0.1:28917` |
| Compass | `mongodb://127.0.0.1:28917` |
| Navicat | Host `127.0.0.1`, port `28917`, sin autenticación |
| Con auth | `mongodb://admin:secreto@127.0.0.1:28917` |

> Modelo de datos: el servidor expone **una base de datos** (la del
> flag `-db`) con todas las colecciones del store. Los comandos se
> aceptan sobre cualquier `$db`, pero todos apuntan al mismo store.

## Comandos soportados

| Grupo | Comandos |
|---|---|
| Handshake | `hello`, `isMaster`, `ping`, `buildInfo`, `getLog`, `connectionStatus`, `whatsmyuri`, `serverStatus` |
| Sesiones | `startSession`, `endSessions`, `logout` |
| CRUD | `insert`, `find`, `getMore`, `killCursors`, `count`, `countDocuments`, `estimatedDocumentCount`, `distinct`, `update`, `delete`, `findAndModify` |
| Aggregate | `$match $sort $skip $limit $count $group $unwind $project $lookup` |
| Admin | `listDatabases`, `listCollections`, `create`, `drop`, `createIndexes`, `listIndexes`, `dropIndexes`, `dbStats`, `collStats` |
| Auth | `saslStart` / `saslContinue` (SCRAM-SHA-256) |

## Autenticación

- Sin `-user`: no hay auth de wire. Los clientes que conectan sin credenciales
  funcionan (salvo que el **motor** tenga RBAC activo, ≥1 usuario: entonces toda
  operación devuelve `Unauthorized`). Un cliente que **intente** autenticarse recibe
  `Authentication failed` —lo que Compass/Navicat/mongosh muestran como *invalid
  credentials*— y el servidor lo registra indicando que falta `-user`/`-pass`.
- Con `-user`: el wire exige SCRAM-SHA-256. Tras autenticar, si el motor tiene RBAC
  activo se crea una `Session` del motor con ese usuario (permisos y `fieldDeny` se
  aplican igual que en Go): `-user`/`-pass` deben ser un usuario **existente en la
  base** (`_users`, consola web → Usuarios), no una credencial independiente.
- Diagnósticos de arranque: se imprime qué store sirve el wire, qué bases de `-dbdir`
  quedan solo para la consola web (y cómo servirlas: `-db <nombre>`), y si `-user`
  no existe en la base (con la lista de usuarios válidos).

## Particularidades del subset

- `_id`: ObjectId y números se normalizan a string (hex del ObjectId).
- Números: BSON int32/int64/double → el motor es numérico tipo JSON
  (5 y 5.0 son iguales al filtrar).
- Sort multi-clave: se aplica en orden alfabético de campos.
- Proyecciones punteadas y `$elemMatch/$size/$type/$mod/$all/$expr`
  no están soportados (error `BadValue` / `CommandNotFound`).
- Compresión (`OP_COMPRESSED`) no anunciada; checksums OP_MSG verificados.
- Cursores con TTL de 10 min de inactividad; batch default 101 docs.
- `maxBsonObjectSize` = 1 MB (límite real del motor).

## Ejemplo de sesión completa (mongosh)

```javascript
// conectar: mongosh mongodb://127.0.0.1:28917
use midb

db.createCollection("clientes")
db.clientes.insertOne({ _id: "c1", nombre: "Ana", email: "ana@x.com" })
db.clientes.createIndex({ email: 1 }, { unique: true })

db.clientes.find({ nombre: /^A/ })
db.clientes.aggregate([
  { $group: { _id: "$city", n: { $sum: 1 } } }
])

db.clientes.updateOne({ _id: "c1" }, { $set: { nivel: "gold" } })
db.clientes.deleteOne({ _id: "c1" })
db.clientes.drop()
```
