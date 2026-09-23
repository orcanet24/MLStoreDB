# Distribución de archivos

## 1. Situación actual (módulo independiente)

```
mlstoredb/                   # module mlstoredb (go 1.26.5)
├── go.mod / go.sum
├── db/                      # ← EL MOTOR (41 archivos .go: 21 prod + 20 test)
│   ├── ... (ver árbol abajo)
├── wire/                    # ← M8: wire protocol Mongo (11 archivos: 10 prod + 1 test)
│   ├── bson.go · msg.go · cursor.go · server.go · handshake.go
│   ├── commands.go · query.go · update.go · agg.go · scram.go
│   └── wire_test.go
├── tools/
│   ├── smoke/main.go        # smoke: open → index → insert → find → close
│   ├── loadtest/main.go     # prueba de carga 10k (disco + cifrado)
│   └── mls-server/main.go   # M8: servidor Mongo-compatible (CLI)
├── scripts/
│   └── test-race.ps1        # -race con CGO + gcc MSYS2 (db + wire)
└── doc/                     # ← ESTA documentación (aislada de ML)
    ├── README.md · ARQUITECTURA.md · API.md · CONSULTAS.md
    ├── FORMATO_ARCHIVO.md · PRUEBAS.md · RECREAR.md
    ├── PLAN_V2.md           # bitácora plan v2 (M0→M9)
    └── manual/              # M8: manual de usuario ES/EN + manual.html
        ├── es/ (9 temas) · en/ (9 temas) · README.md · manual.html
```

---

## 2. Mapa del package `db`

### 2.1 Producción (21 archivos, ~7.541 líneas)

| Archivo | Líneas | Responsabilidad |
|---|---:|---|
| **store.go** | 1419 | Store, CRUD con fases de hooks, Find/Count/planner, sort, clone, backpressure |
| **index.go** | 854 | Index hash+ord, encode/decode claves, seek range/prefix, matrix CSR en RAM |
| **file.go** | 670 | Header 152B, crypto (KEK/DEK), `Open`/`Flush`/`Close`, `Options`, `ResolveMachineID` glue |
| **filev2.go** | 1017 | Formato v2: records DOC/META/IDX/DEL/COMMIT, page cache hook, append/compact, migración v1→v2 |
| **auth.go** | 572 | RBAC: `Permission`/`Session`, `Authenticate`, users/roles (`_users`/`_roles`), fieldDeny |
| **graph.go** | 464 | M6: `edges.<tipo>`, CSR de adyacencia, `Neighbors`/`Traverse`/`ShortestPath` |
| **triggers.go** | 458 | M5b: triggers JSON (`_triggers`), actions, `$get` templates, adminInsert/adminDelete |
| **filter.go** | 369 | match(), operadores, lookup dotted-path |
| **hooks.go** | 302 | M5a: eventos before/after, On/OnAsync/Off, depth/ciclo, worker FIFO, `WaitAsyncHooks` |
| **repair.go** | 269 | `Repair()` semántico |
| **export.go** | 223 | ExportCSV, sensitive, Migration/ApplyMigrations |
| **cache.go** | 184 | Page cache second-chance, `docEntry`, `CacheStats`, `loadEntry` |
| **machineid.go** | 140 | `ResolveMachineID`: identidad ULID por instalación |
| **rotate.go** | 135 | `RotateKeys` header-only |
| **parallel.go** | 85 | `materializeRefs` paralelo (Find/Count) |
| **runtime.go** | 95 | flock, Snapshot, auto-flush ticker, `OpenWithLock` |
| **filter_regex.go** | 49 | compileCached + applyRegex |
| **errors.go** | 17 | Sentinels (incl. `ErrUnauthorized`/`ErrForbidden`/`ErrHookCycle`/`ErrHookDepth`/`ErrExists`) |
| **machine_windows.go** | 17 | MachineGuid (build tag windows) |
| **machine_nonwindows.go** | 7 | fallback "" (build tag !windows) |
| **zlib_helpers.go** | 21 | zlib writer/reader + crypto/rand |
| **Producción** | **~7541** | |

### 2.2 Tests (20 archivos, ~5.366 líneas) — 173 PASS

