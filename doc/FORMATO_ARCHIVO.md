# Formato del archivo `.mlstore`

> **v2 es el formato actual** (`format_ver = 2`, desde M1): log de records
> cifrados (DOC/META/IDX/DEL/COMMIT) tras el mismo header de 152 B, con
> page cache y `Compact()`. La sección siguiente documenta el **header**
> (común a v1 y v2) y el layout del cuerpo **v1** (JSON único), que sigue
> siendo soportado para migrar en el primer flush. El detalle completo de
> v2 (records, CRC, commit point, torn-tail) está en [PLAN_V2.md](PLAN_V2.md).

## 1. Layout físico

```
Offset  Tamaño  Campo
────────────────────────────────────────
0       4       magic = "MLDB"
4       2       format_ver = uint16 (v1 o v2)
6       2       flags uint16 (bit0 = payload comprimido zlib)
8       8       schema_version uint64
16      8       created_at uint64 (unix)
24      8       updated_at uint64 (unix)
32      16      kdf_salt (Argon2id)
48      4       kdf_time
52      4       kdf_mem_kib
56      4       kdf_par
60      12      gcm_nonce_base
72      48      dek_wrapped (32B DEK + 16B GCM tag)
──────────────── header body = 120 B (va en HMAC)
120     32      header_hmac = HMAC-SHA256(KEK, bytes[0..119])
──────────────── header total = 152 B (cleartext)
152     ...     payload = AES-256-GCM(DEK, nonce, body)
EOF-16  16      GCM tag del payload
```

**Total header fijo: `headerSize = 152`.**

- **v1:** el payload es un único ciphertext con `fileData` JSON (collections/docs/…).
- **v2:** el payload es un log de records (cada uno con su propio GCM);
  `COMMIT` es el único commit point; docs fríos se cargan bajo demanda.

---

## 2. Jerarquía de claves

```
material     = SHA256( masterKey ‖ 0x00 ‖ machineID )
KEK          = Argon2id(material, salt, t, m, p, 32 bytes)
DEK          = 32 bytes aleatorios (una por archivo)
dek_wrapped  = AES-256-GCM(KEK, nonce=salt[0:12], AAD=nil, plaintext=DEK)
nonce_base   = 12 bytes aleatorios (por escritura de payload)
payload      = AES-256-GCM(DEK, nonce=nonce_base, AAD=nil, plaintext=body)
header_hmac  = HMAC-SHA256(KEK, header_body[0:120])
```

### machineID en la práctica

| Caso | Valor usado |
|---|---|
| `Options.MachineID` no vacío | ese string |
| Vacío + Windows | `MachineGuid` del registro |
| Vacío + otro OS o error de registro | `"default"` |
| Archivo viejo creado con `"default"` y MachineID vacío | retry automático `"default"` en `Open` |

**Binding por instalación (recomendado para MLD):** usar `ResolveMachineID(dbPath, "")` → ULID aleatorio por instalación en `<dir>/mldstore.machineid` (fuera de la BD, fuera del cifrado). Ver [API.md](API.md). El master queda igual para todos; lo que hace único al archivo es la pareja master + install-id.

### Parámetros Argon2

Persistidos **en el header** (no hardcodeados al reabrir):

| Modo | Time | Memory (KiB) | Parallelism |
|---|---:|---:|---:|
| Producción default | 1 | 65536 (64 MB) | 4 |
| `LightKDF: true` (tests) | 1 | 8192 (8 MB) | 1 |

`Options.LightKDF` solo aplica al **crear** params por primera vez; al reabrir se usan los del header.

---

## 3. Plaintext del payload (`fileData`)

JSON ordenado para que el snapshot sea estable (docs por `_id`, claves de colección ordenadas al marshal de mapa — Go ordena map keys al serializar; docs se emiten con IDs sorteados):

```jsonc
{
  "meta": {
    "app": "mld",
    "schema_version": 3
  },
  "collections": {
    "usuarios": {
      "indexes": [
        { "fields": ["email"], "unique": true }
      ],
      "sensitive": ["internal_note"],
      "docs": [
        { "_id": "u1", "email": "a@b.c", "perfil": { "nombre": "Ana" } }
      ]
    }
  }
}
```

| Campo | Persiste |
|---|---|
| `meta.schema_version` | versión de migraciones |
| `collections.*.indexes` | solo **definición** (`fields`, `unique`); la estructura se rebuild al abrir |
| `collections.*.sensitive` | lista CSV por colección |
| `collections.*.docs` | array de documentos completos, IDs ordenados |

No se persisten: `entries`, `unique`, `ord`, dirty flags, locks.

---

## 4. Compresión

```go
plain = json.Marshal(fileData)
body, compressed = zlib(plain)   // solo si len(zlib) < len(plain)
if compressed { flags |= flagCompressed }
```

JSON de payloads repetitivos suele comprimir 3–8×. El flag va en el header.

---

## 5. Flush (escritura)

