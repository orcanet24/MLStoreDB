# Pruebas y benchmarks

## 1. Cómo correrlas

```bash
# suite completa (gate)
go test ./... -count=1

# verbose
go test ./db ./wire -count=1 -v

# solo un archivo/área
go test ./db -count=1 -run 'TestRange|TestCompound'
go test ./wire -count=1 -run 'TestSCRAM|TestCursor'

# benchmarks (1 iteración rápida)
go test ./db -bench=. -benchtime=1x -run=XXX

# benchmarks estables (varias corridas)
go test ./db -bench='Insert|Get|Find' -benchtime=2s -run=XXX -count=3

# estático
go vet ./...
go build ./...

# race detector (requiere CGO + gcc; ver scripts/test-race.ps1)
powershell -File scripts/test-race.ps1

# carga end-to-end con disco (10k docs)
go run ./tools/loadtest

# smoke mínimo
go run ./tools/smoke

# servidor wire e2e (arrancar y conectar con mongosh/Compass)
go run ./tools/mls-server -addr 127.0.0.1:28917 -db demo
```

**Resultado actual de referencia:** `go test ./... -count=1` → **ok** ·
**201 PASS / 0 FAIL** (motor 173 + wire 28) · `go vet` limpio ·
`scripts/test-race.ps1` → **race verde (db + wire)**.

> Nota: `-race` requiere `CGO_ENABLED=1` + toolchain C. En este entorno hay gcc (MSYS2, `C:\msys64\mingw64\bin\gcc.exe`); usar `powershell -File scripts/test-race.ps1` que setea `CGO_ENABLED=1` y resuelve el PATH automáticamente.

---

## 2. Suites por archivo

### 2.1 `store_test.go` — CRUD y Find base (13 tests)

| Test | Cubre |
|---|---|
| TestInsertAndGet | roundtrip básico |
| TestInsertRequiresID | auto ULID; `_id` no string → `ErrNoID` |
| TestInsertDuplicate | `_id` repetido |
| TestGetNotFound | coll y doc missing |
| TestUpsertCreatesAndReplaces | replace total (no merge) + `_id` del arg |
| TestUpdateShallowMerge | merge top-level; no muta `_id`; missing → ErrNotFound |
| TestDelete | delete + segundo delete |
| TestFindAllAndEquality | nil filter orden `_id`; eq simple y compuesta |
| TestFindNumericEquality | int/float cross |
| TestFindSkipLimitSort | sort asc/desc, limit, skip |
| TestCountAndCollections | Count + Collections ordenadas |
| TestCallerCannotMutateStoredDoc | Insert copia |
| TestSchemaVersion | 0 → migración 1 |

### 2.2 `filter_test.go` — matriz de operadores (10 tests)

| Test | Cubre |
|---|---|
| TestFilterComparisonOps | `$gt $gte $lt $lte $ne $eq` |
| TestFilterInNin | `$in` / `$nin` |
| TestFilterExists | `$exists` true/false |
| TestFilterRegex | `$regex` + `$options i` |
| TestFilterLogicalAndOrNot | `$and $or $not` |
| TestFilterDottedPath | `from.name` |
| TestFilterDottedPathThroughArray | `items.item_id` |
| TestFilterBadOperator | `$bogus` → ErrBadFilter |
| TestFilterCombinedWithSortLimit | filtro + sort + limit juntos |
| TestFilterEqualityStillWorks | igualdad implícita |

### 2.3 `index_test.go` — índices (14 tests)

| Test | Cubre |
|---|---|
| TestEnsureIndexAndList | ensure idempotente + ListIndexes |
| TestUniqueIndexBlocksDuplicate | unique en insert |
| TestUniqueIndexOnExistingDocs | build sobre docs en conflicto |
| TestUniqueIndexOnUpdate | conflicto + rollback |
| TestUniqueIndexOnUpsert | conflicto upsert; mismo doc ok |
| TestIndexMaintainedOnDelete | delete libera unique |
| TestDropIndex | drop + drop missing |
| TestCompoundUniqueIndex | unique de tupla |
| TestFindUsesIndexEquality | eq con índice |
| TestFindIndexWithIn | `$in` con índice |
| TestFindIndexIntersectsEqualities | intersección de dos índices |
| TestProjection | projection top-level |
| TestNullAndMissingIndexSameSentinel | sentinel `u:` unique clash + eq null |
| TestMultiValueArrayIndex | arrays multi-valor |

