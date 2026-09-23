# INFORME DE PROYECTO — MLStoreDB

**Fecha:** 23 de septiembre de 2026
**Módulo Go:** `mlstoredb` · **Paquete principal:** `db` · **Go:** 1.26.5
**Estado del proyecto:** Funcionalmente completo (M0–M7 entregados; M8 pendiente)

---

## 1. Resumen ejecutivo

MLStoreDB es una **base de datos documental embebida, cifrada y sin servidor**, escrita en Go puro (sin CGO en producción). Almacena documentos JSON (`map[string]any` con `_id` obligatorio) en un único archivo `.mlstore` protegido con AES-256-GCM, con derivación de claves Argon2id.

El motor ofrece CRUD estilo MongoDB, índices hash + ordenados con matriz CSR, caché de páginas con segundo intento (second-chance), RBAC con sesiones, hooks síncronos/asíncronos, triggers JSON persistentes, capa de grafos, consultas con operadores Mongo, exportación CSV con redacción de campos sensibles, migraciones de esquema, snapshots, compactación, rotación de claves y reparación semántica.

**Verificación del día de la fecha:**

| Verificación | Resultado |
|---|---|
| `go test ./db -count=1` | `ok mlstoredb/db 10.8s` |
| Tests con `-v` | **173 PASS, 0 FAIL** (incluye subtests) |
| `go vet ./...` | Limpio, sin hallazgos |

---

## 2. Objetivo y alcance

**Objetivo:** extraer del proyecto de origen un motor de almacenamiento standalone, independiente del dominio original, con cifrado fuerte y API de base de datos documental en proceso.

**Dentro del alcance:**
- Documentos JSON con `_id` string (ULID automático si falta)
- Un solo archivo cifrado por base de datos, formato v2 (log de registros append-only)
- Modelo en memoria: RAM como fuente de verdad + caché de páginas para documentos fríos
- Concurrencia segura dentro de un proceso (RWMutex + flock + backpressure de escritura)

**Fuera del alcance (decisiones de diseño D1–D22):**
- SQL, transacciones multi-colección, joins, pipelines de agregación
- Protocolo de red / servidor (M8 pendiente)
- Escritura multi-proceso, WAL, ORM

---

## 3. Arquitectura

```
┌─────────────────────────────────────────────────────────────┐
│ API pública (exportada)                                      │
│ Open/Close/Flush/Snapshot/Compact/RotateKeys/Repair         │
│ Insert/Upsert/Update/Delete/Get/Find/Count/Explain          │
│ EnsureIndex/DropIndex · ExportCSV/Migrations                │
│ Authenticate→Session (RBAC) · On/OnAsync (hooks) · Triggers │
│ AddEdge/Neighbors/Traverse/ShortestPath (grafos)            │
└───────────────────────────┬─────────────────────────────────┘
                            │ sync.RWMutex (s.mu)
┌───────────────────────────▼─────────────────────────────────┐
│ Store (fuente de verdad en memoria)                          │
│ collections → entries (caché de páginas) → indexes (+CSR)   │
│ Planner · match() · sort · Find paralelo · hooks/triggers   │
│ RBAC (_users/_roles) · grafos (edges.* CSR)                 │
└───────────────────────────┬─────────────────────────────────┘
                            │ prepareFlush (RLock breve)
┌───────────────────────────▼─────────────────────────────────┐
│ Persistencia (serializada por flushMu, fuera de s.mu)        │
│ v2: log DOC/META/IDX/DEL/COMMIT + CRC32-C                    │
│ → zlib (si conviene) → AES-256-GCM por registro             │
│ Cabecera 152B (magic MLDB, Argon2, DEK envuelto, HMAC)      │
│ Reemplazo atómico · append incremental · Compact()          │
└──────────────────────────────────────────────────────────────┘
```

### Modelo de concurrencia

