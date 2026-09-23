# Arquitectura del motor

## 1. Vista general

```
┌─────────────────────────────────────────────────────────────────────┐
│                            API pública                              │
│  Open / OpenWithLock / Close / Flush / FlushSync / Snapshot / Compact│
│  Insert / Upsert / Update / Delete / Get / Find / Count / Explain   │
│  EnsureIndex / DropIndex / ListIndexes / ExportCSV / Migrations     │
│  Authenticate → Session (RBAC) · On/OnAsync (hooks) · Triggers      │
│  AddEdge / Neighbors / Traverse / ShortestPath (grafo)              │
└───────────────────────────┬─────────────────────────────────────────┘
                            │ sync.RWMutex (s.mu)
┌───────────────────────────▼─────────────────────────────────────────┐
│  Store (fuente de verdad EN RAM)                                    │
│  collections: map[string]*collection                                │
│    collection.entries    map[_id]*docEntry  (page cache second-chance)│
│    collection.indexes    []*index  (+ index_matrix CSR persistida)  │
│    collection.sensitive  []string   (ocultos en CSV)                │
│                                                                     │
│  Planner: planCandidates → equality / prefix / range seek           │
│  Match:   match() filtros + deep clone solo de la página            │
│  Sort:    por índice (short-circuit) | heap parcial | full          │
│  Find:    materializeRefs paralelo (FindWorkers) + backpressure     │
│  Hooks:   before/after por evento (M5a) · Triggers JSON (M5b)       │
│  Auth:    _users/_roles + Session (M4) · Grafo CSR edges.* (M6)     │
└───────────────────────────┬─────────────────────────────────────────┘
                            │ prepareFlush (RLock breve)
┌───────────────────────────▼─────────────────────────────────────────┐
│  Persistencia (fuera de s.mu, serializado por flushMu)              │
│  Formato v2: log de records DOC/META/IDX/DEL/COMMIT (CRC) cifrados  │
│  → zlib (si gana) → AES-256-GCM                                     │
│  header claro 152B (magic MLDB, Argon2, DEK wrapped, HMAC)          │
│  atomicReplace / append incremental · Compact() reescribe el log    │
│  Auto-flush ticker 2s si dirty; dirtyGen anti-pérdida               │
│  FlushSync / SyncOnWrite para durabilidad inmediata (H6)            │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 2. Modelo lógico

```
Database (archivo cifrado, formato v2)
 └── Collection  (creada con el primer Insert — sin DDL)
      ├── entries    map[string]*docEntry  // _id → doc residente o frío
      ├── indexes    []*index              // hash + ord (+ index_matrix CSR)
      └── sensitive  []string              // no van al CSV

Colecciones de sistema (lazy, ocultas de Collections()):
  _users · _roles · _triggers

Grafo (M6): colecciones edges.<tipo>
  {_id, _from, _to, ...props} → adyacencia CSR en RAM (out + in)
```

### Reglas de documento

| Regla | Detalle |
|---|---|
| `_id` | string obligatorio; si falta en Insert → auto ULID |
| Tamaño máx | 1 MB (`ErrTooLarge`) — check con `json.Marshal` |
| Tipos JSON | string, float64, bool, nil, []any, Document/map |
| Anidación | libre dentro de valores; campos top-level planos |
| Fechas | el motor NO interpreta fechas; guardás RFC3339 o epoch |
| Null vs missing | misma sentinel de índice `u:`; `$ne` incluye docs sin campo |

### Deep clone (aislamiento)

| Operación | Política |
|---|---|
| `Insert` | deep clone de entrada |
| `Get` / `Find` / `project` | deep clone de salida (solo la página) |
| `Update` | deep clone del patch; snapshot `old` shallow top-level |
| `Upsert` | deep clone de entrada |

`map[string]any` **no** se convierte a `Document` al clonar (preserva tipo dinámico).

---

## 3. Ciclo de vida

```
New() ── RAM vacía, schemaVersion=0
  │
Open(path) ── crea DEK o descifra archivo + rebuild índices
  │