| Archivo | Líneas | Tests | Cubre |
|---|---:|---:|---|
| store_test.go | 188 | 13 | CRUD base, sort/limit/skip, aislamiento caller |
| filter_test.go | 186 | 10 | matriz de operadores, dotted path, lógicos |
| index_test.go | 231 | 14 | unique, compound, multi-valor, planner, projection |
| index_matrix_test.go | 245 | 5 | M2: serialize/roundtrip/checksum/invalidación |
| file_test.go | 158 | 7 | roundtrip, corrupt, wrong key |
| filev2_test.go | 418 | 7 | M1: formato v2, cache, torn-tail, Compact, migración |
| repair_test.go | 256 | 9 | FlushSync, SyncOnWrite, Repair, corrupt |
| runtime_test.go | 158 | 8 | snapshot, flock, auto-flush, lock file custom |
| export_test.go | 137 | 7 | CSV BOM, migraciones, sensitive |
| hardening_test.go | 410 | 16 | range seek, planner, dirtyGen, deep clone, regex, sensitive |
| machineid_test.go | 210 | 7 | ResolveMachineID |
| rotate_test.go | 310 | 10 | RotateKeys |
| options_test.go | 154 | 4 | Options defaults, lock custom, auto-flush custom |
| parallel_test.go | 326 | 4 | M3: Find/Count paralelos, backpressure, COW |
| auth_test.go | 406 | 11 | M4: RBAC, sesiones, fieldDeny, system colls |
| hooks_test.go | 373 | 13 | M5a: before/after, async FIFO, depth/ciclo, bypass |
| triggers_test.go | 322 | 11 | M5b: actions, templates, reopen, async |
| graph_test.go | 322 | 8 | M6: Neighbors/Traverse/Path, invalidación, RBAC grafo |
| colladmin_test.go | 195 | 6 | M8: UpdateFields, Create/DropCollection, no-resurrección, Session admin |
| bench_test.go | 326 | 3+11 | límites 1MB + benchmarks |
| **Tests** | **~5366** | **173** | |

**Total motor: 41 archivos · ~12.907 líneas**

### 2.3 Package `wire` (M8) — 11 archivos

**Producción (10 archivos, ~4.293 líneas):**

| Archivo | Líneas | Responsabilidad |
|---|---:|---|
| **bson.go** | 740 | Codec BSON↔Document, extended-JSON ($oid/$date/$binary/$numberLong), claves ordenadas |
| **commands.go** | 700 | insert/find/getMore/killCursors/count/distinct/update/delete/findAndModify, proyecciones, batching |
| **update.go** | 583 | Operadores de update Mongo ($set/$unset/$inc/$push/$pull/…) → diff patch/remove |
| **server.go** | 503 | Server/Serve/Close, dispatch, resolución de comando, mapeo errores→códigos Mongo |
| **agg.go** | 434 | aggregate: $match/$sort/$skip/$limit/$count/$group/$unwind/$project/$lookup |
| **handshake.go** | 392 | hello/isMaster/ping/buildInfo + admin (listCollections/indexes/stats/create/drop) |
| **msg.go** | 351 | Framing OP_MSG/OP_QUERY/OP_REPLY/OP_GET_MORE/OP_KILL_CURSORS + checksum CRC32-C |
| **query.go** | 298 | Traducción filtro Mongo→motor, regex, normalización _id |
| **scram.go** | 210 | SCRAM-SHA-256 server → sesión engine |
| **cursor.go** | 82 | Registro de cursores con TTL |

**Tests (1 archivo, ~1.030 líneas) — 28 PASS:** `wire_test.go` — BSON
roundtrip, handshake OP_MSG/legado, CRUD completo, cursores, aggregate,
índices admin, errores/writeErrors, proyecciones, find legado + getMore,
secuencias kind-1, checksum, SCRAM ok/fail, RBAC, edges por wire.

**Total wire: 11 archivos · ~5.323 líneas**
**Total módulo: 52 archivos .go en packages · ~18.230 líneas (+ tools/)**

---

## 3. Dependencias (go.mod)

```
module mlstoredb
go 1.26.5

require (
    github.com/gofrs/flock v0.13.1     // file lock de proceso
    github.com/oklog/ulid/v2 v2.1.2    // _id auto
    golang.org/x/crypto v0.57.0        // argon2
    golang.org/x/sys v0.48.0           // windows/registry (MachineGuid)
)
```

Stdlib usada: `encoding/json`, `crypto/{aes,cipher,hmac,sha256}`, `compress/zlib`, `encoding/csv`, `encoding/binary`, `regexp`, `sync`, `sync/atomic`, `sort`, `os`, `path/filepath`, `time`, `errors`, `fmt`, `io`, `strconv`, `strings`, `bytes`, `testing`.

