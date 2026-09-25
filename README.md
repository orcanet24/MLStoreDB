# MLStoreDB — Memory-Log Store Database

![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![Tests](https://img.shields.io/badge/tests-225%20PASS%20%2F%200%20FAIL-brightgreen)
![Race](https://img.shields.io/badge/-race-green)
![Licencia](https://img.shields.io/badge/tipo-embebida%20%2B%20servidor-blue)

> **Un archivo cifrado. Índices en memoria. Log append-only. Grafos nativos. Triggers. Y se conecta con tus herramientas de MongoDB favoritas — sin ser MongoDB.**

---

## La historia

MLStoreDB nació de un problema real: un CRM donde **todo era JSON** — órdenes, compradores, envíos, items con estructuras anidadas e impredecibles. Forzar eso a tablas relacionales era pelear contra la naturaleza de los datos.

La solución obvia era acceso directo al JSON, al estilo MongoDB. Pero entonces pensamos: **¿y si vamos más lejos?**

Dos decisiones de arquitectura definieron el motor — y el nombre:

- **Memory (Memoria):** los índices no se consultan desde disco. Se pre-computan como matrices CSR que viven en RAM y se cargan en microsegundos. Las búsquedas equality/$in resuelven en O(1) sin tocar disco. Los documentos calientes viven en un page cache con presupuesto configurable. El grafo completo (adyacencia CSR) vive en memoria.

- **Log:** no hay UPDATE in-place. Los registros de datos van a un log append-only cifrado con COMMIT atómico como único punto de consistencia. Sin WAL, sin fragmentación, self-heal ante torn-writes. Rápido para escribir, rápido para leer, simple para razonar.

**ML = Memory + Log.** Dos palabras que describen exactamente cómo funciona: índices en memoria, datos en log cifrado.

Pero además:

- Un vendedor conecta con compradores, que conectan con órdenes… eso es un **grafo**. ¿Por qué no tenerlo nativo, como Neo4j, sin otra base de datos más?
- Cuando entra una orden pagada queremos auditar, contar, notificar… eso son **triggers**. ¿Por qué no declararlos en JSON y que vivan dentro de la BD?
- Los datos son sensibles. ¿Por qué cifrar es un extra y no el **default**?

El resultado: una base de datos documental **simple, cifrada y de alto rendimiento** — donde "ML" no es una sigla de negocio, sino la descripción fiel de su arquitectura: índices en **Memoria**, datos en **Log**.

## ¿Qué es MLStoreDB?

| | |
|---|---|
| **Tipo** | Document store embebido (JSON con `_id`) + grafos + triggers |
| **Persistencia** | **1 solo archivo cifrado** (AES-256-GCM, Argon2id) — backup = copiar un archivo |
| **Acceso** | API Go embebida (como SQLite) **o** servidor wire protocol Mongo-compatible **o** consola web |
| **Consola web** | `mls-server -web` → CRUD, índices, import/export CSV+JSON, triggers y canvas de grafos interactivo |
| **Índices** | Hash + ordenados con **matriz CSR en RAM**; unique, compound, rangos |
| **Seguridad** | RBAC embebido: usuarios, roles, permisos por colección, `fieldDeny`, sesiones |
| **Grafos** | Aristas tipadas (`edges.<tipo>`), `Neighbors`, `Traverse` (BFS), `ShortestPath` — todo en RAM |
| **Automatización** | Hooks Go (`before`/`after`, veto) + triggers JSON declarativos persistidos |
| **Memoria** | Page cache con presupuesto (`CacheBytes`) — heap estable al crecer los datos |
| **Concurrencia** | RWMutex + striping, Find paralelo, backpressure de escrituras, `-race` verde |

### No es MongoDB. Se conecta como MongoDB.

MLStoreDB **habla el wire protocol de MongoDB** (subset `OP_MSG` + legado): apuntas **Compass, Navicat o mongosh** a `mongodb://127.0.0.1:28917` y funcionan — explorar colecciones, CRUD, `aggregate` con `$lookup`/`$group`, índices. Pero debajo no hay ningún código de MongoDB: es un motor propio, un archivo cifrado, sin instalación ni infraestructura. Es "habla mi idioma", no "soy tu base de datos".

## Rendimiento (en una laptop de 2011, sí, 2011)

Medido en un **Intel i5-2430M** con carga end-to-end de 10.000 documentos con payload real:

| Operación | Por operación | Nota |
|---|---:|---|
| **Lectura por `_id`** | **~5–10 µs** | índice hash en RAM, sin tocar disco |
| **Insert** | **~30–50 µs** | 10k docs ≈ 0.3–0.5 s |
| **Find con índice** | 1.7–1.9× más rápido que el scan | índice CSR single-field en memoria |
| **Count con índice** | **~5 µs** | O(1) vía `countKey` |
| **Página 20 con índice+sort+limit** | ~2.5 ms | sobre 10k docs complejos |
| **Lectura JSON complejo anidado** | **~24 µs** | deep-clone incluido |
| **Reabrir 10k docs cifrados** | ~100–420 ms | Argon2 + descifrado + rebuild de índices CSR |

> Si esto corre así en una laptop de 2011, imagínalo en hardware moderno — o en un ARM de borde consumiendo vatas.

**Memory + Log en acción:** los índices CSR se cargan en microsegundos desde el archivo, viven en RAM para seeks O(1), y el log append-only garantiza escrituras rápidas con durabilidad atómica. Sin WAL, sin fragmentación, sin sorpresas.

## Empieza en 30 segundos

### Modo embebido (como SQLite, pero documental)

```go
import "mlstoredb/db"

store, _ := db.OpenWithLock("crm.mlstore", db.Options{
    MasterKey: []byte("mi-clave-maestra-de-32-bytes!!"),
})
defer store.Close()

store.Insert("orders", db.Document{
    "_id": "o1", "total": 250, "buyer_id": "c1",
    "items": []any{db.Document{"sku": "A1", "qty": 2}},
})

docs, _ := store.Find("orders",
    db.Document{"total": db.Document{"$gte": 100}},
    &db.FindOptions{Sort: map[string]int{"total": -1}, Limit: 10})
```

### Modo servidor (conecta Compass / Navicat / mongosh)

```bash
go run ./tools/mls-server -addr 127.0.0.1:28917 -path crm.mlstore \
    -key "mi-clave-maestra-de-32-bytes!!" -db crm
```

```bash
mongosh mongodb://127.0.0.1:28917
```

En Windows: doble clic a **`iniciar-servidor.bat`** y listo. Para resetear credenciales sin borrar datos: `mls-server -path crm.mlstore -key "..." -reset-auth`.

### Grafos — relaciones nativas

```go
store.AddEdge("compro", "c1", "o1", db.Document{"canal": "web"})

amigos, _ := store.Neighbors("knows", "ana", db.Outgoing)
red, _    := store.Traverse(db.TraverseOptions{EdgeType: "knows", Start: "ana", MaxDepth: 2})
ruta, _   := store.ShortestPath("knows", "ana", "carla", 4)
```

### Triggers — lógica que vive en la BD

```go
store.CreateTrigger(db.Trigger{
    Event: "after_insert", Collection: "orders",
    Filter: map[string]any{"status": "PAID"},
    Actions: []db.TriggerAction{{
        Type: "insert", Collection: "audit",
        Doc: map[string]any{"ref": map[string]any{"$get": "_id"}, "note": "pagada"},
    }},
})
```

### Joins — 4 patrones para JSON anidado

Sin `JOIN` de SQL, pero mejor: embebe documentos, batch con `$in` (2 queries, sin N+1), `$lookup` en `aggregate` desde mongosh, o modela la relación como grafo. Guía completa en el [manual, tema 06](doc/manual/es/06-joins.md).

## ¿Para qué usarla?

MLStoreDB brilla donde necesitas **datos estructurados ricos sin infraestructura**:

- **CRMs y ERPs de escritorio** — un binario + un archivo cifrado, cero instalación de BD
- **Sistemas embebidos / edge / IoT** — footprint mínimo, RAM acotada por presupuesto, sin servidor
- **POS y retail** — opera local, sincroniza cuando hay red; el archivo cifrado viaja seguro
- **Analytics embebido** — `aggregate` con `$group`/`$lookup` dentro de tu propio proceso
- **Prototipos y MVPs** — la misma API que conoces de Mongo, sin Docker ni clúster
- **Cualquier app Go** que hoy usa SQLite pero sueña con documentos y grafos

## Bajo el capó

- **Memory:** índices hash + ordenados con matriz CSR en RAM; page cache con presupuesto; adyacencia de grafo en RAM.
- **Log:** formato v2 con log de records cifrados (bitcask-style) + COMMIT atómico; self-heal ante torn-writes.
- **Cripto:** Argon2id deriva el KEK → DEK → AES-256-GCM por record; `RotateKeys` re-wrap sin re-cifrar.
- **Durabilidad a elección:** auto-flush 2s (default), `SyncOnWrite` por escritura, o RAM pura.
- **Reset seguro:** `mls-server -reset-auth` borra usuarios/roles/sessions sin tocar los datos.

## 🧪 ¿Cómo puedes ayudar a validar MLStoreDB?

El motor principal está listo y estable, pero necesitamos validación real de la comunidad:

- **Pruebas de estrés en hardware moderno:** si tienes un servidor con SSD NVMe o arquitectura ARM (Apple Silicon, Raspberry Pi), corre los benchmarks (`go test ./db -bench=. -benchtime=1x -run=XXX`) y abre un Issue con tus resultados. Todos los números publicados aquí fueron medidos en un i5 de 2011.
- **Revisión criptográfica:** el flujo Argon2id → KEK → DEK → AES-256-GCM vive en [`db/file.go`](db/file.go) y [`db/filev2.go`](db/filev2.go). Si tienes experiencia en seguridad, revisa el diseño.
- **Compatibilidad de herramientas Mongo:** prueba a conectar tu ORM favorito (Mongoose, Prisma, driver oficial de Go/Python) a `mongodb://127.0.0.1:28917`.

## Estado del proyecto

| Fase | Estado |
|---|---|
| Motor: CRUD, filtros, índices, formato cifrado, snapshot, repair | ✅ |
| Formato v2 paginado + page cache + `Compact` | ✅ |
| Índices CSR persistidos en RAM | ✅ |
| Find paralelo + backpressure + Update copy-on-write | ✅ |
| RBAC embebido (usuarios/roles/sessions/fieldDeny) | ✅ |
| Hooks Go + triggers JSON declarativos | ✅ |
| Grafos (edges, Traverse, ShortestPath) | ✅ |
| **Wire protocol Mongo-compatible** (BSON, OP_MSG, CRUD, aggregate, SCRAM-SHA-256) | ✅ |
| **Consola web** (M9): CRUD, índices, import/export CSV+JSON, triggers, canvas de grafos, multi-BD | ✅ completo |
| **Reset de credenciales** (`-reset-auth`): borra auth sin tocar datos | ✅ |
| M10 (escalabilidad masiva: index paging, checkpoint .vtp, grafos dinámicos) | ⬜ propuesto |

## Documentación

- 📖 **[Manual completo ES/EN](doc/manual/README.md)** — conexión, CRUD, joins, triggers, grafos, servidor · versión interactiva: [manual.html](doc/manual/manual.html)
- 🏗️ [Arquitectura](doc/ARQUITECTURA.md) · 🗂️ [Distribución de archivos](doc/DISTRIBUCION_ARCHIVOS.md)
- 🔌 [API Go completa](doc/API.md) · 🔍 [Lenguaje de consultas](doc/CONSULTAS.md)
- 💾 [Formato del archivo](doc/FORMATO_ARCHIVO.md) · 🧪 [Pruebas y benchmarks](doc/PRUEBAS.md)
- 📋 [Bitácora del plan (M0→M10)](doc/PLAN_V2.md)

---

*MLStoreDB (Memory-Log Store Database):* ***M***emory — índices CSR y page cache en RAM · ***L***og — append-only cifrado con COMMIT atómico. Documental como MongoDB, grafos como Neo4j, embebida como SQLite, cifrada como ninguna. Un archivo, todo tu mundo.*
