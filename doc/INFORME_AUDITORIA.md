# Informe de auditoría — docs ↔ código (M9x)

**Fecha:** 2026-09-26 · **Alcance:** proyecto completo (db + wire + adminweb + tools + doc/)
**Motivo:** validar todo lo hecho contra la documentación; hubo cambios sobre la marcha no documentados y trabajos a medias. Bitácora fuente de verdad: [`doc/PLAN_V2.md`](PLAN_V2.md) (sección M9x).

---

## 1. Resultado global

| | Auditoría (hallado) | Después de M9x |
|---|---|---|
| `go build` / `go vet` | ✅ ok | ✅ ok |
| `gofmt -l` | ❌ 23 archivos no canónicos | ✅ vacío |
| `go test ./... -count=1` | ✅ ok (real: **232** PASS) | ✅ **233** PASS / 0 FAIL |
| Número de tests en docs | ❌ 218 (doc/README) · 224 (PLAN_V2) · 225 (README) | ✅ 233 verificado con `-v` |
| `-race` | ✅ db + wire (script solo cubría 2 paquetes) | ✅ db + wire + **adminweb** |
| Cobertura docs vs código | ❌ multi-BD sin documentar; M9d hecho pero "pendiente" | ✅ sincronizado |
| UI completa | ❌ rewrite perdió índices/io/triggers/grafos/admin/i18n | ✅ restaurada y validada e2e |
| Benchmarks | ✅ 11 benchmarks reales corriendo | ✅ re-medidos y registrados |

**Veredicto:** el motor y el backend están sólidos (cero TODO/FIXME/stubs en
producción). Los problemas reales estaban en (a) la documentación atrasada
frente al código, (b) la UI reescrita para multi-BD que perdió funciones, y
(c) tres bugs P1 en el forwarder multi-BD que solo se manifestaban por HTTP
real (los tests unitarios los esquivaban).

---

## 2. Hallazgos detallados

| # | Hallazgo | Severidad | Corrección |
|---|---|---|---|
| A1 | M9d (usuarios/roles + consola de consultas) estaba **implementado y testado** (`adminweb/users.go` 201 L + `users_test.go` + `load_test.go`) pero la bitácora lo marcaba "pendiente" | doc | documentado como completado |
| A2 | `adminweb/multiserver.go` (342 L): **multi-BD completo sin documentar** — rutas `/api/db/{name}/…`, `POST /api/databases` (crea BD + admin), descubrimiento por `-dbdir`, sesión/cookie por BD | doc | documentado (PLAN_V2, DISTRIBUCION, README) |
| A3 | La UI fue reescrita para multi-BD y **perdió**: gestión de índices, import/export, triggers, **canvas de grafos** (`graph.js` 608 L existía pero no se cargaba en `index.html`), usuarios/roles, i18n ES/EN | regresión | UI restaurada (M9x-R1) y validada e2e |
| A4 | `PUT /api/users` al editar roles hacía delete+create con **contraseña fija `placeholder1`** → cualquier cambio de roles reseteaba la password a un valor conocido | **seguridad** | nueva API `Store.SetUserRoles` (in-place, preserva hash y campos) + test de regresión `db/setroles_test.go` |
| A5 | `forwardRequest` no copiaba `RawQuery` → `/export?format=csv` exportaba JSON, `/import?format=&mode=` perdía formato, browse/graph degradados vía proxy multi-BD | P1 | query preservado |
| A6 | `forwardRequest` conectaba el body con `io.Pipe`; al retornar el handler exterior, net/http cierra el body → **el import asíncrono quedaba a 0 insertados** (race con el worker) | P1 | body bufferizado acotado (512 MiB) antes del forward |
| A7 | El forward anteponía mal el prefijo: `POST /api/db/{name}/setup` → sub-servidor recibía `/setup` → **405**; los GET caían al index.html con 200 y `<!DOCTYPE` | P1 | subpath = `/api` + resto |
| A8 | Tabs de colección con `onclick` inline → **CSP propia (`script-src 'self'`) las bloqueaba**; consola: "Refused to execute inline event handler" | P2 | `data-tab` + listeners bindados |
| A9 | `graph.js` leía `nd.Vertex` pero la API serializa `vertex` (tag JSON `json:"vertex"`) → traverse no resaltaba; además llamaba dos veces al traverse | P2 | campo corregido + una sola llamada + cleanup del listener `resize` |
| A10 | 23 archivos `.go` sin formato `gofmt`; conteo de PASS divergente en 3 docs; sección "Gates M9" duplicada con números contradictorios | higiene | `gofmt -w`, números reales, bloques consolidados |