| Mecanismo | Rol |
|---|---|
| `s.mu sync.RWMutex` | Lectores concurrentes, un escritor |
| `flushMu sync.Mutex` | Serializa I/O sin bloquear `s.mu` |
| `dirtyGen uint64` | Detecta mutaciones durante el snapshot de flush |
| `flock` + `mlstoredb.lock` | Un solo proceso dueño → `ErrAlreadyOpen` |
| `bpCond *sync.Cond` | Backpressure: bloquea escritores si `writeSeq - durableSeq ≥ MaxPendingWrites` |
| `regexCache sync.Map` | `$regex` compilado una vez por patrón |
| `graphMu sync.Mutex` | Reconstrucción CSR de grafos (orden: graphMu → s.mu) |

**Flush no bloqueante:** `prepareFlush` toma un lock breve → `writeState` corre **sin** `s.mu`. Los lectores nunca se congelan durante escrituras a disco.

---

## 4. Componentes principales

| Archivo | Líneas aprox. | Responsabilidad |
|---|---:|---|
| `store.go` | 1.419 | Store central: CRUD, Find/Count/planner, sort, clone, backpressure |
| `filev2.go` | 1.017 | Formato v2: log de registros, CRC, escaneo, append/compact |
| `index.go` | 854 | Índices hash+ordenados, seek de rango/prefijo, matriz CSR |
| `file.go` | 670 | Options, cabecera 152B, criptografía (KEK/DEK), ciclo de vida |
| `auth.go` | 572 | RBAC: Permission/Session, Argon2id PHC, FieldDeny |
| `graph.go` | 464 | `edges.<tipo>`, CSR, Neighbors/Traverse/ShortestPath (BFS) |
| `triggers.go` | 458 | Triggers JSON persistentes (`_triggers`), plantillas `$get` |
| `filter.go` | 369 | Lenguaje de consultas: 13 operadores + rutas con punto |
| `hooks.go` | 302 | Hooks before/after, FIFO asíncrono acotado (1024), profundidad ≤ 8 |
| `repair.go` | 269 | Reparación semántica (RepairReport), descifrado tolerante |
| `export.go` | 223 | CSV (BOM + RFC 4180), lista negra de sensibles, migraciones |
| `runtime.go` | 95 | flock, Snapshot, ticker de auto-flush (2s), OpenWithLock |
| `cache.go` | 184 | Caché de páginas second-chance, docEntry atómico, evicción 2 fases |
| `rotate.go` | 135 | Rotación de claves O(1) — solo re-envuelve cabecera |
| `machineid.go` | 140 | ULID por instalación (`mlstoredb.machineid`) |
| `parallel.go` | 85 | Find/Count paralelo (umbral 512 ids, workers sin locks) |
| `filter_regex.go` | 49 | Caché de regex |
| Otros | ~100 | `errors.go`, `zlib_helpers.go`, `machine_windows/nonwindows.go` |

**Total producción:** ~7.400 líneas en 21 archivos · **Tests:** 19 archivos, ~5.100 líneas.

---

## 5. Funcionalidades por hito

| Hito | Funcionalidad | Estado |
|---|---|---|
| M0 | Núcleo: CRUD, cifrado, persistencia v1 | ✅ Completado |
| M1 | Consultas: operadores, planner, sort/limit/skip, Explain | ✅ Completado |
| M2 | Índices: hash + ordenados, seek de rango, matriz CSR persistida | ✅ Completado |
| M3 | Rendimiento: caché de páginas, Find paralelo, backpressure | ✅ Completado |
| M4 | RBAC: usuarios/roles/sesiones, FieldDeny, colecciones sistema | ✅ Completado |
| M5a | Hooks Go: before/after, síncronos/asíncronos, veto, ciclos | ✅ Completado |
| M5b | Triggers JSON: acciones set/unset/insert/update/upsert/delete | ✅ Completado |
| M6 | Grafos: aristas tipadas, Neighbors/Traverse/ShortestPath | ✅ Completado |
| M7 | Robustez: Repair, RotateKeys, Snapshot, Compact, dirtyGen | ✅ Completado |
| M8 | Protocolo de red (wire protocol Mongo) | ⏳ Pendiente |

---

## 6. Seguridad