OpenWithLock = Open + flock(mldstore.lock) + ticker auto-flush 2s
  │
  ├── mutaciones → markDirty() → dirtyGen++
  │     └── SyncOnWrite=true → flush inmediato tras mutación exitosa
  ├── Find/Get concurrentes con RLock
  ├── Flush cada 2s si dirty (o Close / Flush / FlushSync manual)
  └── Repair(path) si Open falla con docs corruptos semánticamente
  │
Close = para ticker → Flush final → libera flock → closed=true
```

- **Pérdida máxima en crash:** ~2s de mutaciones (sin WAL — decisión de diseño). Para endpoints críticos usar `FlushSync()` o `Options.SyncOnWrite: true` (pérdida = 0 tras la mutación retornar).
- **Fuente de verdad de negocio:** si el sistema es un cache de una API externa, esto es aceptable; si es ledger financiero, hace falta WAL (fuera de alcance actual) — ver D15.

---

## 4. Concurrencia

| Mecanismo | Rol |
|---|---|
| `sync.RWMutex s.mu` | lectores (Find/Get/Count/Explain) concurrentes; escritor único |
| `flushMu sync.Mutex` | serializa `writeState` (I/O) sin retener `s.mu` |
| `dirtyGen uint64` | detecta mutaciones durante el snapshot del flush |
| `afterMutation()` (H6) | si `SyncOnWrite`, dispara flush fuera de `s.mu` tras mutación exitosa |
| `flock` + `mldstore.lock` | un solo proceso dueño del archivo → `ErrAlreadyOpen` |
| `regexCache sync.Map` | `$regex` compilado una vez por patrón |

### Flush no bloqueante (detalle)

1. `prepareFlush`: write-lock breve (params crypto primera vez) → RLock para `toFileData()` (copia docs ordenados).
2. `writeState`: marshal + zlib + AES-GCM + Argon2 KEK + `atomicReplace` — **sin `s.mu`**.
3. Si `dirtyGen` no cambió respecto del snapshot → limpia `dirty`; si cambió, queda dirty para el próximo tick.

Resultado: `Find`/`Get` no se congelan durante la escritura a disco (~cientos de ms).

---

## 5. Índices (estructura interna)

Cada `*index` tiene dos estructuras:

```
entries map[string]map[string]struct{}   // clave encodificada → set de _id  (non-unique)
unique  map[string]string                // clave → _id                       (unique)
ord     []ordEntry                       // claves distintas ordenadas por (val, key)
ordDirty bool                            // lazy sort antes de binary search
```

`ordEntry{val any, key string}`:
- `val` = valor crudo del **primer campo** (para `compareOrdered`)
- `key` = clave full encodificada (`field1\x1ffield2…`)

### Orden total (para rangos y sort por índice)

```
typeRank: nil(0) < números(1) < string(2) < bool(3) < otros(4)
dentro del rank: compareValues (numérico o lexicográfico)
bool: false < true
otros: solo empate por key
```

Rangos **nunca** cruzan tipos (semántica Mongo-like).

### Encoding de clave

| Valor | Encoding |
|---|---|
| nil / missing | `u:` |
| string | `s:len:valor` |
| number | `n:formatFloat` |
| bool | `b:true` / `b:false` |
| object | `j:{json}` |
| campos compuestos | join con `\x1f` |

Arrays top-level: cada elemento genera una fila de índice (multi-valor). Array vacío → indexa `nil`.

### Mantenimiento

- `addDoc` / `removeDoc` en cada Insert/Update/Upsert/Delete.
- Único: conflicto → `ErrDuplicate` con rollback completo (no media aplicación).
- `newIndex` arranca `ordDirty=true` (bulk build: append + un solo sort).
- `ordAdd` en índice limpio: insert binario (no re-sort).
- `ordRemove` con dirty: linear scan (evita sort forzado por updates).

---

## 6. Persistencia vs RAM

| Aspecto | Decisión |
|---|---|
| Fuente de verdad runtime | docs en entries; page cache second-chance con budget `CacheBytes` |
| Escritura | log de records cifrados v2 (DOC/META/IDX/DEL/COMMIT) — no WAL |
| Compresión | zlib solo si reduce tamaño (flag en header) |
| Reapertura | descifra + scan hasta COMMIT + rebuild índices/matrix + reload triggers |
| Snapshot | misma codificación, ruta distinta (backup) |
| Compact | reescribe el log sin DOC/DEL muertos (M1) |
| Rotación de clave | `RotateKeys` header-only (H8) |
| Corrupción semántica (docs inválidos) | `Repair(path, opts)` repara y reescribe; falla criptográfica → `ErrCorrupt` (H6) |

---

## 7. Seguridad (resumen)

```
KEK   = Argon2id( SHA256(master ‖ 0 ‖ machineID), salt, t, m, p )
DEK   = 32B aleatorios por archivo
wrap  = AES-GCM(KEK, salt[:12], DEK)
payload = AES-GCM(DEK, nonce, compress(records v2))
HMAC  = HMAC-SHA256(KEK, header_body[0..120])
```

- `machineID`: `Options.MachineID` o MachineGuid (Windows) o `"default"`.
- Archivos viejos con HMAC `"default"` reabren con retry legacy.
- Clave incorrecta / header alterado / payload alterado → `ErrCorrupt` (no reescribe el original).

**RBAC embebido (M4):** passwords argon2id PHC en `_users`; roles/permisos en
`_roles` con `FieldDeny`; `Authenticate` → `*Session` (token, TTL, GC lazy).
Con ≥1 usuario, el API crudo exige sesión; system colls → `ErrForbidden`.

Parámetros Argon2 (persistidos en header):

| Modo | t | mem KiB | p |
|---|---|---|---|
| default | 1 | 65536 | 4 |
| LightKDF (tests) | 1 | 8192 | 1 |

---

## 8. Decisiones de diseño registradas

| # | Decisión | Alternativa descartada | Motivo |
|---|---|---|---|
| D1 | Carga todo en RAM al abrir | mmap / lazy pages | Volumen acotado; Find simple |
| D2 | Flush completo sin WAL | WAL + checkpoints | Simplicidad; pérdida ≤2s aceptada (salvo FlushSync/SyncOnWrite) |
| D3 | Un solo ciphertext | segmentos | Archivos ≤ ~50MB |
| D4 | `_id` string siempre | ObjectID binario | IDs de sistemas externos son strings |
| D5 | Hash + ord en RAM, no B-tree persistente | B-tree on disk | Dataset en RAM |
| D6 | Índices reconstruidos al abrir | persistir estructuras índice | Menos código, reconstrucción rápida |
| D7 | KEK = proveedor + machine | contraseña usuario | Cliente no puede descifrar solo |
| D8 | Update = shallow merge top-level | `$set/$unset` completos | Suficiente para apps embebidas |
| D9 | Auto-flush 2s | flush por mutación | Coalescing de ráfagas |
| D10 | Colecciones autodescubiertas | DDL | Como Mongo |
| D11 | Deep clone en bordes | copy-on-write | Corrección > micro-optimización |
| D12 | Flush fuera de `s.mu` + dirtyGen | flush bajo write lock | No bloquear lectores ~400ms |
| D13 | Rangos siempre re-match | confiar solo en índice | Cross-type / residual |
| D14 | Regex cache global | compilar por query | Patrones repetidos |
| D15 | `FlushSync`/`SyncOnWrite` opt-in | WAL completo | Durabilidad por endpoint sin replay |
| D16 | `Repair()` solo semántico | intentar arreglar GCM | Tag GCM all-or-nothing ⇒ Snapshot |
| D17 | Formato v2: log de records + COMMIT (M1) | snapshot JSON completo | Append barato, crash-safe, page cache |
| D18 | `index_matrix` CSR persistida (M2) | reconstruir solo desde `ord` | Open más rápido en índices grandes |
| D19 | Find paralelo + backpressure + Update COW (M3) | todo serial | Throughput multi-core sin romper aislamiento |
| D20 | RBAC embebido lazy con `_users` (M4) | auth externa + middleware | Autónomo; `New()` sin system colls (test de colecciones) |
| D21 | Triggers = hooks Go **y** JSON declarativo (M5a/M5b) | solo uno de los dos | Flexibilidad código + reglas de datos |
| D22 | Grafo sobre colecciones `edges.*` + CSR en epoch (M6) | DB de grafo separada | Un solo motor; anti-ahogo con límites BFS |
