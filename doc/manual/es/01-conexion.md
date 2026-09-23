# 01 — Conexión

Cómo abrir y cerrar una base de datos MLStoreDB desde Go.

## Base de datos en memoria

```go
package main

import "mlstoredb/db"

func main() {
    store := db.New()          // RAM, sin archivo
    defer store.Close()

    _ = store.Insert("saludo", db.Document{"_id": "h1", "msg": "hola"})
}
```

## Base de datos en archivo cifrado

```go
store, err := db.OpenWithLock("datos.mlstore", db.Options{
    MasterKey: []byte("mi-clave-maestra-de-32-bytes!!"), // KEK
    MachineID: "mi-equipo",                               // opcional
})
if err != nil {
    panic(err)
}
defer store.Close()
```

- `Open` abre/crea el archivo **sin** lock de proceso.
- `OpenWithLock` además toma un lock exclusivo (`mlstoredb.lock`): dos
  procesos no pueden abrir la misma BD a la vez.
- El archivo queda cifrado con AES-256-GCM (nunca hay JSON en claro).

## Opciones útiles de `Options`

| Campo | Qué hace |
|---|---|
| `MasterKey []byte` | Clave maestra (KEK). Requerida para archivos. |
| `MachineID string` | Identidad de la máquina mezclada en la clave. |
| `LightKDF bool` | Argon2 más rápido (solo la primera creación). |
| `SyncOnWrite bool` | Flush síncrono tras cada mutación (durabilidad). |
| `AutoFlush time.Duration` | Período del flush automático (default 2s). |
| `CacheBytes int64` | Presupuesto del page cache (0=256MB, <0=ilimitado). |
| `FindWorkers int` | Goroutines para Find paralelo (0=GOMAXPROCS). |
| `MaxPendingWrites int` | Backpressure de escrituras durante flush. |
| `SessionTTL time.Duration` | Vida de las sesiones RBAC (0=24h, <0=nunca). |

## Usuarios, roles y sesiones (RBAC)

```go
// 1) crear rol con permisos
store.CreateRole("editor", []db.Permission{
    {Collection: "orders", Read: true, Write: true},
    {Collection: "audit",  Read: true},              // solo lectura
})
// rol comodín
store.CreateRole("admin", []db.Permission{
    {Collection: "*", Read: true, Write: true},
})

// 2) crear usuario (activa RBAC en el primer usuario)
store.CreateUser("ana", "secreto", []string{"editor"})

// 3) autenticar → sesión
sess, err := store.Authenticate("ana", "secreto")
if err != nil { // ErrUnauthorized
    panic(err)
}
docs, _ := sess.Find("orders", db.Document{"status": "PAID"}, nil)
```

- Sin usuarios: la API cruda del `Store` funciona sin restricciones.
- Con ≥1 usuario: la API cruda devuelve `ErrUnauthorized` y hay que usar
  la `Session` (con redacción `fieldDeny` incluida).

## Conexión desde clientes Mongo (Navicat / Compass / mongosh)

Arranca el servidor wire:

```bash
go run ./tools/mls-server -addr 127.0.0.1:28917 -path datos.mlstore \
    -key "mi-clave-maestra-de-32-bytes!!" -db midb
```

Y conéctate con `mongodb://127.0.0.1:28917`. Detalle completo en
[09-servidor-mongo.md](09-servidor-mongo.md).
