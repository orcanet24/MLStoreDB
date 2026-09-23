# Recrear el motor como proyecto aislado (multipropósito)

Guía para extraer `mlstore` del repo origen y tener un **módulo de BD propio**, sin dependencias de Mercado Libre.

> **Estado:** la extracción ya está hecha en este repo — módulo `mlstoredb`,
> package `db`, tools en `tools/`. Esta guía queda como checklist de referencia.

---

## 1. Qué NO llevar

| Dejar fuera | Motivo |
|---|---|
| Colecciones `ml_*`, `product_slots`, … | dominio ML |
| `cmd/mld` con questions de ML | es smoke del producto (aquí: `tools/smoke`) |
| Docs de PLAN_MAESTRO / informes ML | producto, no motor |
| Lógica free/full, licencias, OAuth | capa de negocio |
| Cualquier import de `internal/ml` o `internal/services` | el motor no los tiene hoy (verificar al extraer) |

El package `db` **hoy no importa nada del dominio ML**. Solo trae `Document`, `Store`, etc.  
El único “nombre ML” residual es el prefijo histórico en textos/docs y magic `MLDB` — renombrables (ver §5).

---

## 2. Estructura destino sugerida

```
miproyecto-bd/
├── go.mod                 # module github.com/usted/mibd
├── README.md
├── db/                    # package db (40 archivos .go)
│   ├── store.go · index.go · filter.go · filter_regex.go
│   ├── file.go · filev2.go · cache.go · parallel.go
│   ├── runtime.go · export.go · errors.go · repair.go · rotate.go
│   ├── machineid.go · machine_windows.go · machine_nonwindows.go
│   ├── auth.go · hooks.go · triggers.go · graph.go   # M4–M6
│   ├── zlib_helpers.go
│   ├── *_test.go          # copiar todas
│   └── bench_test.go
├── examples/
│   └── minimal/main.go
├── tools/
│   ├── loadtest/main.go
│   └── smoke/main.go
└── doc/
    └── (copiar doc/*)
```

**Regla:** el motor no debe conocer el app. El app importa `db`, no al revés.

---

## 3. Pasos de extracción

### 3.1 Copiar

```powershell
# desde la raíz del repo origen
New-Item -ItemType Directory -Path ..\miproyecto-bd\db -Force
Copy-Item db\*.go ..\miproyecto-bd\db\
Copy-Item doc\* ..\miproyecto-bd\doc\ -Recurse
Copy-Item tools\loadtest\main.go ..\miproyecto-bd\tools\loadtest\
Copy-Item tools\smoke\main.go ..\miproyecto-bd\tools\smoke\
```

### 3.2 go.mod nuevo

```go
module github.com/usted/mibd

go 1.26.5

require (
    github.com/gofrs/flock v0.13.1
    github.com/oklog/ulid/v2 v2.1.2
    golang.org/x/crypto v0.57.0
    golang.org/x/sys v0.48.0
)
```

```powershell
cd ..\miproyecto-bd
go mod tidy
```

### 3.3 Renombrar package

En todos los `.go` de `db/` (ya hecho en este repo):

```text
package mlstore  →  package db
```

En tools/examples:

```text
import "mld/internal/mlstore"  →  import "mlstoredb/db"
mlstore.  →  db.
```

### 3.4 Verificar

```powershell
go build ./...
go vet ./...
go test ./... -count=1
go run ./tools/smoke
go run ./tools/loadtest
```

Gate: **167 tests en verde** + smoke + loadtest + `scripts/test-race.ps1`.

---

## 4. Checklist de aislamiento

- [ ] `grep -r "ml_\|mercado\|MercadoLibre" db/` → sin resultados de dominio
- [ ] `grep -r "mld/internal" .` → sin referencias al repo viejo
- [ ] `go test ./... -count=1` OK
- [ ] `go vet ./...` OK
- [ ] Ejemplo mínimo corre en README
- [ ] Ningún archivo de `db/` importa `net/http`, OAuth, etc.
- [ ] `grep -r "FlushSync\|Repair" db/` → presentes (H6)

---

## 5. Renombres opcionales (mantener semántica)

| Actual | Sugerido | Dónde |
|---|---|---|
| prefijo error `db:` (antes `mlstore:`) | el que quieras | errors.go + fmt.Errorf |
| magic `MLDB` | `MYDB` | file.go `magicStr` **+** migración de archivos viejos si importa |
| lock `mlstoredb.lock` | `appdb.lock` | file.go `defaultLockFileName` (o `Options.LockFile`) |
| `fileMeta.App` de origen | app propia | file.go `toFileData` |

> Si cambiás `magicStr`, los archivos creados con `MLDB` no abren. Solo hacerlo en proyecto nuevo o con plan de migración.

---

## 6. Cómo extenderlo (multipropósito)

### 6.1 Campos sensibles

```go
s.SetSensitiveFields("sesiones", []string{"token", "ip"})
```

### 6.2 Migraciones en el bootstrap de TU app

```go
_ = s.ApplyMigrations([]db.Migration{
    {Version: 1, Name: "init", Up: func(st *db.Store) error {
        if err := st.EnsureIndex("users", []string{"email"}, true); err != nil {
            return err
        }
        return nil
    }},
    {Version: 2, Name: "sessions", Up: func(st *db.Store) error {
        return st.EnsureIndex("sessions", []string{"user_id"}, false)
    }},
})
```