**Sin CGO en producción.** Compila puro en windows/amd64, linux, darwin (CGO solo para `-race`).

---

## 4. Responsabilidades por capa

### 4.1 store.go — núcleo del store

- Tipo `Document`, `Store`, `collection`
- CRUD: `Insert`, `Upsert`, `Update`, `Delete`, `Get` (+ variantes con `*Session` / hooks M5a)
- Consulta: `Find`, `Count`, `Explain`, `Collections`, `SchemaVersion`
- Índices (API): `EnsureIndex`, `DropIndex`, `ListIndexes`, `applyIndexes`
- Planner: `planCandidates`, `fieldPlan`, `seekEqualityField`, `findIndexForRange`, `intersectIDs`, `rangeValues`, `equalityValues`
- Sort: `sortRefsByID`, `sortByPartial`, `docHeap`, `sortBy`, `compareValues`
- Clone: `clone`, `cloneShallow`, `cloneValue`, `project`
- Concurrencia M3: `waitBackpressureLocked` (writeSeq/durableSeq), cores `*Apply`, `previewWrite`/`previewDoc` (fases hook)
- Límite: `maxDocBytes` (1MB), `docID` (ULID), `checkDocSize`

### 4.2 index.go — estructuras de aceleración

- `index`, `ordEntry`, `newIndex`, `info`
- Orden: `typeRank`, `compareOrdered`, `ordLess`, `ensureOrd`, `ordAdd`, `ordRemove`
- Mutación: `addDoc`, `removeDoc`, `addKey`, `removeKey`
- Seek: `lookupIDs`, `countKey`, `seekPrefix`, `seekRange`, `collectPrefixScan`, `orderedIDs`, `idsSortedForKey`
- Encoding: `extractIndexRows`, `encodeIndexKey`, `encodeIndexValue`, `sameFields`
- `rangeBound` (lo/hi inclusivo/exclusivo)
- M2: `csrMatrix`, `serialize`, `loadCSR`, `alignToOrd` (matriz persistida en IDX)

### 4.3 filter.go — lenguaje de consulta

- `match`, `matchField`, `matchOpsMulti`, `matchLogical`, `matchOps`, `matchOneOp`
- Comparación: `compareOp`, `comparableTypes`
- Lógicos: `$and $or $not $nor` + `isLogicalOp`
- Paths: `lookup`, `lookupMulti`, `lookupPartsMulti` (dot + arrays)
- Helpers: `toDoc`, `toDocList`, `toAnyList`, `hasOpKeys`, `equalJSON`, `toFloat`

### 4.4 file.go + filev2.go — formato y crypto

- Constantes: `magicStr`, `formatVersion=2`, `headerSize=152`, `flagCompressed`, params KDF
- `Options` (+ `CacheBytes`, `FindWorkers`, `MaxPendingWrites`, `SessionTTL`), `machineID()`, `errNoMaster`
- `header` (marshalBody, setHMAC, verifyHMAC, parseHeader)
- Crypto: `deriveKEK`, `sealGCM`, `openGCM`
- Payload v1: `fileData`, `fileMeta`, `fileColl`, `toFileData`, `loadFileData`
- **v2 (filev2.go):** records `DOC/META/IDX/DEL/COMMIT` (CRC Castagnoli), `scanV2`, `openV2`, `appendV2`/`writeFullV2`, `Compact`, migración v1→v2 (`prepareV1Migration`)
- Compresión: `compressIfNeeded`, `decompressIfNeeded`
- Escritura: `atomicReplace`, `randomBytes`
- Ciclo: `Open`, `Flush`, `Close`, `markDirty`
- Flush no bloqueante: `flushState`, `prepareFlush`, `writeState`

### 4.5 cache.go — page cache (M1)

- `docEntry` (residentes + segunda oportunidad), `resident`/`loadEntry` (docs fríos desde disco)
- `CacheStats`, `cacheAdmit`, `evictLocked`/`evictEntryLocked` bajo `cacheMu`
- Budget: `Options.CacheBytes` (0=256 MiB, <0=ilimitado)

### 4.6 parallel.go — Find/Count paralelos (M3)

- `materializeRefs` (umbral `parallelMinIDs=512`), `materializeRefsSerial`, `countMatches`
- `Options.FindWorkers`: 0=GOMAXPROCS, <0=serie, >0=n

