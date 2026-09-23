# proyecto_bd — Motor de base de datos multipropósito (JSON + índices + cifrado)

Documentación completa del motor `mlstore` para **recrear un motor de BD propio, aislado del proyecto Mercado Libre**.

Este directorio describe el motor tal como está hoy: arquitectura, distribución de archivos, formato binario, API, lenguaje de consulta, persistencia, pruebas y benchmarks. No depende de colecciones ni dominio ML: todo lo específico de Mercado Libre queda fuera.

| | |
|---|---|
| **Nombre interno** | `mlstore` (módulo `mlstoredb`, package `db`) |
| **Lenguaje** | Go 1.26 (CGO solo para `-race` en tests) |
| **Tipo** | Document store embebido (estilo MongoDB) |
| **Persistencia** | 1 archivo cifrado, formato **v2** (log de records + AES-256-GCM + Argon2id) |
| **Modelo** | Docs en RAM con page cache (budget `Options.CacheBytes`); snapshot cifrado |
| **Tests** | **167 PASS / 0 FAIL** |
| **Producción** | ~7.365 líneas · **Tests** ~5.136 líneas · **Total** 40 archivos `.go` en `db/` (+ `tools/`) |

---

## Índice

1. [ARQUITECTURA](ARQUITECTURA.md) — componentes, modelo de datos, concurrencia, ciclo de vida
2. [DISTRIBUCION_ARCHIVOS](DISTRIBUCION_ARCHIVOS.md) — mapa de archivos, líneas, responsabilidad de cada uno
3. [FORMATO_ARCHIVO](FORMATO_ARCHIVO.md) — layout del `.mlstore`, jerarquía de claves, flush atómico
4. [API](API.md) — API pública Go completa con ejemplos (CRUD, auth, hooks, triggers, grafo)
5. [CONSULTAS](CONSULTAS.md) — filtros, índices, planner, sort, `Explain`
6. [PRUEBAS](PRUEBAS.md) — suites, cobertura por área, cómo correrlas, benchmarks
7. [RECREAR](RECREAR.md) — guía paso a paso para extraer el motor a un módulo propio
8. [PLAN_V2](PLAN_V2.md) — bitácora del plan v2 (M0→M8): RBAC, hooks, triggers, grafo

**Estado v2:** plan M0–M7 cerrado — extracción a `package db`, formato v2 paginado +
page cache, `index_matrix` CSR persistida, Find paralelo + backpressure + Update COW,
RBAC embebido (`Authenticate`→`Session`, `_users`/`_roles`), hooks Go, triggers
JSON declarativos y grafo (`edges.<tipo>`, `Neighbors`/`Traverse`/`ShortestPath`).
Estadísticas: **167 PASS / 0 FAIL**, 40 archivos `.go` en `db/` (prod 7.365 L ·
tests 5.136 L), `-race` verde.

---

## Qué es (y qué no es)

**Es:**
- Document store JSON con `_id` string
- Índices hash + ordenados (equality, `$in`, rangos, prefijo compuesto, sort por índice) + matriz CSR persistida (`index_matrix`)
- Un solo archivo cifrado en disco (formato v2: log de records + COMMIT); backup = copiar 1 archivo
- API estilo Mongo acotada: `Insert/Upsert/Update/Delete/Get/Find/Count`
- RBAC embebido (usuarios/roles/permisos, sesiones con TTL) y export CSV
- Hooks Go (`before`/`after` por evento) + triggers JSON declarativos
- Grafo: colecciones `edges.<tipo>` con `Neighbors`/`Traverse`/`ShortestPath`
- Migraciones de esquema forward-only
- Seguro para concurrencia en un proceso (RWMutex + flock + backpressure de escrituras)
- `FlushSync`/`SyncOnWrite` (durabilidad inmediata) + `Repair()` (H6)

**No es:**
- SQL, transacciones multi-colección, joins, aggregation pipelines
- Multi-proceso writer (un solo dueño del archivo)
- WAL / ledger financiero (pérdida máxima ~2s en crash; `FlushSync`/`SyncOnWrite` para durabilidad inmediata)
- ORM ni servidor de red (es embebido, in-process; el wire protocol Mongo es M8 pendiente)

