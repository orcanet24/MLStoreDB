# PLAN v2 — MLStoreDB: BD JSON + Grafos (bitácora de desarrollo)

Plan maestro ejecutado fase por fase. Cada fase se cierra solo con todos sus
gates en verde; no se avanza a la siguiente con fases abiertas.

## Decisiones cerradas (con el usuario)

| Decisión | Elección |
|---|---|
| Base de código | Traer fuente de MLD (`internal/mlstore`) y evolucionarla |
| Triggers | Hooks Go (M5a) **y** triggers declarativos JSON (M5b) |
| Autenticación | Embebida en la BD (`Authenticate` → `*Session`, `_users`/`_roles`) |

## Arquitectura v2 (resumen)

- **Único archivo cifrado** con la jerarquía actual: Argon2id KEK → DEK → AES-256-GCM.
- **Matriz de índices** (`index_matrix`, formato CSR) pre-cargada en RAM y
  persistida dentro del archivo; fallback de rebuild por checksum.
- **Paginación en RAM**: documentos en páginas selladas en disco + page cache
  LRU con presupuesto (`Options.CacheBytes`); heap estable al crecer los datos.
- **Footer/manifest** como único commit point (época atómica, sin WAL).
- **Concurrencia**: RWMutex por colección, striping por docID, `Find` paralelo
  acotado a `GOMAXPROCS`, backpressure con semáforos y colas bounded.
- **RBAC**: `_users` / `_roles`, sesiones, permisos por colección + `fieldDeny`.
- **Hooks/triggers** con depth-limit, detección de ciclos y cola FIFO bounded.
- **Grafo**: colecciones `edges.<tipo>` (`_from`/`_to`), adyacencia CSR en RAM,
  `Neighbors` / `Traverse` / `ShortestPath` con límites anti-ahogo.

## Estado de fases

| Fase | Estado | Gates |
|---|---|---|
| M0 Extracción a `package db` | ✅ | `go build` · `go vet` · 114 PASS / 0 FAIL · smoke OK · loadtest OK · `-race` verde |
| M1 Formato v2 paginado + page cache | ✅ | `go build` · `go vet` · 115 PASS / 0 FAIL (7 nuevos v2) · smoke OK · loadtest PASS · `-race` verde |
| M2 `index_matrix` CSR persistida | ✅ | `go build` · `go vet` · 120 PASS / 0 FAIL (5 nuevos matrix) · smoke OK · loadtest PASS · `-race` verde |
| M3 Concurrencia sharded + paralela | ✅ | `go build` · `go vet` · 124 PASS / 0 FAIL (4 nuevos parallel) · smoke OK · loadtest PASS · `-race` verde |
| M4 RBAC embebido | ✅ | `go build` · `go vet` · 135 PASS / 0 FAIL (11 nuevos auth) · smoke OK · loadtest PASS · `-race` verde |
| M5a Hooks Go | ✅ | `go build` · `go vet` · 148 PASS / 0 FAIL (13 nuevos hooks) · smoke OK · loadtest PASS · `-race` verde |
| M5b Triggers declarativos | ✅ | `go build` · `go vet` · 159 PASS / 0 FAIL (11 nuevos triggers) · smoke OK · loadtest PASS · `-race` verde |
| M6 Grafo (edges + traverse) | ✅ | `go build` · `go vet` · 167 PASS / 0 FAIL (8 nuevos graph) · smoke OK · loadtest PASS · `-race` verde |
| M7 Docs + benchmarks + race final | ✅ | `go build` · `go vet` · 167 PASS / 0 FAIL · smoke OK · loadtest PASS · benchmarks 11 OK · `-race` verde · docs `doc/*` actualizados |
| M8 Wire protocol Mongo-compatible | ✅ | `go build` · `go vet` · 201 PASS / 0 FAIL (28 nuevos wire + 6 motor) · smoke OK · loadtest PASS · `-race` verde (db+wire) · e2e mls-server OK · manual ES/EN |
| M9 (post-plano) Consola web: administración + grafos (phpMyAdmin + Neo4j Browser) | ⬜ pendiente de aprobación | |

---

## M0 — Extracción del motor (completado)

**Origen:** `C:\Proyectos\GO\ML\internal\mlstore` (26 archivos `.go`),
`cmd/loadtest`, `scripts/test-race.ps1`.

**Estructura creada:**