**Falsos positivos descartados durante la auditoría:** `a.txt` era un stub
vacío (`package adminweb`) → eliminado; `bin/mls-server.exe~` era un binario
huérfano → eliminado; `backup/` está vacío (se usa por `Snapshot(dest)` a
ruta elegida, no es directorio fijo) → documentado; los assets `static/`
nuevos no eran "pérdida" de diseño sino reescritura funcional.

---

## 3. Trabajo restaurado y corregido

- **M9x-R1 (UI):** `app.js` 936 L con i18n ES/EN (~190 claves ×2, persistente
  en `localStorage`), tabs por colección (Documentos · Índices ·
  Importar/Exportar · Triggers), consola de consultas, vista de grafos y vista
  de administración (usuarios + roles con matriz read/write/fieldDeny).
  `index.html` integra Grafos + Administración + selector de idioma +
  `<script src="/graph.js">`.
- **M9x-R2 (multi-BD):** forward con body bufferizado + query preservado +
  prefijo `/api`; UI con `apiUrl()` correcto y restauración de sesión por
  `localStorage.mls-db`.
- **Motor:** `Store.SetUserRoles(username, roles)` — reemplaza roles in-place
  bajo `s.mu`, valida que los roles existan, preserva `password_hash` y todo
  otro campo; marco el doc mutado para el próximo flush.

---

## 4. Gates verificados (2026-09-26)

```
go build ./...                                ✅
go vet ./...                                  ✅
gofmt -l db wire adminweb tools               ✅ vacío
go test ./... -count=1                        ✅ 233 PASS / 0 FAIL
  · db       181
  · wire      28
  · adminweb  24
go run ./tools/smoke                          ✅ smoke OK: Ana
go test ./db -bench=. -benchtime=1x -run=XXX  ✅ 11 benchmarks (i5-2430M):
  Insert10k 0.74 ms · FindFullScan10k 10.1 ms · FindIndexed10k 8.0 ms
  Flush10k 4.9 s · ExportCSV10k 174 ms · InsertComplex 0.20 ms
  GetComplexJSON 32 µs · FindComplexPage20 2.4 ms
  FindComplexProjection 9.4 ms · CountComplexNested 3.8 ms
  ReopenComplex10k 95 ms (LightKDF)
scripts/test-race.ps1                         ✅ db + wire + adminweb
node --check app.js + graph.js                ✅
```

### e2e vivo (navegador real → `mls-server -web 127.0.0.1:28918`)

Login multi-BD → crear BD `demobd` con admin → colección `clientes` →
insertar `c1`/`c2` → índice `ciudad` (crear/listar) → trigger `audit_cli`
(crear y **dispara de verdad**: `audit` recibe `{ref:"c2"}` vía `$get`) →
export CSV correcto (`_id,ciudad,nombre,total`) → import JSON asíncrono
inserta `c3`/`c4` (job + progreso) → rol `lector` read-only + usuario
`visor` → cambio de roles `lector→[lector,admin]` **preservando la password**
(`visor123` entra; `placeholder1` → 401; regresión A4) → aristas
`refiere` c1→c2→c3 → traverse BFS = `[c1,c2,c3]` → canvas: buscar `c1`,
expandir vecinos = **2 nodos · 1 arista** con leyenda por colección/tipo.

---

## 5. Estado final del módulo

| Componente | Líneas prod | Tests |
|---|---:|---|
| `db/` (motor) | 8.317 | 181 PASS |
| `wire/` (protocolo Mongo) | 4.591 | 28 PASS |
| `adminweb/` (consola web) | 2.296 (+2.204 assets) | 24 PASS |
| `tools/` (smoke · loadtest · mls-server) | 539 | — |
| **Total** | **15.743 + 2.204** | **233 PASS / 0 FAIL** |

Pendiente (planificado, no iniciado): **M10** — escalabilidad masiva
(index paging, checkpoint `.vtp`, reindex, grafos dinámicos). Ver
[PLAN_V2.md](PLAN_V2.md) §M10.