### 4.7 runtime.go — proceso y backup

- `acquireLock` (flock `mlstoredb.lock`)
- `Snapshot(dest)` — backup cifrado independiente
- `startAutoFlush` / `stopAutoFlush` (ticker 2s)
- `OpenWithLock` — Open + lock + ticker

### 4.8 export.go — salida e evolución

- `sensitiveFields` (lista negra global)
- `SetSensitiveFields`, `SensitiveFields` (por colección, persistido)
- `ExportCSV` (BOM UTF-8, columnas top-level, omit sensitive) + `exportCSV(sess,…)`
- `csvValue` (anidados → JSON string)
- `Migration`, `ApplyMigrations` (solo Up, orden por versión)

### 4.9 auth.go — RBAC embebido (M4)

- `Permission`, `Session`, `AuthActive`
- `Authenticate` → `*Session` (argon2id PHC), `CreateUser`, `CreateRole`, `ChangePassword`, `Revoke`
- Gates: `checkReadLocked`/`checkWriteLocked`, `fieldDeny` + `redactDoc`/`filterTouchesFields`
- System colls: `_users`, `_roles` (lazy, CRUD genérico → `ErrForbidden`)

### 4.10 hooks.go — hooks Go (M5a)

- Eventos `before_/after_insert|update|delete|upsert`, `On`/`OnAsync`/`Off`
- `HookContext`, `hookFrame` (depth ≤8, ciclo → `ErrHookCycle`), FIFO async (1024)
- Worker parametrizado, `WaitAsyncHooks`, `hookBypass` (system session)

### 4.11 triggers.go — triggers JSON (M5b)

- `Trigger`/`TriggerAction`, `CreateTrigger`/`DeleteTrigger`/`ListTriggers`
- System coll `_triggers`, templates `{"$get":…}`, `adminInsert`/`adminDelete`
- Registrados como hooks M5a; `ensureTriggersLoaded` en `Open`

### 4.12 graph.go — grafo (M6)

- Colecciones `edges.<tipo>` (`_from`/`_to`), CSR en RAM (`buildAdj`, epoch de invalidación)
- `AddEdge`/`RemoveEdge`, `Neighbors`, `Traverse` (BFS con límites), `ShortestPath`
- Lock order: `graphMu` → `s.mu`

### 4.13 filter_regex.go

- `regexCache sync.Map`, `compileCached`, `applyRegex`
- Errores de patrón se cachean como sentinel → `ErrBadFilter` repetido barato

### 4.14 machine_*.go

- Windows: lee `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`
- Otros OS: `""` → caller cae a `"default"` (salvo `Options.MachineID` explícito)

### 4.15 zlib_helpers.go

- `newZlibWriter`, `zlibDecompress`, `readRandom`

### 4.16 errors.go

```
ErrNotFound    ErrDuplicate   ErrCorrupt   ErrTooLarge
ErrAlreadyOpen ErrNoID        ErrBadFilter
ErrUnauthorized ErrForbidden  ErrHookCycle ErrHookDepth
```

### 4.17 repair.go / rotate.go / machineid.go

- `Repair(path, opts)` — reescritura semántica
- `RotateKeys(newMaster, newMachineID)` — re-wrap DEK header-only
- `ResolveMachineID(dbPath, explicit)` — ULID por instalación (`mlstoredb.machineid`)

---

## 5. Comandos relacionados

| Comando | Qué hace |
|---|---|
| `go test ./db -count=1` | suite completa |
| `go test ./db -bench=. -benchtime=1x -run=XXX` | benchmarks |
| `go run ./tools/loadtest` | carga 10k end-to-end con disco |
| `go run ./tools/smoke` | smoke: open → index → insert → find → close |
| `go vet ./...` | lint estático |
| `go build ./...` | compilación |
| `powershell -File scripts/test-race.ps1` | `-race` con CGO+gcc |

---

## 6. Qué copiar para un proyecto aislado

Mínimo imprescindible (package completo, sin renombrar nada):

```
db/*.go                    # los 40 archivos
go.mod                     # module nuevo + 4 dependencias
```

Opcional de referencia:

```
doc/*                      # esta documentación
tools/loadtest/main.go     # medición de perf
tools/smoke/main.go        # smoke
```

Ver [RECREAR.md](RECREAR.md) para el checklist de extracción.