```
MLStoreDB/
├── go.mod                 # module mlstoredb (go 1.26.5, mismas 4 deps)
├── db/                    # package db (era package mlstore) — 26 archivos
├── tools/loadtest/        # carga 10k end-to-end
├── tools/smoke/           # smoke mínimo open→index→insert→find→reopen
├── scripts/test-race.ps1  # -race con CGO+gcc (ruta ./db/)
└── doc/                   # especificación original + esta bitácora
```

**Renombres aplicados (semántica intacta):**

| Antes | Después |
|---|---|
| `package mlstore` | `package db` |
| prefijo de errores `mlstore:` | `db:` |
| lock `mldstore.lock` | `mlstoredb.lock` |
| install id `mldstore.machineid` | `mlstoredb.machineid` |
| `fileMeta.App = "mld"` | `"mlstoredb"` |
| import en tools `mld/internal/mlstore` | `mlstoredb/db` |

Magic del archivo se mantiene `MLDB` (compatibilidad con la especificación en
`doc/FORMATO_ARCHIVO.md`).

**Corrección de race preexistente (gate `-race`):** `startAutoFlush` leía
`s.path` y `s.opts.autoFlushInterval()` fuera del lock mientras `RotateKeys`
escribía `s.opts` bajo `s.mu` → captura de path+intervalo bajo `s.mu.RLock`
antes de spawnear el ticker (`db/runtime.go`).

**Gates M0:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **114 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M1 — Formato v2 paginado + page cache (completado)

**Formato (`formatVersion=2`)** — bitcask-style log cifrado tras el header de
152 B (`MLDB`); sin cuerpo JSON en claro.

| Campo | Tamaño | Detalle |
|---|---|---|
| type | 1 | DOC=1 META=2 IDX=3 DEL=4 COMMIT=5 |
| flags | 1 | bit0 zlib · bit1 suspect (id no confiable) |
| idLen | 2 LE | blob id = u16be(collLen)+coll+docID (en claro) |
| payLen | 4 LE | longitud del payload AES-GCM |
| nonce | 12 | AES-256-GCM |
| crc | 4 LE | Castagnoli sobre `hdr[0:20] ‖ id ‖ ct` |
| id blob | idLen | identificador en claro para indexado en scan |
| ciphertext | payLen | AES-256-GCM(DEK, nonce, plain \| zlib) |

**Reglas de integridad:**

- **COMMIT** es el único commit point; scan aplica solo registros hasta el
  último COMMIT completo.
- Torn tail a mitad de append → self-heal (`truncate` a COMMIT).
- CRC falla en registro completo → `ErrCorrupt`.
- Torn **sin** COMMIT previo en el offset del header → `ErrCorrupt`
  (la primera escritura es atómica y siempre termina en COMMIT).
- Sin COMMIT → truncar a `headerSize` (datos no confirmados descartados).
- Header alterado → HMAC falla → `ErrCorrupt`.

**Campos IDX / META (autoritativos en v2):**

- META payload = `{App, SchemaVersion, Collections{Indexes, Sensitive}}`.
- IDX payload = `{Coll, Fields, Unique, Rows[{K,V,IDs}]}` (orden canónico);
  unique multi-id → `ErrDuplicate`; IDX faltante → eager fallback.

**Page cache (paginación en RAM):**

- `collection.entries map[string]*docEntry` reemplaza `docs`; docs fríos se
  cargan bajo demanda (`loadEntry` → lectura del record desde disco).
- Registry bajo `cacheMu` (sin tomar `s.mu`); segunda oportunidad (clock);
  presupuesto `Options.CacheBytes` (0=default 256 MB, negativo=ilimitado).
- Sin eviction cuando `path==""` o `noEvict` (ventana de migración v1).
- API pública: `Store.CacheStats() → {Resident, Cold, Bytes, Budget, Evictions, TotalEntries}`.

**Commit / write path:**

- Full rewrite (atómico tmp+rename) cuando: `!sawFile || v1Migration ||
  compactForce || snapshot a otro destino || path==""`.
- Append incremental (`O_APPEND`) en el caso normal; DEL se encola solo si
  `sawFile || flushInProgress`.
- API interna preservada para tests: `prepareFlush(path, clearDirty) → *flushState`
  con `st.gen`; `writeState(st) error`; campos `s.dirty` / `s.dirtyGen`.

