# API pública del motor

Package: **`mlstoredb/db`** (módulo `mlstoredb`).

```go
type Document map[string]any
```

---

## 1. Ciclo de vida

### `New() *Store`

RAM vacía, sin archivo, `schemaVersion = 0`.

### `Open(path string, opts Options) (*Store, error)`

- Crea archivo nuevo si no existe.
- Si existe: descifra, parsea (v2: records + COMMIT; v1: cuerpo JSON), rebuild índices + matriz CSR, re-registra triggers.
- **No** toma flock ni auto-flush.

```go
type Options struct {
    MasterKey   []byte // requerido (material KEK)
    MachineID   string // "" = MachineGuid / "default"; ver ResolveMachineID
    LightKDF    bool   // true solo tests; solo al crear params
    SyncOnWrite bool   // true = cada mutación exitosa hace FlushSync
    AutoFlush   time.Duration // ticker de flush automático; 0/negativo = 2s
    LockFile    string        // nombre del lock file; "" = "mlstoredb.lock"
    CacheBytes  int64         // page cache budget; 0=256MiB, <0=ilimitado (M1)
    FindWorkers int           // Find/Count paralelo; 0=GOMAXPROCS, <0=serie, >0=n (M3)
    MaxPendingWrites int      // backpressure durante flush; 0=ilimitado (M3)
    SessionTTL  time.Duration // TTL de sesiones; 0=24h, <0=nunca (M4)
}
```

### `ResolveMachineID(dbPath, explicit string) (string, error)`

Identidad **por instalación** para `Options.MachineID` (binding cliente-máquina del §4.3 de diseño).

- `explicit` no vacío → se valida (`[A-Za-z0-9._-]`, ≤256 chars) y se devuelve tal cual; **no** lee ni escribe archivo (tests / override).
- Si no: usa el archivo `<dir>/mldstore.machineid` **junto a la BD** (nunca dentro):
  - Existe → devuelve su contenido validado (estable entre reinicios y actualizaciones del binario).
  - No existe → genera un ULID aleatorio, lo persiste (creación exclusiva; creadores concurrentes adoptan el id del ganador) y lo devuelve.

```powershell
// instalación / primera ejecución:
installID, err := db.ResolveMachineID(dbPath, "")

// cada arranque:
s, err := db.OpenWithLock(dbPath, db.Options{
    MasterKey: masterDelProveedor,
    MachineID: installID, // ← binding por instalación
})
```

**Registro en el servidor de licencias:** durante la activación el binario envía `installID` (+ fingerprint opcional) y el servidor lo guarda asociado al cliente. Con eso:

1. La BD de ese cliente **solo** abre en esa instalación (un `.mlstore` copiado no descifra en otra máquina aunque se conozca el master).
2. Si el cliente pierde `mldstore.machineid`, el servidor sabe qué ULID reemitir (recuperación).
3. La misma `installID` activándose en varias máquinas = señal de licencia compartida.

La identidad viaja **fuera** de la BD a propósito: si estuviera dentro, un atacante con el archivo la leería y conocería el material del KEK.

⚠️ BDs existentes creadas con `MachineID` vacío (MachineGuid/`"default"`) siguen abriendo igual; no cambiar el binding de un archivo existente sin rewrap/rewrite (ver `Snapshot`).

### `OpenWithLock(path string, opts Options) (*Store, error)`

`Open` + flock `mldstore.lock` + ticker auto-flush 2s.  
**Recomendado** para el binario dueño del archivo.

### `(*Store) Close() error`

Para ticker → Flush final → libera flock → `closed`. Idempotente.

### `(*Store) Flush() error`

No-op si no está dirty (o closed). Ver [FORMATO_ARCHIVO.md](FORMATO_ARCHIVO.md).

### `(*Store) FlushSync() error`

`Flush` + `fsync` del archivo final (y best-effort del directorio). Para escrituras críticas (tokens OAuth, settings) cuando necesitas durabilidad antes de responder al cliente. No-op de fsync si no hay path.

### `(*Store) Snapshot(dest string) error`

Backup cifrado a otra ruta; no cambia `path` ni dirty. Requiere MasterKey.

### `RotateKeys(newMaster []byte, newMachineID string) error`

Re-cifra el **wrap** de la DEK con nuevo master y/o MachineID **sin re-cifrar el payload** (operación header-only, O(1) — µs independiente del tamaño de la BD):