| Capa | Mecanismo |
|---|---|
| Derivación de claves | KEK = Argon2id(SHA256(master ‖ 0x00 ‖ machineID)) |
| Cifrado de datos | DEK por archivo, AES-256-GCM por registro |
| Integridad | HMAC-SHA256 de cabecera + CRC32-Castagnoli por registro |
| Contraseñas | Hash Argon2id formato PHC (`$argon2id$v=19$m=..,t=..,p=..$`) |
| Tokens de sesión | 32 bytes aleatorios (crypto/rand), base64url, TTL |
| Redacción | `ExportCSV` omite lista negra global (13 patrones: `password`, `api_key`, `token`, …) + sensibles por colección + FieldDeny de RBAC |
| Rotación | `RotateKeys` re-envuelve solo la cabecera (O(1)); clave incorrecta → `ErrCorrupt` sin tocar el archivo |
| Bind a instalación | MachineGuid (Windows) / ULID por instalación (`mlstoredb.machineid`) |

**Colecciones sistema protegidas:** `_users`, `_roles`, `_triggers` — ocultas de `Collections()`, CRUD genérico → `ErrForbidden`.

---

## 7. Formato de archivo v2

- **Cabecera:** 152 bytes en claro: magic `MLDB`, versión de formato, flags, versión de esquema, timestamps, salt Argon2 (16B), parámetros KDF, nonce base (12B), DEK envuelto (48B), HMAC (32B).
- **Registros** (cabecera 24B): `tipo u8 | flags u8 | idLen u16 | payloadLen u32 | nonce[12] | crc32` + id (colección+docID en claro) + payload cifrado (zlib si conviene).
- **Tipos:** DOC=1, META=2, IDX=3, DEL=4, COMMIT=5.
- **Commit atómico:** orden DEL → DOC → META → IDX → COMMIT; la carga solo aplica hasta el último COMMIT (colas truncadas por escritura parcial se descartan).
- **Compatibilidad:** archivos v1 (JSON plano) se migran automáticamente al abrir.

---

## 8. Consultas

**Operadores soportados:** `$eq $ne $gt $gte $lt $lte $in $nin $regex $exists $and $or $not $nor` + igualdad implícita, rutas con punto (`a.b.c`) y expansión de arrays.

**Planner:** igualdad → seek hash; prefijo → seek de prefijo; rango → seek binario en banda de tipo. Orden total: `nil < números < strings < bool < otros` (las rangos nunca cruzan tipos, estilo Mongo).

**No soportados:** `$elemMatch`, `$where`, `$expr`, `$size`, `$type`, `$mod`, `$all`, `$slice` (prefijo `$` desconocido → `ErrBadFilter`).

---

## 9. Calidad y verificación

### 9.1 Resultados (23/09/2026)

- `go test ./db -count=1` → **ok** (10.8s)
- 173 verificaciones PASS / 0 FAIL (conteo `-v`, incluye subtests)
- `go vet ./...` → limpio
- `-race` → verde vía `scripts/test-race.ps1` (CGO + gcc MSYS2)

### 9.2 Cobertura por área

| Archivo de test | Tests | Área |
|---|---:|---|
| `store_test.go` | 13 | CRUD, sort/limit/skip, aislamiento del llamador |
| `filter_test.go` | 10 | Matriz de operadores, rutas, lógicos |
| `index_test.go` | 14 | Únicos, compuestos, multi-valor, planner |
| `index_matrix_test.go` | 5 | CSR: serialización, roundtrip, checksum |
| `file_test.go` | 7 | Roundtrip, corrupción, clave errónea |
| `filev2_test.go` | 7 | v2, caché, cola truncada, Compact, migración v1→v2 |
| `repair_test.go` | 9 | FlushSync, SyncOnWrite, Repair |
| `runtime_test.go` | 8 | Snapshot, flock, auto-flush |
| `export_test.go` | 7 | CSV BOM, migraciones, sensibles |
| `hardening_test.go` | 16 | Seek de rango, dirtyGen, deep clone, regex |
| `machineid_test.go` | 7 | ResolveMachineID |
| `rotate_test.go` | 10 | Rotación de claves |
| `options_test.go` | 4 | Defaults de Options |
| `parallel_test.go` | 4 | Find/Count paralelo, backpressure, COW |
| `auth_test.go` | 11 | RBAC, sesiones, FieldDeny |
| `hooks_test.go` | 13 | Before/after, FIFO, profundidad/ciclos |
| `triggers_test.go` | 11 | Acciones, plantillas, reapertura |
| `graph_test.go` | 8 | Vecinos/recorrido/ruta, invalidación, RBAC |
| `bench_test.go` | 3+11 | Límite 1MB + benchmarks |