**APIs nuevas:** `Store.Compact()` (rewrite completo sin DOC/DEL muertos),
migración v1→v2 en el primer flush.

**Tests nuevos (`db/filev2_test.go`):** formato/commit-point · cold+eviction ·
torn-tail self-heal · Compact · migración v1→v2 · delete no-reappear ·
append incremental.

**Gates M1:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **115 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M2 — `index_matrix` CSR persistida (completado)

**Idea:** la vista aplanada de cada índice (filas `ord` × columnas docID) se
serializa como **CSR** dentro del payload IDX ya cifrado, con **checksum** y
fallback; en RAM se mantiene como matriz viva mientras no haya mutaciones.

**Formato del payload IDX (aditivo — JSON ignora campos desconocidos):**

```json
{
  "c": "coll", "f": ["status"], "u": false,
  "rows": [ { "k": "...", "v": ..., "ids": ["..."] } ],
  "csr": {
    "keys": ["..."], "vals": [...],
    "indptr": [0, n1, ...], "indices": [col, ...],
    "ids": ["doc1", ...], "csum": 1234567890
  }
}
```

**Carga (`loadRows`):**

1. `Fields`/`Unique` vs META → `errIdxMismatch` → eager (sin cambios).
2. `csr` presente **y** `valid()` (forma + CRC32-C) → `loadCSR` → materializa
   `entries`/`unique`/`ord` y deja `idx.matrix` vivo (alineado vía `alignToOrd`).
3. Checksum/forma falla → **fallback a `rows`**.
4. Ambos ausentes/corruptos → error ya existente → eager rebuild.

**Seeks con matriz viva:** `seekRange` / `orderedIDs` recorren
`indices[indptr[i]:indptr[i+1]]` cuando `idx.matrix != nil && !ordDirty`.
`addKey`/`removeKey` limpian `idx.matrix`; `serialize()` lo reactiva en el flush.

**Tests (`db/index_matrix_test.go`):** serialize→reopen con matriz + IXSCAN ·
checksum corrupto → Rows · JSON roundtrip + tamper · invalidación en mutación ·
unique multi-ID → `ErrDuplicate`.

**Compatibilidad:** payload aditivo; sin bump de `formatVersion`.

**Gates M2:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **120 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M3 — Concurrencia: Find paralelo + backpressure (completado)

**Idea:** materializar refs de páginas frías en paralelo (sin `s.mu`), limitar
escrituras sin flush (backpressure por seq) y hacer `Update` copy-on-write
para que un fallo de validación no deje el doc vivo a medio parchear.

**`db/parallel.go` — `materializeRefs`:**

- Chunking contiguo sobre `ids` en `Options.findWorkers()` goroutines; cada
  worker llama `loadEntry` **sin** `s.mu`/`flushMu` (registro de caché con
  `cacheMu`; I/O puntual bajo `s.mu` solo en `loadEntry` — ya existente).
- Umbral `parallelMinIDs=512`; por debajo → serie. Orden preservado
  (concat de chunks). Usado por `Find` (COLLSCAN e IXSCAN) y `Count`.
- `FindWorkers`: 0=`GOMAXPROCS`, `<0`=1 (serie), `>0`=n.

**Backpressure (`store.go` + `file.go`):**

- `writeSeq` se incrementa en `markDirty`; `durableSeq` se fija desde
  `flushState.writeSeq` en `applyFlushed`; `bpCond *sync.Cond` sobre `s.mu`
  (init en `New()`).
- `waitBackpressureLocked()` al inicio de insert/upsert/update/delete:
  espera mientras `flushInProgress && writeSeq-durableSeq >= MaxPendingWrites`.
- `Broadcast` en `applyFlushed` y en **todos** los paths de error de
  `prepareFlush`/`writeState` (corrige fuga preexistente de `flushInProgress`).

**Update copy-on-write:**

- Construye `next := cloneShallow(doc)` + patch, valida tamaño/índices **antes**
  de `docP.Store(&next)` → fallo deja el doc vivo intacto.
- Fix de test: el goroutine de flush no comparte `WaitGroup` con writers
  (deadlock stop-close); aserción int-vs-float64 corregida.

**Fix flaky:** `ResolveMachineID` (`machineid.go`) — ventana O_EXCL
crear→escribir se leía vacía → reintenta 8×2ms; vacío persistente sigue
errorando (preserva `TestResolveMachineIDEmptyFileRegenerates`).