- Verifica primero el KEK **viejo** (HMAC + unwrap); clave actual incorrecta → `ErrCorrupt` y archivo intacto.
- Rechaza store dirty (el archivo en disco quedaría viejo) → `Flush`/`FlushSync` antes.
- Serializado contra Flush/Snapshot vía `flushMu`; Find/Get siguen funcionando (la DEK en RAM no cambia).
- Salt nuevo (evita reuso del wrap nonce) + HMAC recalculado + `fsync` final.
- El store en RAM **adopta** las nuevas credenciales: mutaciones posteriores flushean con el nuevo KEK.
- `newMaster`/`newMachineID` vacíos = mantener el valor actual (rewrap seguro).

```go
// rotación de master comprometido:
s, _ := db.Open(path, db.Options{MasterKey: oldMaster, MachineID: installID})
err := s.RotateKeys(newMaster, "")          // mismo machine, nueva master

// migración de máquina (con ResolveMachineID de la PC destino):
err = s.RotateKeys(nil, newInstallID)
```

Implementa el gap declarado en §7 (antes: Snapshot + reopen como único camino). Ver `RECREAR.md §6.4` para el flujo con servidor de licencias.

### `Repair(path string, opts Options) (*RepairReport, error)`

Recupera un archivo **semánticamente** corrupto y lo reescribe limpio (atomic replace + flock):

- descarta docs con `_id` no-string / >1MB / duplicados (gana el menor `_id`)
- conflictos de índice unique → baja los docs posteriores (`UniqueConflicts`)
- reconstruye todos los índices desde los docs supervivientes
- missing `_id` → auto ULID

**No** repara corrupción criptográfica (magic/HMAC/GCM/JSON) → `ErrCorrupt`; en ese caso restaura un `Snapshot`. Devuelve `ErrNotFound` si el archivo no existe, `ErrAlreadyOpen` si otro proceso lo tiene lockeado.

```go
type RepairReport struct {
    Path, Collections, DocsKept, DocsDropped int
    IndexesRebuilt, UniqueConflicts           int
    Resaved                                   bool
}
```

---

### `Compact() error` (M1)

Reescribe el log v2 completo sin registros DOC/DEL muertos (el archivo queda
en su tamaño mínimo). Requiere store abierto; serializa contra Flush.

### `CacheStats() CacheStats` (M1)

```go
type CacheStats struct {
    Resident int   // docs residentes en RAM
    Bytes    int64 // bytes estimados del presupuesto usado
    Evictions uint64
}
```

## 2. CRUD

> Cuando hay ≥1 usuario (RBAC activo, ver §6), el API **`Store.*` crudo**
> devuelve `ErrUnauthorized` en lecturas/escrituras — usar `Session.*`
> (`Authenticate` → `*Session`). Sin usuarios, el comportamiento es el de abajo.

### `Insert(coll string, doc Document) error`

- Auto ULID si falta `_id`.
- `_id` no string → `ErrNoID`.
- `_id` existente → `ErrDuplicate`.
- Doc > 1MB → `ErrTooLarge`.
- Conflicto unique → `ErrDuplicate` + rollback de índices.
- Deep clone de `doc`.

### `Upsert(coll string, id string, doc Document) error`

Reemplaza o crea. `id` gana sobre `doc["_id"]`. Setea `doc["_id"] = id`.

### `Update(coll string, id string, patch Document) error`

- Merge **shallow** de claves top-level (no deep-merge de objetos anidados).
- No permite cambiar `_id`.
- Cada valor del patch se deep-copia.
- Doc resultante > 1MB → error y rollback.
- Conflicto unique → error y rollback (documento queda como estaba).
- Colección/doc inexistente → `ErrNotFound`.

### `Delete(coll string, id string) error`

Quita doc y sus entradas de índice. Falta → `ErrNotFound`.

### `Get(coll string, id string) (Document, error)`

Deep clone. Falta → `ErrNotFound`.

---

## 3. Consulta

### `Find(coll string, filter Document, opts *FindOptions) ([]Document, error)`

```go
type FindOptions struct {
    Sort       map[string]int // 1 asc, -1 desc
    Limit      int
    Skip       int
    Projection []string       // campos top-level; _id siempre incluido
}
```

- `filter` nil/vacío → todos los docs.
- Orden por defecto: `_id` asc (determinístico) si no hay `Sort`.
- `nil, nil` como opts → solo filtro + orden `_id`.
- Colección inexistente → `(nil, nil)` (no error).
- Filtro inválido → `ErrBadFilter`.
- Solo la página final se deep-clona (projections copian en profundidad lo proyectado).