```
prepareFlush(path, clearDirty)
  │
  ├─ s.mu.Lock   (breve): params crypto primera vez, dirty/closed check
  │              copia dek, salt, kdf*, master, dirtyGen → flushState
  ├─ s.mu.RLock  (breve): st.fd = s.toFileData()  // deep copy docs ordenados
  │
writeState(st)   ← SIN s.mu, bajo flushMu
  │
  ├─ json.Marshal(st.fd)
  ├─ compressIfNeeded
  ├─ sealGCM(DEK, nonce aleatorio, body)
  ├─ deriveKEK + sealGCM wrap DEK
  ├─ header{...}.setHMAC(KEK)
  └─ atomicReplace(path, header||ct)
        tmp = CreateTemp(dir, name+".tmp*")
        tmp.Write / Sync / Close
        os.Rename(tmp, path)
        fallback Windows: Remove(path) + Rename de nuevo
  │
si clearDirty && dirtyGen == st.gen → dirty=false
si dirtyGen cambió → queda dirty (hubo mutación durante el flush)
```

**Invariantes:**
- Lectores (`Find`) pueden correr durante `writeState`.
- Mutadores pueden correr durante `writeState`; sus `markDirty` bump `dirtyGen`.
- Nunca se limpia `dirty` si hubo mutación después del snapshot.

---

## 6. Open (lectura)

```
Open(path, opts)
  │
  ├─ archivo no existe → DEK nuevo, dirty=true, listo
  ├─ len < headerSize+16 → ErrCorrupt
  ├─ parseHeader → magic, format_ver, params
  │     format_ver > v2 → error "newer than supported"
  ├─ KEK = deriveKEK(master, machine, salt, t,m,p)
  ├─ verifyHMAC(KEK)  (con retry legacy "default" si aplica)
  ├─ openGCM unwrap DEK
  ├─ openGCM payload → decompress → json.Unmarshal → loadFileData
  │     rebuild de índices a partir de docs + defs de index
  └─ dirty=false
```

Cualquier fallo de HMAC / unwrap / GCM / JSON → **`ErrCorrupt`**. El archivo original **no se modifica**.

Si el JSON decifrado es válido pero el contenido semántico está mal (`_id` no-string, unique conflict), `Open` falla con `ErrNoID`/`ErrDuplicate` y **`Repair(path, opts)`** puede reescribir un archivo limpio (drop + rebuild de índices). La corrupción criptográfica **no** es reparable ⇒ restaurar Snapshot.

---

## 6b. FlushSync / SyncOnWrite

- `FlushSync()` = `Flush` + `fsync` del archivo final (+ best-effort dir).
- `Options.SyncOnWrite=true` → cada mutación exitosa llama `FlushSync` antes de retornar (para tokens/settings).
- Sigue sin WAL: la ventana de pérdida en crash duro sin SyncOnWrite queda en ≤2s (auto-flush).

---

## 7. Snapshot (backup)

```go
s.Snapshot("backup.mlstore")
```

- Misma codificación que Flush.
- **No** cambia `s.path` ni `dirty` (usa `prepareFlush(dest, clearDirty=false)`).
- Bajo `flushMu` para no interleave con un Flush normal.

---

## 8. Lock de proceso

| Archivo | Contenido |
|---|---|
| `<dir>/<LockFile>` | flock exclusivo (gofrs/flock); default `mldstore.lock`, configurable vía `Options.LockFile` |

- `OpenWithLock` intenta `TryLock`; falla → `ErrAlreadyOpen`.
- `Open` (sin lock) no verifica — pensado para lecturas internas/tests.
- `Close` libera el flock.

---

## 9. Auto-flush

```
ticker cada Options.AutoFlush (default 2s) → si path != "" → s.Flush()
```

- El ticker se detiene primero en `Close` (antes del flush final).
- Mutaciones post-Close en RAM no se persisten (ticker muerto; `Insert` todavía funciona en memoria pero no es el uso esperado).

---

## 10. Errores de archivo

| Error | Cuándo |
|---|---|
| `ErrCorrupt` | magic/HMAC/GCM/JSON inválidos; format_ver futuro en algunos caminos |
| `ErrAlreadyOpen` | flock ocupado |
| `errNoMaster` (interno) | `Snapshot`/`Flush` sin `Options.MasterKey` |
| error texto | `MasterKey` vacía en `Open`; `format_ver` más nuevo que soportado |

---

## 11. Compatibilidad / evolución

| Campo | Uso futuro |
|---|---|
| `format_ver` | cambios de layout binario; v1 = actual |
| `flags` | bit1+ reservados (WAL, segmentos, etc.) |
| `schema_version` | migraciones lógicas de contenido (no del binario) |
| Header v1 | lectores futuros deben seguir abriendo v1 |

---

## 12. Límites

| Límite | Valor | Error |
|---|---|---|
| Documento | 1 MB (`1<<20`) | `ErrTooLarge` |
| Tamaño total BD | sin hard-limit en código ( diseño contempla ~1GB; observar heap) | — |
| Pérdida crash | ≤ ~2s | diseño D2/D9 |