**Tests (`db/parallel_test.go`):** parallel≡serial (Find+Count, IXSCAN y
COLLSCAN) · Find∥Upsert∥Flush · backpressure se libera tras flush ·
COW fallo no muta doc vivo.

**Gates M3:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **124 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M4 — RBAC embebido (completado)

**Idea:** autenticación y autorización dentro del motor: usuarios/roles en
colecciones de sistema, `Authenticate` → `*Session`, chequeos en el CRUD.
**Permisivo por defecto:** sin usuarios, la API `Store` cruda funciona igual
que siempre (los 124 tests previos no cambian).

**Colecciones de sistema (lazy — nunca en `New()`/`Open()`):**

- `_users`: `{_id, password_hash (argon2id PHC), roles[], disabled?}`.
  Creada solo en `CreateUser` → `TestCountAndCollections` intacto.
- `_roles`: `{_id, permissions:[{collection, read, write, fieldDeny[]}]}`.
  `collection:"*"` = wildcard. Creada solo en `CreateRole`.
- Ambas **ocultas** de `Collections()` e inaccesibles vía CRUD genérico
  (`checkWriteLocked` → `ErrForbidden` siempre; previene usuarios falsos
  y tamping de hash). Admin APIs escriben por path dedicado.

**Modelo de sesiones:**

- `Store.Authenticate(user, pass) → *Session` (user desconocido y pass
  malo → `ErrUnauthorized`, sin enumeración). Token aleatorio 32B base64url.
- Permisos resueltos al login (snapshot). `Session` con `Get/Find/Count/
  Insert/Upsert/Update/Delete/ExportCSV` + `Revoke/User/Roles/Token`.
- Expiración: `Options.SessionTTL` (0=24h, <0=nunca). GC lazy en cada
  `Authenticate` (`purgeExpiredLocked`). `Revoke(token)` y `ChangePassword`
  (revoca sesiones del usuario) invalidan de inmediato
  (`validLocked` verifica mapa + expiry bajo `s.mu`).

**Chequeos (bajo `s.mu`):**

- `authActiveLocked()` = `len(_users.entries) > 0`.
- Nil session (API `Store` cruda): activo → `ErrUnauthorized`; inactivo → ok.
- Session: `validLocked` → `ErrUnauthorized`; coll de sistema → `ErrForbidden`;
  sin permiso de lectura/escritura → `ErrForbidden`.
- `fieldDeny`: se redacta de resultados (`Get`/`Find`/`ExportCSV`) y los
  filtros que **sonpean** campos denegados (top-level + recursión
  `$and/$or/$nor/$not` via `filterTouchesFields`) → `ErrForbidden`
  (bloquea sondeo vía `Count`/`$gt` etc.).

**Passwords:** argon2id PHC (`$argon2id$v=19$m=..,t=..,p=..$salt$hash`),
params siguen `Options.LightKDF` (tests rápidos). Constant-time compare.

**APIs nuevas:** `CreateUser`, `CreateRole`, `ChangePassword`, `Revoke`,
`Authenticate`, `AuthActive`, métodos de `Session`, `Options.SessionTTL`,
errores `ErrUnauthorized`/`ErrForbidden`.

**Tests (`db/auth_test.go`):** permissive default · activación en 1er user ·
auth completo de CRUD · read-without-write · fieldDeny (Get/Find/Count/CSV +
`$or`) · revoke · expiración · ChangePassword revoca · system colls ·
validación de roles · persistencia reopen (fieldDeny + auth activos).

**Gates M4:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **135 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M5a — Hooks Go (completado)

**Idea:** callbacks registrados por evento CRUD con **depth-limit**,
**detección de ciclos** y **cola FIFO bounded** para la vía asíncrona.

**Eventos (strings, reutilizables por M5b):**
`before_insert|after_insert|before_update|after_update|before_delete|
after_delete|before_upsert|after_upsert`.

**API:**

- `On(event, coll, fn) (id, err)` — síncrono. `coll` `""`/`"*"` = todas.
  Before-*: corre antes de aplicar; **error = veto** (mutación no ocurre).
  After-*: corre inline tras aplicar; errores ignorados (mutación ya vale).