### 2.4 `file_test.go` — crypto y persistencia (7 tests)

| Test | Cubre |
|---|---|
| TestFlushOpenRoundtrip | flush → magic no-JSON → reopen → find + índices |
| TestOpenWrongKeyCorrupt | clave mala → ErrCorrupt |
| TestOpenTamperedHeader | flip byte header → ErrCorrupt |
| TestOpenTamperedPayload | flip byte payload → ErrCorrupt |
| TestFlushNoopWhenClean | flush inicial crea archivo |
| TestOpenMissingMasterKey | sin MasterKey → error |
| TestCreatedThenReopenSameContent | nested `map[string]any` preservado + unique |

### 2.4b `repair_test.go` — hardening H6 (9 tests)

| Test | Cubre |
|---|---|
| TestFlushSyncPersistsImmediately | FlushSync → clean + reopen sin Close |
| TestSyncOnWriteAutoPersists | SyncOnWrite: mutador deja archivo durable |
| TestSyncOnWriteFailedMutationDoesNotFlush | mutación fallida no dispara flush |
| TestCorruptMainSnapshotRecovers | payload corrupto → ErrCorrupt; Snapshot abre; Repair no puede (GCM) |
| TestTruncatedFileErrCorrupt | truncado → Open y Repair → ErrCorrupt |
| TestRepairInvalidIDType | `_id` no string dropeado; report DocsDropped/Kept/Resaved |
| TestRepairUniqueConflict | unique conflict dropea el mayor `_id`; IndexesRebuilt |
| TestRepairMissingFile | archivo ausente → ErrNotFound |
| TestRepairRequiresMasterKey | sin MasterKey → error |

### 2.5 `runtime_test.go` — snapshot, lock y auto-flush (8 tests)

| Test | Cubre |
|---|---|
| TestSnapshotCreatesIndependentBackup | backup aislado del main |
| TestSnapshotRequiresMasterKey | sin key → error |
| TestFileLockBlocksSecondOpen | ErrAlreadyOpen + release |
| TestAutoFlushPersistsWithinInterval | persiste sin Close/Flush manual |
| TestCloseReleasesLock | reabre después de Close |
| TestAutoFlushStopsOnClose | ticker muerto post-Close |
| TestSnapshotEmptyDest | dest vacío → error |
| TestLockFileCreated | existe `mldstore.lock` |

### 2.6 `export_test.go` — CSV y migraciones (7 tests)

| Test | Cubre |
|---|---|
| TestExportCSVBOMAndColumns | BOM, columnas, no filtra `refresh_token` |
| TestExportCSVNestedJSON | anidado → JSON en celda |
| TestExportCSVMissingColl | coll missing → header `_id` |
| TestApplyMigrationsForward | v1→v2, re-apply no-op |
| TestApplyMigrationsFutureRejected | store v5 vs mig v2 → "update the program" |
| TestApplyMigrationsErrorPropagates | error Up no avanza versión |
| TestMigrationsPersistAcrossReopen | schema + índices tras reopen |

### 2.6b `hardening_test.go` — hardening H1–H5 (16 tests)

| Test | Cubre |
|---|---|
| TestRangeSeekIndexedMatchesFullScan | 6 filtros range: índice == baseline |
| TestRangeSeekOrderAscDesc | sort asc/desc por rango indexado |
| TestRangeCrossTypeExcluded | `$gt` numérico excluye string/missing |
| TestStringRangeIndexed | rango de strings |
| TestSortByIndexMatchesSortWithoutIndex | sort por índice == sort normal |
| TestCompoundPrefixEquality | prefijo + full + Explain exact |
| TestCompoundLeadingRange | leading equality + range residual + ReMatch |
| TestExplainCollScanWhenNoIndex | COLLSCAN |
| TestFlushDoesNotLoseConcurrentMutation | dirtyGen en ventana de flush |
| TestFindDuringFlushDoesNotError | Find+Insert concurrentes con Flush |
| TestDeepCloneNestedMutation | arrays/maps anidados aislados |
| TestUpdatePatchDeepCopy | patch copiado en profundidad |
| TestProjectionDeepCopy | proyección copia arrays |
| TestRegexCacheRepeated | cache hit + error cacheado |
| TestSensitiveFieldsPerCollection | sensitive por coll en CSV |
| TestSensitiveFieldsPersist | sensitive tras reopen |

### 2.7 Suites v2 (M0–M6)