### 9.3 Comandos

```bash
go build ./...                          # compilar
go test ./db -count=1                   # suite completa (~11s)
go vet ./...                            # análisis estático
go test ./db -bench=. -run=XXX          # benchmarks
go run ./tools/smoke                    # prueba de humo
go run ./tools/loadtest                 # prueba de carga 10k docs
powershell -File scripts/test-race.ps1  # con -race
```

---

## 10. Dependencias

Solo 4 dependencias externas, todas justificadas:

| Dependencia | Versión | Propósito |
|---|---|---|
| `github.com/gofrs/flock` | v0.13.1 | Lock de proceso exclusivo |
| `github.com/oklog/ulid/v2` | v2.1.2 | `_id` e IDs de instalación |
| `golang.org/x/crypto` | v0.57.0 | Argon2id (KDF + passwords) |
| `golang.org/x/sys` | v0.48.0 | Registro de Windows (MachineGuid) |

Todo lo demás es stdlib. Go puro, portable a windows/amd64, linux y darwin.

---

## 11. Limitaciones y trabajo pendiente

1. **M8 — Protocolo de red:** único hito del plan original pendiente (wire protocol compatible Mongo).
2. **M9 — Cliente web de visualización de grafos:** estilo Neo4j Browser; permitiría explorar `edges.*`, `Traverse` y `ShortestPath` desde el navegador (registrado 2026-09-23, depende de M8 o de un servidor HTTP propio).
3. **Sin transacciones multi-colección** ni WAL: por diseño (D1–D22), no es un bug.
4. **Sin README en la raíz** del repositorio: la documentación vive íntegramente en `doc/`.
5. **Sin automatización de build** (Makefile/CI/Dockerfile): solo comandos estándar de Go.
6. **Escritura mono-proceso:** el flock impide dos procesos escribiendo; multi-proceso queda fuera de alcance.
7. **Documentos ≤ 1MB** (`ErrTooLarge`): límite deliberado del diseño.

---

## 12. Conclusión

MLStoreDB es un motor **completo, probado y listo para integrar**. Los 8 bloques funcionales entregados (núcleo, consultas, índices, rendimiento, RBAC, hooks/triggers, grafos, robustez) superan sus 16 grupos de verificación de diseño con 173 tests en verde, análisis estático limpio y suite race-clean. La arquitectura en capas con flush no bloqueante, el cifrado por registro con jerarquía KEK/DEK y la persistencia CSR de índices constituyen una base sólida para el trabajo restante del plan: M8 (protocolo de red) y M9 (cliente web de grafos).

---

## Anexo — Documentación relacionada

| Documento | Contenido |
|---|---|
| `doc/README.md` | Visión general y estado de hitos |
| `doc/ARQUITECTURA.md` | Arquitectura + 22 decisiones de diseño (D1–D22) |
| `doc/API.md` | Referencia completa de la API pública |
| `doc/CONSULTAS.md` | Lenguaje de consultas y planner |
| `doc/FORMATO_ARCHIVO.md` | Formato binario y criptografía |
| `doc/PRUEBAS.md` | Suites, matriz de cobertura, referencia de rendimiento |
| `doc/DISTRIBUCION_ARCHIVOS.md` | Mapa de archivos y capas |
| `doc/RECREAR.md` | Guía de extracción paso a paso |
| `doc/PLAN_V2.md` | Bitácora del plan v2 (M0→M8) |