- `OnAsync(event, coll, fn)` — solo `after_*` (un hook async no puede
  vetar). Encola en FIFO acotado (`hookQueueSize=1024`; enqueue **bloquea**
  si está lleno (backpressure). Worker lazy; para en `Close` (jobs restantes
  se descartan). `WaitAsyncHooks()` = barrera para tests/ops.
- `Off(id) bool` — desregistra.

**Payload `HookContext`:** `Event, Collection, ID, Doc, Old` +
helpers `Get/Find/Count/Insert/Upsert/Update/Delete`.

**Protecciones:**

- `hookFrame {parent, hookID, depth}` encadenado por mutación anidada.
- **Depth:** `depth > maxHookDepth (8)` → `ErrHookDepth`.
- **Ciclo:** mismo `hookID` ya en la cadena → `ErrHookCycle`
  (before: veto con error; after async: job se descarta).
- Las mutaciones anidadas **deben** ir por `HookContext.*` (propagan el
  frame). API `Store.*`/`Session.*` pública arranca frame=nil (raíz).

**RBAC:** hooks corren como `hookBypass` (`Session.system=true`): confiables,
no requieren sesión; igual **no** pueden tocar `_users`/`_roles`.

**Fases por mutación (sin hooks registrados → cero overhead, paths
originales intactos):** preview (RLock: auth + existencia + Old) →
before-* (sin lock, puede mutar `Doc`/patch) → apply (write lock, auth de
nuevo) → after-* (inline o encolado).

**Tests (`db/hooks_test.go`):** veto before · before muta doc · after sync +
match `""`/`"*"` · Off · update/delete payload (`Old`/`Doc`) ·
anidado+depth-limit · ciclo before → `ErrHookCycle` · depth before →
`ErrHookDepth` · async FIFO 50 + Wait · `OnAsync(before_*)` rechazado ·
bypass RBAC + system coll prohibido · backpressure/cierre.

**Gates M5a:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **148 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M5b — Triggers declarativos JSON (completado)

**Idea:** reglas en JSON persistidas en la colección de sistema
**`_triggers`**, registradas como hooks M5a (reutiliza depth/ciclos/cola).

**Shape del trigger:**

```json
{
  "_id": "auto_audit",
  "event": "after_insert",
  "collection": "orders",
  "enabled": true,
  "async": false,
  "filter": {"status": "PAID"},
  "actions": [
    {"type": "set", "fields": {"stamped": true}},
    {"type": "unset", "names": ["temp"]},
    {"type": "insert", "collection": "audit",
     "doc": {"ref": {"$get": "_id"}, "note": "created"}},
    {"type": "update", "collection": "counters", "id": "orders",
     "set": {"last": {"$get": "_id"}}},
    {"type": "upsert", "...": "..."},
    {"type": "delete", "collection": "tmp", "id": "t1"}
  ]
}
```

**Templates:** cualquier valor `{"$get":"field"}` resuelve contra el `Doc`
del evento; `{"$get":"old.field"}` contra `Old`. `insert` sin `_id` recibe
ULID automático.

**Semántica por fase:**

- `set` en `before_*`: muta el `Doc` work-copy (persiste con la mutación).
  En `before_update`, el `Doc` es el **preview merged** (old+patch); al
  terminar los hooks se difea a `patch` + `remove[]` (así `unset` puede
  borrar campos — `updateApply` acepta `remove`).
- `set` en `after_*`: self-`Update` vía `HookContext`.
- `unset`: solo `before_*` (validado en `CreateTrigger`).
- `filter`: `match()` del motor contra `Doc` (o `Old` si `Doc` nil).

**APIs:** `CreateTrigger(t) (id, err)` (valida + persiste + `On/OnAsync`),
`DeleteTrigger(id)` (doc + `Off`), `ListTriggers()`. Admin path
`adminInsert`/`adminDelete` (sin hooks, sin RBAC — solo APIs admin).

**System coll `_triggers`:** se suma a `isSystemColl` (oculta de
`Collections()`, CRUD genérico → `ErrForbidden`). **`Open()` llama
`ensureTriggersLoaded()`** en fresh/v1/v2 → re-registra hooks al reopen.

**Validaciones:** evento conocido · `async` solo `after_*` · ≥1 acción ·
tipos de acción + campos requeridos · `unset` solo before.

**Tests (`db/triggers_test.go`):** set before + filter · insert audit con
`$get` + auto `_id` · unset en before_update (borrado real) · upsert
contador · ref `old.*` · delete action · validaciones · delete+list ·
disabled no registra · persistencia reopen · async + `WaitAsyncHooks`.

**Gates M5b:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **159 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M6 — Grafo: edges + CSR + traverse (completado)

**Idea:** aristas en colecciones normales `edges.<tipo>` con docs
`{_id, _from, _to, ...props}`; adyacencia **CSR en RAM** (keys ordenadas,
`indptr`, `indices` → tabla `neigh` de ids), cacheada y reconstruida por
**epoch** en cada mutación de una coll `edges.*`.

**Diseño (`db/graph.go`):**

- **CSR**: `buildAdj(pairs, reverse)` → dedup + sort de vecinos por fila;
  dos snapshots por tipo: `graphOut` (`_from→_to`) e `graphIn` (`_to→_from`).
- **Invalidación**: `noteEdgeMutation(coll)` (bajo `s.mu`) hace
  `graphEpoch.Add(1)` en `insertApply`/`upsertApply`/`updateApply`/`deleteApply`
  cuando `isEdgeColl(coll)`. **Orden de locks: `graphMu` → `s.mu`**
  (`ensureGraphLocked` toma `RLock`); `AddEdge` genérico y CRUD sobre
  `edges.*` también bumpa el epoch → el cache nunca queda stale.
- **Anti-ahogo**: `MaxDepth` default 5 (cap 32), `Limit` default 10 000
  (cap 100 000), `ShortestPath` depth default 6 (cap 32). Todo acotado.

**APIs:**

| API | Semántica |
|---|---|
| `AddEdge(type, from, to, props)` | insert en `edges.<type>`; `_id` ULID auto |
| `RemoveEdge(type, id)` | delete del doc de arista |
| `Neighbors(type, v, dir)` | `Outgoing`/`Incoming`/`Both` (unión dedupe) |
| `Traverse(TraverseOptions)` | BFS con `MinDepth`/`MaxDepth`/`Limit`, orden de descubrimiento, incluye start@0 |
| `ShortestPath(type, from, to, maxDepth)` | BFS no ponderado → `[from,…,to]` o `nil` |

Todas con variante `Session.*` (RBAC `checkRead`/`checkWrite` sobre
`edges.<type>`); la API `Store.*` cruda sigue `ErrUnauthorized` cuando
auth activo (consistente con M4).

**Tests (`db/graph_test.go`, 8):** AddEdge+Neighbors out/in/both →
validación → invalidación de cache (AddEdge/Delete/Insert/RemoveEdge) →
Traverse depth/min/limit/incoming/clamp → ShortestPath (directo, shallow,
inaccesible, misma arista) → dedupe multi-arista → RBAC (raw denied,
admin ok, sin perm → Forbidden) → tipos distintos + visibilidad en
`Collections()`.

**Gates M6:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./db/ -count=1` → **167 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M7 — Docs + benchmarks + race final (completado)

**Idea:** poner al día toda la documentación del motor con la realidad del
código tras M0–M6, correr benchmarks de referencia y cerrar con la suite
completa + race.

**Docs actualizados (`doc/`):**

- **README.md** → stats (167 PASS · prod 7.365 L · tests 5.136 L · 40 `.go`),
  índice con PLAN_V2, "Qué es" con RBAC/hooks/triggers/grafo/v2, ejemplo de
  arranque con `CreateUser`/`Authenticate`, tabla de estado con M0–M7 ✅ y
  M8 pendiente.
- **DISTRIBUCION_ARCHIVOS.md** → mapa real del package `db` (21 prod + 19
  test, líneas por archivo), dependencias del módulo `mlstoredb`, capas
  4.1–4.17 (cache, parallel, auth, hooks, triggers, graph, filev2), comandos
  `./db` + `tools/*`.
- **API.md** → package `mlstoredb/db`, `Options` completo (`CacheBytes`,
  `FindWorkers`, `MaxPendingWrites`, `SessionTTL`), `Compact`/`CacheStats`,
  nota de RBAC en CRUD, secciones nuevas **6 RBAC**, **7 Hooks**,
  **8 Triggers**, **9 Grafo**, sentinels completos, gaps con M1–M8, ejemplo
  integral.
- **PRUEBAS.md** → comandos `./db`/`tools/*`, referencia **167 PASS**,
  suites v2 (filev2/matrix/parallel/auth/hooks/triggers/graph), matriz de
  cobertura 1–16, loadtest y benchmarks con números del plan v2.
- **ARQUITECTURA.md** → diagrama con v2 + RBAC + hooks + grafo, modelo
  lógico (entries + system colls + edges), persistencia v2, seguridad con
  RBAC, decisiones D17–D22.
- **FORMATO_ARCHIVO.md** → nota de **format_ver=2** (log de records +
  COMMIT) apuntando a esta bitácora; header 152 B común v1/v2.
- **RECREAR.md** → paths actuales (`db/`, `tools/`, `doc/`), gate 167,
  roadmap con M1–M7 tachados y M8 listado.

**Benchmarks (`go test ./db -bench=. -benchtime=1x -run=XXX`)** → 11 OK
(Insert10k ~0.68 ms · GetComplex ~24 µs · Flush10k ~4.2 s · Reopen v2
~102 ms LightKDF). Números estables: `-benchtime=2s -count=3`.

**Gates M7:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./db/ -count=1` → **167 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `go test ./db -bench=. -benchtime=1x -run=XXX` → 11 benchmarks OK ✅
- `powershell -File scripts/test-race.ps1` → **ok** ✅

---

## M9 — Consola web: administración + grafos (pendiente)

**Idea:** una consola web profesional embebida que combine lo mejor de
**phpMyAdmin** (administrar la BD completa desde el navegador) y del
**Neo4j Browser** (explorar y construir grafos en un canvas interactivo),
sin escribir una línea de Go.

**Alcance previsto — consola de administración (estilo phpMyAdmin):**

- Colecciones: listar, crear, borrar, ver/crear índices.
- Documentos: explorar con paginación/filtros/sort, editor JSON, CRUD
  completo, export CSV.
- Seguridad RBAC (M4): usuarios, roles, permisos y `fieldDeny` gestionables
  desde la UI.
- Triggers (M5b): listar, crear, editar, habilitar/deshabilitar.
- Stats del motor: `CacheStats`, counts, tamaño de archivo, sesiones.

**Alcance previsto — canvas de grafos (estilo Neo4j Browser):**

- Nodos = documentos, aristas = `edges.<tipo>`; render interactivo con
  drag, zoom y pan.
- **Crear conexiones arrastrando** de un nodo a otro en el canvas →
  `AddEdge` (tipo + props en el momento).
- Exploración incremental: expandir vecinos (`Neighbors`), recorridos BFS
  (`Traverse`) y rutas más cortas (`ShortestPath`) resaltados en el grafo.
- **Agrupación visual de grafos**: por colección, tipo de arista o
  propiedad (clusters/fold).
- Panel de consultas: consultas documentales y de grafo con resultados
  como tabla **o** como grafo.

**Decisiones técnicas previstas (se cierran al arrancar M9):**

- Servidor HTTP propio embebido en Go (no depende del wire protocol M8;
  pueden convivir en el mismo binario).
- UI servida como assets estáticos embebidos en el binario (sin build de
  node, sin CDN externo).
- Auth vía RBAC existente (`Authenticate` → `Session`, cookie/token).

**Dependencia:** ninguna dura de M8; se ejecuta tras cerrar M8 para no
abrir dos frentes de red simultáneos.

**Registrado:** 2026-09-23 (visión ampliada a pedido del usuario:
administrar la BD + CRUD + agrupar grafos + canvas de conexiones).

---

## M8 — Wire protocol Mongo-compatible (completado)

**Idea:** servidor TCP que habla el wire protocol de MongoDB (subset
`OP_MSG` + legado `OP_QUERY`/`OP_REPLY`/`OP_GET_MORE`/`OP_KILL_CURSORS`)
para conectar Navicat, Compass o mongosh sin escribir código Go.

**Paquete `wire/` (10 archivos, 4.293 L prod + 1.030 L test):**

| Archivo | Contenido |
|---|---|
| `bson.go` | Codec BSON↔`Document` con extended-JSON (`$oid`, `$date`, `$binary`, `$numberLong`, `$regex`, min/maxKey); enteros → int32/int64/double preservando el type byte; decode con claves ordenadas |
| `msg.go` | Framing: header 16B, OP_MSG (secciones kind 0/1, checksum CRC32-C, moreToCome), OP_QUERY (`$cmd` + find legado), OP_REPLY, OP_GET_MORE, OP_KILL_CURSORS |
| `cursor.go` | Registro de cursores con TTL 10 min, batch y kill |
| `server.go` | `Server`/`Serve`/`Close`, loop por conexión, dispatch, resolución de comando por primer nombre conocido, mapeo de errores → códigos Mongo |
| `handshake.go` | hello/isMaster/ping/buildInfo/getLog/connectionStatus/whatsmyuri/serverStatus/startSession + listDatabases/listCollections/create/drop/createIndexes/listIndexes/dropIndexes/dbStats/collStats |
| `commands.go` | insert/find/getMore/killCursors/count/countDocuments/estimatedDocumentCount/distinct/update/delete/findAndModify; proyecciones include/exclude; batching con tope 16MB |
| `query.go` | Traducción filtro Mongo→motor (regex BSON→`$regex`, normalización `_id` ObjectId/número→string, operadores no soportados → BadValue) |
| `update.go` | Operadores `$set $unset $inc $mul $min $max $rename $push $addToSet $pop $pull $pullAll $currentDate $setOnInsert` + reemplazo → diff patch/remove |
| `agg.go` | aggregate: `$match $sort $skip $limit $count $group $unwind $project $lookup` (estado por grupo, accumuladores $sum/$avg/$min/$max/$count/$first/$last/$push/$addToSet) |
| `scram.go` | SCRAM-SHA-256 server (PBKDF2 15000, verificación de prueba, v= signature, skipEmptyExchange) → sesión engine si RBAC activo |

**Adiciones al motor (M8):**

- `Store.UpdateFields`/`Session.UpdateFields` (patch + remove[] explícito;
  los before-hooks ven la forma final) — base de `$unset`/replace del wire.
- `Store.CreateCollection`/`DropCollection` (+ Session): create persiste
  en META aunque vacía; drop pasa cada doc por el delete path normal y
  **el scan v2 queda META-gated** (DOC/IDX de colecciones ausentes del
  último META se ignoran → nada resucita al reabrir).
- Session admin: `EnsureIndex`/`DropIndex`/`ListIndexes`/`Collections`
  con chequeos RBAC correspondientes.
- Nuevo sentinel `ErrExists` (→ code 48 NamespaceExists).

**Herramientas:**

- `tools/mls-server`: CLI (`-addr -path -key -db -user -pass -machine
  -light-kdf`) con shutdown por señal; expone **una** base de datos con
  todas las colecciones del store.
- `scripts/test-race.ps1` ahora cubre `./db/ ./wire/`.

**Manual de usuario (`doc/manual/`):** 9 temas × 2 idiomas (ES/EN) en
`.md` + `manual.html` interactivo con pestañas y selector de idioma —
conexión, insert, select, update, delete, **joins (4 patrones para JSON
anidado entre colecciones)**, triggers, grafos y servidor Mongo.

**Tests:** `wire/wire_test.go` (28): BSON roundtrip + number encoding ·
handshake OP_MSG y legado · insert/find roundtrip (ObjectId→hex) ·
filtros/sort/skip/limit · cursores (batch 101/getMore/kill) ·
count/distinct · update operators ($set/$unset/$inc/$push/$pull) +
replace + upsert/multi · delete/findAndModify · aggregate ($group/$unwind/
$lookup) · índices admin (unique 11000) · create/drop/listCollections ·
stats · errores (CommandNotFound/writeErrors ordered-unordered) ·
proyecciones · find legado + OP_GET_MORE · secuencias kind-1 · checksum
OP_MSG · SCRAM (ok + password incorrecto) · RBAC engine sin wire-auth ·
edges por wire. Motor: `db/colladmin_test.go` (6): UpdateFields (+hooks),
CreateCollection, DropCollection, **persistencia de drop sin resurrección**,
Session admin.

**Limitaciones documentadas:** `_id` no-string se normaliza a string;
números BSON con semántica JSON (5 == 5.0); sort multi-clave en orden
alfabético de campos; sin `$elemMatch/$size/$type/$mod/$all/$expr`, sin
proyecciones punteadas, sin compresión OP_COMPRESSED; sin pipeline
updates; `maxBsonObjectSize` = 1 MB.

**Gates M8:**

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./... -count=1` → **201 PASS / 0 FAIL** ✅
- `go run ./tools/smoke` → `smoke OK: Ana` ✅
- `go run ./tools/loadtest` → PASS end-to-end ✅
- `powershell -File scripts/test-race.ps1` → **ok (db + wire)** ✅
- e2e `mls-server` por TCP real (hello + ping) ✅