| Archivo | Tests | Cubre |
|---|---:|---|
| filev2_test.go | 7 | formato v2, cold+eviction, torn-tail, Compact, migración v1→v2, delete no-resurrect |
| index_matrix_test.go | 5 | matriz CSR: serialize, checksum fallback, roundtrip, invalidación |
| parallel_test.go | 4 | Find/Count paralelos (`ParallelFind*`), backpressure, Update COW |
| machineid_test.go | 7 | ResolveMachineID por instalación |
| rotate_test.go | 10 | RotateKeys (master/machine/no-op/dirty/tamper) |
| options_test.go | 4 | Options defaults, lock custom en Repair, auto-flush custom |
| auth_test.go | 11 | **M4 RBAC**: Authenticate, sesiones, fieldDeny, system colls, roles |
| hooks_test.go | 13 | **M5a hooks**: before/after, veto, async FIFO/backpressure, depth/ciclo, bypass |
| triggers_test.go | 11 | **M5b triggers**: set/unset/insert/upsert/delete actions, `$get`, reopen, async |
| graph_test.go | 8 | **M6 grafo**: Neighbors/Traverse/ShortestPath, invalidación cache, RBAC |

### 2.8 `bench_test.go` — límites y benchmarks (3 tests + 11 benchmarks)

| Benchmark / Test | Mide |
|---|---|
| TestDocSizeLimit1MB | doc >1MB → ErrTooLarge; justo bajo → ok |
| TestUpsertSizeLimit | límite en Upsert |
| TestUpdateSizeLimitRollback | update gigante no deja basura |
| BenchmarkInsert10k | insert payload "real" |
| BenchmarkFindFullScan10k | sin índice |
| BenchmarkFindIndexed10k | con índice |
| BenchmarkFlush10k | flush cifrado 10k |
| BenchmarkExportCSV10k | CSV 10k |
| BenchmarkInsertComplex | JSON anidado ~2–4KB |
| BenchmarkGetComplexJSON | **lectura** deep-clone de JSON complejo |
| BenchmarkFindComplexPage20 | página 20 con índice+sort+limit |
| BenchmarkFindComplexProjection | full-scan + projection |
| BenchmarkCountComplexNested | count path `payload.shipping.mode` |
| BenchmarkReopenComplex10k | descifra + unmarshal + rebuild 10k complejos |

---

## 3. Matriz de cobertura (design §9 → estado)

| Grupo de gates | Estado | Dónde |
|---|---|---|
| 1. Unit: filtros, sort, projection, unique, update | ✅ | store/filter/index |
| 2. Roundtrip persistencia (golden RAM↔archivo) | ✅ | file/runtime |
| 3. Crypto: wrong key, tamper header/payload | ✅ | file_test |
| 4. Crash / dirtyGen / auto-flush | ✅ | hardening + runtime |
| 5. Migración forward + versión futura | ✅ | export_test |
| 6. Windows atomic replace | ✅ (local win32) | file.go atomicReplace |
| 7. Benchmarks NF2/NF3 | ✅ | bench + loadtest |
| 8. CSV BOM/quotes/nested/sensitive | ✅ | export + hardening |
| 9. Durable + repair + race (H6) | ✅ | repair_test + test-race.ps1 |
| 10. Formato v2 + page cache (M1) | ✅ | filev2_test |
| 11. index_matrix CSR persistida (M2) | ✅ | index_matrix_test |
| 12. Find paralelo + backpressure + COW (M3) | ✅ | parallel_test + store_test |
| 13. RBAC / sesiones / fieldDeny (M4) | ✅ | auth_test |
| 14. Hooks before/after + async (M5a) | ✅ | hooks_test |
| 15. Triggers JSON declarativos (M5b) | ✅ | triggers_test |
| 16. Grafo edges/Traverse/Path (M6) | ✅ | graph_test |

---

## 4. Referencia de rendimiento

### 4.1 Carga end-to-end 10k (`go run ./tools/loadtest`)

Máquina de referencia: **Intel i5-2430M**, Go 1.26.5, Windows. Última corrida medida (plan v2, formato + page cache):