Operadores: ver [CONSULTAS.md](CONSULTAS.md).

### `Count(coll string, filter Document) (int, error)`

- Igualdad/`$in` single-field con índice → O(k) sin clonar.
- Si no, planner + match residual.
- Colección inexistente → `0, nil`.

### `Explain(coll string, filter Document) ExplainResult`

```go
type ExplainResult struct {
    Collection     string   `json:"collection"`
    Plan           string   `json:"plan"` // "COLLSCAN" | "IXSCAN"
    Index          []string `json:"index,omitempty"`
    Covered        []string `json:"covered,omitempty"`
    Candidates     int      `json:"candidates"`
    Exact          bool     `json:"exact"`
    ReMatch        bool     `json:"re_match"`
    FilterFields   []string `json:"filter_fields"`
    CollectionSize int      `json:"collection_size"`
}
```

No ejecuta `match` sobre docs; solo clasifica el plan.

### `Collections() []string`

Nombres ordenados.

### `SchemaVersion() uint64`

Versión actual de migraciones.

---

## 4. Índices

### `EnsureIndex(coll string, fields []string, unique bool) error`

- `fields` 1..N (compuesto, orden sensible).
- Idempotente si mismos campos + misma unique.
- Mismos campos con unique distinto → `ErrDuplicate`.
- Construye sobre docs existentes; confliclo unique → `ErrDuplicate` y **no** queda índice a medias.

### `DropIndex(coll string, fields []string) error`

Orden sensible. Falta → `ErrNotFound`.

### `ListIndexes(coll string) ([]IndexInfo, error)`

```go
type IndexInfo struct {
    Fields []string `json:"fields"`
    Unique bool     `json:"unique"`
}
```

Colección inexistente → lista vacía, nil.

---

## 5. Export y evolución

### `ExportCSV(coll string, w io.Writer) error`

- UTF-8 **BOM** + RFC 4180.
- Columnas: `_id` primero, luego unión de top-level keys ordenadas.
- Valores anidados/array → JSON string.
- Omite lista negra global + `SensitiveFields` de la colección.
- Colección inexistente → solo header `_id`.

### `SetSensitiveFields(coll string, fields []string)`

Reemplaza la lista local (se persiste en el archivo).

### `SensitiveFields(coll string) []string`

Copia de la lista local.

### `Migration` + `ApplyMigrations`

```go
type Migration struct {
    Version uint64
    Name    string
    Up      func(s *Store) error
}

err := s.ApplyMigrations([]Migration{
    {Version: 1, Name: "init", Up: func(st *Store) error {
        return st.EnsureIndex("usuarios", []string{"email"}, true)
    }},
})
```

- Solo `Up`, ordenadas por `Version`.
- Versiones ya aplicadas → no-op.
- `schemaVersion` store **mayor** que la última migración → error “update the program”.
- El lock se libera durante `Up` (puede llamar métodos del Store).
- Error en `Up` → no avanza `schemaVersion`.
- `schemaVersion` inicial store fresco = **0**.

---

## 6. RBAC embebido (M4)

Al crear el **primer usuario** se activa el RBAC: el API crudo `Store.*` pasa a
exigir sesión (`ErrUnauthorized`); las system colls (`_users`, `_roles`,
`_triggers`) siempre rechazan CRUD genérico (`ErrForbidden`).

```go
type Permission struct {
    Collection string   // nombre exacto o "*"
    Read       bool
    Write      bool
    FieldDeny  []string // campos redactados / filtros que los tocan → ErrForbidden
}
```

- `CreateRole(name string, perms []Permission) error`
- `CreateUser(username, password string, roles []string) error`
  — password hasheada con **argon2id PHC** (`$argon2id$…`); `LightKDF` acelera en tests.
- `Authenticate(username, password) (*Session, error)`
  — usuario desconocido y password malo → ambos `ErrUnauthorized`.
  Token 32B base64url; TTL `Options.SessionTTL` (0=24h, <0=nunca); GC lazy.
- `ChangePassword(username, newPassword) error` — revoca sesiones del usuario.
- `Revoke(token string)` / `session.Revoke()`
- `AuthActive() bool`