### 6.3 Indexar según tus queries

Antes de cada `Find` crítico:

```go
ex := s.Explain("orders", filter)
if ex.Plan == "COLLSCAN" && ex.CollectionSize > 10000 {
    // log: agregar índice
}
```

### 6.4 Identidad por instalación y servidor de licencias

`ResolveMachineID(dbPath, "")` genera/lee un ULID por instalación en `<dir>/mldstore.machineid` (fuera de la BD). Úsalo como `Options.MachineID` para que la BD **solo** abra en esa instalación con ese master.

Flujo de activación recomendado (server de licencias):

```
1. instalador/primera ejecución → installID = ResolveMachineID(dbPath, "")
2. activación → POST https://tu-dominio/license/activate
               { install_id, machine_fingerprint?, app_version }
3. server guarda (cliente, install_id) → emite licencia.mld firmada
4. cada arranque → OpenWithLock(dbPath, {MasterKey, MachineID: installID})
```

Consecuencias operativas:

- BD copiada a otra PC → no descifra (falta el install-id de origen).
- Cliente pierde el archivo de identidad → el server conoce el ULID y puede reemitirlo.
- Misma installID en N máquinas → señal de licencia compartida (política de negocio).
- Cambio de PC legítimo → nueva identidad ⇒ requiere rewrite de la BD (Snapshot + reopen con nuevo MachineID) y re-registro en el server.

### 6.4b Rotación de clave

`RotateKeys(newMaster, newMachineID)` **implementada** (H8): re-wrap de la misma DEK con nuevo KEK — solo header, O(1), sin re-cifrar payload. Casos: master comprometido, cambio de PC legítimo (combinar con `ResolveMachineID` de la PC destino, §6.4), rekey post-incidente. Requiere store limpio (`FlushSync` antes).

### 6.5 Si necesitás WAL / multi-proceso

Fuera de alcance actual. El formato tiene `format_ver` y `flags` para evolucionar sin romper lectores v1. Para durabilidad inmediata usar `FlushSync` o `SyncOnWrite` (H6).

### 6.6 Si necesitás >1MB por doc

Cambiar `maxDocBytes` en store.go y documentar el nuevo límite (riesgo de OOM: todo va en RAM).

### 6.7 Si el archivo queda corrupto semánticamente

Usar `Repair(path, opts)` (H6): limpia `_id` inválidos, docs >1MB, unique conflicts y reescribe atómicamente. Falla criptográfica (GCM) → `ErrCorrupt`; usar Snapshot de respaldo.

---

## 7. Riesgos al recrear

| Riesgo | Mitigación |
|---|---|
| Cambiar magic y “perder” archivos | no cambiar en prod; o dual-read |
| MasterKey débil / hardcoded | secret del entorno o KMS; nunca en repo |
| BD copiada entre clientes/PCs | `ResolveMachineID` + registro en license server (§6.4) |
| Olvidar flock en app multi-instancia | usar `OpenWithLock` |
| Esperar transacciones | no existen; diseñar idempotencia en el caller |
| Dataset > RAM | page cache con budget (`CacheBytes`); medir heap con loadtest |
| Archivo corrupto semánticamente | `Repair(path, opts)` (H6); respaldo previo con Snapshot |
| Auth desactivada por error | RBAC se activa con el **primer** `CreateUser`; si no hay usuarios, el API crudo queda abierto a propósito (bootstrap) |
| `-race` sin CGO en CI | instalar gcc (MSYS2/MinGW) y correr `scripts/test-race.ps1`; o aceptar tests sin race |

---

## 8. Extensiones ideas (roadmap multipropósito)

Priorizadas por valor general:

1. ~~**RotateKeys** — rewrap DEK sin reencrypt de payload completo (solo header)~~ → **hecho (H8)**.
2. ~~Formato v2 paginado + page cache~~ → **hecho (M1)**.
3. ~~`index_matrix` CSR persistida~~ → **hecho (M2)**.
4. ~~Find paralelo + backpressure + Update COW~~ → **hecho (M3)**.
5. ~~RBAC embebido~~ → **hecho (M4)**.
6. ~~Hooks Go + triggers JSON~~ → **hecho (M5a/M5b)**.
7. ~~Grafo edges/Traverse/ShortestPath~~ → **hecho (M6)**.
8. **Wire protocol Mongo-compatible (OP_MSG)** — M8, pendiente de aprobación.
9. **Índices textuales** — `$text` / prefix search más allá de `ord` de string.
10. **TTL helper** — job que borra docs por `expires_at` (la app ya puede).
11. **WAL opcional** si hay caso financiero.
12. **Iteradores / batch Find** para datasets que no caben en una página clonada.
13. **Compression zstd** como flag alternativo a zlib.

---

## 9. Comandos de validación final

```powershell
cd miproyecto-bd
go build ./...
go vet ./...
go test ./... -count=1
go test ./db -bench=. -benchtime=1x -run=XXX
powershell -File scripts/test-race.ps1   # -race con CGO+gcc
go run ./tools/smoke
go run ./tools/loadtest
```

Cuando todo pase: el motor está **listo como dependencia de cualquier app Go**, sin Mercado Libre.