---

## Arranque mínimo (aislado de ML)

```go
package main

import (
    "fmt"
    "mlstoredb/db" // package extraído (módulo mlstoredb)
)

func main() {
    s, err := db.OpenWithLock("datos.db", db.Options{
        MasterKey: []byte("clave-maestra-32-bytes-long!!!!"),
        MachineID: "mi-app",       // "" = MachineGuid (Windows) o "default"
        LightKDF:  false,          // true solo en tests
        // SyncOnWrite: true,      // opcional: cada mutación es durable al retornar
        // CacheBytes: 256<<20,    // presupuesto page cache (0=default, <0=ilimitado)
    })
    if err != nil { panic(err) }
    defer s.Close()

    _ = s.EnsureIndex("usuarios", []string{"email"}, true)

    _ = s.Insert("usuarios", db.Document{
        "_id":   "u1",
        "email": "ana@ejemplo.com",
        "perfil": db.Document{"nombre": "Ana"},
    })

    docs, _ := s.Find("usuarios", db.Document{"email": "ana@ejemplo.com"}, nil)
    fmt.Println(len(docs), docs[0]["perfil"])

    // RBAC (opcional): al crear el primer usuario, el API cruda exige sesión
    _ = s.CreateRole("admin", []db.Permission{{Collection: "*", Read: true, Write: true}})
    _ = s.CreateUser("root", "segura-larga-1", []string{"admin"})
    sess, _ := s.Authenticate("root", "segura-larga-1")
    _, _ = sess.Find("usuarios", nil, nil)
}
```

Ver [RECREAR.md](RECREAR.md) para extraer a módulo independiente.

---

## Estado

| Hito | Estado |
|---|---|
| M1 CRUD + RAM | ✅ |
| M2 Filtros + sort/limit/skip/projection | ✅ |
| M3 Índices + únicos + planner | ✅ |
| M4 Formato + cifrado + Open/Close/Flush | ✅ |
| M5 Snapshot + flock + auto-flush 2s | ✅ |
| M6 ExportCSV + migraciones | ✅ |
| M7 Benchmarks + límite 1MB/doc | ✅ |
| H1 Índice ordenado + range seek | ✅ |
| H2 Planner prefijo/rango + Explain | ✅ |
| H3 Flush no bloqueante (dirtyGen) | ✅ |
| H4 Deep clone | ✅ |
| H5 Regex cache + MachineGuid + sensitive | ✅ |
| H6 FlushSync/SyncOnWrite + Repair + tests corrupt + -race | ✅ |
| H7 Machine ID por instalación (`ResolveMachineID`, ULID fuera de la BD) | ✅ |
| H8 `RotateKeys` (re-wrap DEK header-only, sin re-cifrar payload) | ✅ |
| H9 `Options` configurables (`AutoFlush`, `LockFile`) | ✅ |
| **Plan v2** (ver [PLAN_V2](PLAN_V2.md)) | |
| M0 Extracción a `package db` (módulo `mlstoredb`) | ✅ |
| M1 Formato v2 paginado + page cache + `Compact` | ✅ |
| M2 `index_matrix` CSR persistida | ✅ |
| M3 Find paralelo + backpressure + Update COW | ✅ |
| M4 RBAC embebido (users/roles/sessions) | ✅ |
| M5a Hooks Go (before/after, sync/async) | ✅ |
| M5b Triggers JSON declarativos | ✅ |
| M6 Grafo (`edges.*`, Neighbors/Traverse/ShortestPath) | ✅ |
| M7 Docs + benchmarks + race final | ✅ |
| M8 Wire protocol Mongo-compatible | ⬜ pendiente de aprobación |

Docs de motor: `doc/*`, bitácora `doc/PLAN_V2.md`, `scripts/test-race.ps1` (`-race` con CGO+gcc MSYS2).