`Session` expone el mismo CRUD con RBAC aplicado:
`Get/Find/Count/Insert/Upsert/Update/Delete/ExportCSV` +
`Neighbors/Traverse/ShortestPath/AddEdge` (§9) + `User()`, `Roles()`, `Token()`.

---

## 7. Hooks Go (M5a)

```go
type HookFunc func(h *HookContext) error

type HookContext struct {
    Event      string // before_|after_ + insert|update|delete|upsert
    Collection string
    ID         string
    Doc        Document // work-copy (before puede mutarlo; delete → nil)
    Old        Document // pre-image (insert → nil)
}
// helpers: h.Get/Find/Count (lecturas) · h.Insert/Upsert/Update/Delete
// (mutaciones anidadas; propagan depth/ciclo; corren como principal confiable)
```

- `On(event, coll string, fn HookFunc) (uint64, error)` — síncrono.
  `before_*`: retorno de error **veta** la mutación; puede mutar `h.Doc`.
  `after_*`: corre inline; el error se ignora.
- `OnAsync(event, coll string, fn HookFunc) (uint64, error)` — solo `after_*`,
  FIFO acotado (1024; el enqueue bloquea = backpressure).
- `Off(id uint64) bool`
- `WaitAsyncHooks()` — barrera (no llamar desde dentro de un hook).
- Límites: depth ≤ 8 (`ErrHookDepth`), mismo hook en la cadena → `ErrHookCycle`.
- `coll` acepta comodín `*` / prefijo `coll.*`.
- Sin hooks registrados → cero overhead en el path caliente.
- Los hooks se detienen en `Close()`.

---

## 8. Triggers JSON declarativos (M5b)

Reglas en la system coll `_triggers`, registradas como hooks M5a
(reutilizan depth/ciclos/cola). Se re-cargan en cada `Open`.

```go
type TriggerAction struct {
    Type       string         // set | unset | insert | update | upsert | delete
    Collection string
    ID         any            // string o {"$get":"field"}
    Doc        map[string]any // para insert/upsert (acepta {"$get":…})
    Fields     map[string]any // set
    Names      []string       // unset (solo before_*)
    Set        map[string]any // update (merge)
}

type Trigger struct {
    ID         string          // _id
    Event      string          // before_|after_ + insert|update|delete|upsert
    Collection string          // "" o "*" = todas
    Enabled    *bool           // nil = true
    Async      bool            // solo after_* → FIFO
    Filter     map[string]any  // match() contra Doc (o Old si Doc nil)
    Actions    []TriggerAction
}
```

- `CreateTrigger(t) (id, err)` · `DeleteTrigger(id)` · `ListTriggers()`
- Templates: `{"$get":"field"}` contra `Doc`; `{"$get":"old.field"}` contra `Old`.
- `insert` sin `_id` recibe ULID automático.
- `set` en `before_*` muta el work-copy; en `before_update` se difea a
  `patch` + `remove[]` (así `unset` borra de verdad).
- Admin paths `adminInsert`/`adminDelete` (sin hooks ni RBAC) solo vía estas APIs.

---

## 9. Grafo (M6)

Aristas en colecciones normales `edges.<tipo>` con docs
`{_id, _from, _to, …props}`; adyacencia **CSR en RAM** (rebuild por epoch
ante cualquier mutación de `edges.*`).

- `AddEdge(edgeType, from, to string, props Document) (id, error)`
  — `_id` ULID auto; variantes `Session.AddEdge` (RBAC write).
- `RemoveEdge(edgeType, id string) error`
- `Neighbors(edgeType, vertex string, dir Direction) ([]string, error)`
  — `Outgoing` (default) | `Incoming` | `Both` (unión dedupe, ordenado).
- `Traverse(TraverseOptions) ([]TraverseNode, error)` — BFS:

```go
type TraverseOptions struct {
    EdgeType, Start string
    Direction Direction // 0 = Outgoing
    MinDepth, MaxDepth  int // MaxDepth 0=5, cap 32 (anti-ahogo)
    Limit                int // 0=10000, cap 100000
}
type TraverseNode struct { Vertex string; Depth int }
```

  Incluye `Start` en depth 0; resultados en orden de descubrimiento.
- `ShortestPath(edgeType, from, to string, maxDepth int) ([]string, error)`
  — BFS no ponderado → `[from,…,to]`, o `nil` si inaccesible.
  `maxDepth` 0=6, cap 32.
- Variantes `Session.*` verifican lectura/escritura sobre `edges.<tipo>`.