| Paso | Total | Por op | Notas |
|---|---:|---:|---|
| OpenWithLock | 1.04 ms | — | flock + auto-flush 2s |
| **Insert 10k** | 311 ms | **31 µs** | heap 0.2 → 23.6 MB |
| **Get por `_id` ×10k** | 51 ms | **5.1 µs** | aleatorio |
| Find full-scan `buyer_id` | 28 ms | 140 µs | sin índice |
| Find **con índice** `buyer_id` | 17 ms | 83 µs | **1.7×** vs scan, 20 hits |
| Find unique `order_id` | 3 ms | 15 µs | índice unique |
| Find rango+sort+limit 20 | 2257 ms | 22.6 ms | full-scan de `total` (sin idx) |
| **Update 10k** | 3132 ms | **313 µs** | merge shallow |
| Count con filtro | 1 ms | 5 µs | O(1) con índice |
| Flush (AES+zlib+atomic) | 9949 ms | — | dirty forzado (v2) |
| Close+Open (reopen) | 419 ms | — | 10k docs intactos (v2) |
| Snapshot | 5371 ms | — | 3.13 MB backup cifrado |
| Archivo en disco | 6.15 MB | — | magic `MLDB` |

Resumen: insert/lectura/edición estables a 10k; unique y count usan índice; flush/reopen incluyen crypto real.

### 4.2 Benchmarks Go (`go test ./db -bench=. -benchtime=1x -run=XXX`)

11 benchmarks en `bench_test.go`. Corrida de referencia `benchtime=1x`
(i5-2430M, corrida única — órdenes de magnitud; para números estables usar
`-benchtime=2s -count=3`):

| Benchmark | ns/op (1x) | Qué mide |
|---|---:|---|
| **Insert10k** | ~0.68 ms | insert payload moderado |
| FindFullScan10k | ~5.0 ms | Find sin índice (10k) |
| FindIndexed10k | ~5.2 ms | Find con índice (misma cardinalidad alta) |
| **InsertComplex** | ~0.22 ms | JSON anidado ~2–4 KB |
| **GetComplexJSON** | ~24 µs | deep-clone de JSON complejo por `_id` |
| FindComplexPage20 | ~2.5 ms | página 20: índice + sort + limit |
| FindComplexProjection | ~7.7 ms | full-scan 10k + projection de 50 |
| CountComplexNested | ~3.3 ms | path `payload.shipping.mode` sobre 5k |
| **Flush10k** | ~4.2 s | log v2 + AES + zlib + atomic |
| ExportCSV10k | ~122 ms | BOM + columnas top-level |
| **ReopenComplex10k** | ~102 ms | Argon2 LightKDF + scan v2 + rebuild |

Comando:

```bash
go test ./db -bench=. -benchtime=2s -run=XXX -count=3
```

### 4.3 Disco (resumen)

| Operación | Tiempo |
|---|---:|
| Flush 10k (AES+zlib+rename) | ~4 s (1x) / ver loadtest |
| Reopen 10k complejos | ~100 ms LightKDF (v2); ~0.8–2.5 s KDF prod (v1) |
| Archivo 10k payload CRM | ~6 MB en loadtest |

### 4.4 Lectura de resultados (cómo leer los números)

- **Get ~24µs** = recuperación de un JSON complejo completo (deep clone) → decenas de miles de lecturas/seg.
- **Insert complejo ~0.2ms** → miles de inserts/seg suficientes para sync.
- **Reopen** en v2 es mucho más rápido que el snapshot JSON de v1 (scan incremental + LightKDF en tests).
- **Find con projection caro** si hace full-scan; siempre indexar el campo de filtro y usar `Explain` para verificar `IXSCAN`.

---

## 5. Qué agregar al recrear el módulo

Checklist mínimo de puerta de calidad:

- [ ] `go test ./... -count=1` en verde
- [ ] `go vet ./...` limpio
- [ ] Test de corrupt (header + payload)
- [ ] Test de wrong key
- [ ] Test de unique rollback en Update
- [ ] Test indexed == full-scan para rangos
- [ ] Test deep clone (mutación de resultado)
- [ ] Test dirtyGen / mutación durante flush
- [ ] Test flock (segundo open)
- [ ] Test migración forward + versión futura
- [ ] Test CSV BOM + sensitive
- [ ] Benchmark Insert / Get / Flush / Reopen grabado como referencia
- [ ] Smoke binario (open → index → insert → find → close)
- [ ] Test corrupt → Snapshot recovery (`TestCorruptMainSnapshotRecovers`)
- [ ] Test truncado → `ErrCorrupt`
- [ ] Test `Repair` (id inválido + unique conflict)
- [ ] Test `FlushSync` / `SyncOnWrite`
- [ ] `powershell -File scripts/test-race.ps1` en verde (`-race`, CGO+gcc)

Los `tools/loadtest` y `tools/smoke` de este repo son plantillas listas para copiar.