---

## 10. Errores sentinel

```go
var (
    ErrNotFound     = errors.New("db: not found")
    ErrDuplicate    = errors.New("db: duplicate _id or unique index")
    ErrCorrupt      = errors.New("db: corrupt file")
    ErrTooLarge     = errors.New("db: document or db size limit exceeded")
    ErrAlreadyOpen  = errors.New("db: file locked by another process")
    ErrNoID         = errors.New("db: document requires string _id")
    ErrBadFilter    = errors.New("db: invalid filter")
    ErrUnauthorized = errors.New("db: unauthorized")   // M4: sin sesión válida
    ErrForbidden    = errors.New("db: forbidden")      // M4: coll/sistema/fieldDeny
    ErrHookCycle    = errors.New("db: hook cycle detected")   // M5a
    ErrHookDepth    = errors.New("db: hook depth limit exceeded") // M5a
)
```

Usar siempre `errors.Is`.

---

## 11. No incluido (gaps conocidos)

| API de diseño | Estado |
|---|---|
| `RotateKeys(newMaster)` | ✅ implementada (`RotateKeys(newMaster, newMachineID)`, header-only) |
| Formato v2 paginado + page cache | ✅ M1 (`Compact`, `CacheBytes`, `CacheStats`) |
| `index_matrix` CSR persistida | ✅ M2 |
| Find paralelo + backpressure | ✅ M3 (`FindWorkers`, `MaxPendingWrites`) |
| RBAC / sesiones | ✅ M4 |
| Hooks + triggers | ✅ M5a/M5b |
| Grafo edges/traverse | ✅ M6 |
| Wire protocol Mongo (OP_MSG) | ⬜ M8 pendiente de aprobación |
| `$elemMatch`, `$where`, `$expr` | fuera de alcance |
| Transacciones | fuera de alcance |
| WAL | fuera de alcance |
| `$set` / `$unset` / `$inc` | no en Update crudo (el caller calcula); triggers M5b sí hacen set/unset |
| Collation / TTL / geoespacial | fuera de alcance |

---

## 12. Ejemplo completo de uso aislado

```go
s, err := OpenWithLock("app.db", Options{
    MasterKey: key,
    MachineID: "app-1",
})
if err != nil { return err }
defer s.Close()

// índices
_ = s.EnsureIndex("users", []string{"email"}, true)
_ = s.EnsureIndex("orders", []string{"user_id", "status"}, false)
_ = s.EnsureIndex("orders", []string{"total"}, false)

// escritura
_ = s.Insert("users", Document{
    "_id": "u1",
    "email": "ana@x.com",
    "prefs": Document{"theme": "dark"},
})

// lectura con índice
ex := s.Explain("orders", Document{
    "user_id": "u1",
    "status":  "paid",
})
// ex.Plan == "IXSCAN", ex.Exact == true

docs, err := s.Find("orders",
    Document{
        "user_id": "u1",
        "total":   Document{"$gte": 100},
    },
    &FindOptions{
        Sort:  map[string]int{"total": -1},
        Limit: 20,
        Projection: []string{"total", "status"},
    },
)

// RBAC
_ = s.CreateRole("admin", []Permission{{Collection: "*", Read: true, Write: true}})
_ = s.CreateUser("root", "clave-larga-1", []string{"admin"})
sess, _ := s.Authenticate("root", "clave-larga-1")
docs, _ = sess.Find("orders", nil, nil)

// hook síncrono
hookID, _ := s.On("before_insert", "orders", func(h *HookContext) error {
    if h.Doc["total"] == nil { return errors.New("total requerido") }
    return nil
})
_ = hookID

// trigger declarativo
_, _ = s.CreateTrigger(Trigger{
    ID: "audit_orders", Event: "after_insert", Collection: "orders",
    Actions: []TriggerAction{{
        Type: "insert", Collection: "audit",
        Doc: map[string]any{"ref": map[string]any{"$get": "_id"}},
    }},
})

// grafo
_, _ = s.AddEdge("knows", "u1", "u2", nil)
path, _ := s.ShortestPath("knows", "u1", "u2", 0)

// migraciones
_ = s.ApplyMigrations([]Migration{{Version: 1, Name: "boot", Up: seed}})

// backup
_ = s.Snapshot("backup.mlstore")

// export
f, _ := os.Create("users.csv")
defer f.Close()
_ = s.ExportCSV("users", f)
```
