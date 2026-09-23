# Lenguaje de consulta, índices y planner

## 1. Operadores de filtro

### Igualdad

| Sintaxis | Semántica |
|---|---|
| `{"status": "paid"}` | igualdad implícita (`$eq`) |
| `{"n": {"$eq": 5}}` | igualdad explícita |
| `{"st": {"$in": ["A","C"]}}` | pertenece a la lista |
| `{"st": {"$nin": ["A"]}}` | no pertenece; **missing → true** |
| `{"n": {"$ne": 5}}` | distinto; **missing → true** (si arg no es nil) |
| `{"v": null}` / `$eq: null` | null **o** campo ausente (sintaxis Mongo) |

Números: `int` y `float64` comparan iguales (`42 == 42.0`).

### Rango

| Op | Missing field |
|---|---|
| `$gt $gte $lt $lte` | **no matchea** |

- Solo comparable **mismo tipo**: número↔número, string↔string.
- Cross-type → no match (no error).
- Combinaciones puras (`$gte` + `$lt`) se planifican como un solo rango.

### Presencia

| Op | |
|---|---|
| `$exists: true/false` | campo presente (aunque sea null) |

### Regex

```go
Document{"tag": Document{"$regex": "^a", "$options": "i"}}
```

- RE2 (`regexp` Go).
- `$options` con `i` → prefijo `(?i)`.
- Solo matchea valores **string**; non-string → false.
- Patrón compilado una vez y cacheado (`regexCache`); errores también cacheados → `ErrBadFilter`.

### Lógicos (claves top-level del filter)

```go
Document{"$and": []any{ Document{...}, Document{...} }}
Document{"$or":  []any{ ... }}
Document{"$not": Document{ ... }}
Document{"$nor": []any{ ... }}
```

- Varios campos top-level en el mismo filter = **AND** implícito.
- Operadores lógicos **no** anulan seeks de otros campos; solo marcan `exact=false` (re-match).

### No soportado

`$elemMatch`, `$where`, `$expr`, `$size`, `$type`, `$mod`, `$all`, `$slice`…  
Clave desconocida con prefijo `$` → `ErrBadFilter`.

---

## 2. Rutas anidadas (dot notation)

```go
Document{"from.name": "nested"}
Document{"items.item_id": "B"}      // array: ANY element
Document{"payload.shipping.mode": "me2"}
```

- `lookupMulti` camina objects y expande arrays.
- En índice: missing y null comparten sentinel `u:`.
- Array vacío al indexar → `nil`.

---

## 3. Semántica de arrays (match)

| Caso | Comportamiento |
|---|---|
| Igualdad sobre array | true si **algún** elemento iguala |
| Operadores sobre array | true si **algún** elemento satisface **todos** los ops |
| `$ne` sobre array | negación de la igualdad multi-elemento |
| Índice sobre array | cada elemento indexa (multi-valor) |
| Unique + array | dos docs no pueden compartir ningún valor del array |

---

## 4. Índices

### Tipos

| Tipo | Uso |
|---|---|
| Simple no unique | equality, `$in`, count, prefijo de compound |
| Simple unique | lo anterior + unicidad de valor |
| Compuesto | equality en prefijo; unique de la tupla completa |
| Ordenado (`ord`) | rangos + sort por primer campo |

### Creación

```go
_ = s.EnsureIndex("orders", []string{"status"}, false)
_ = s.EnsureIndex("orders", []string{"user_id", "status"}, false)
_ = s.EnsureIndex("users",  []string{"email"}, true)
```

### Codificación de clave

```
encodeIndexKey([v1, v2]) = enc(v1) + "\x1f" + enc(v2)

enc(nil)    = "u:"
enc(str)    = "s:" + len + ":" + str
enc(num)    = "n:" + FormatFloat
enc(bool)   = "b:true" | "b:false"
enc(object) = "j:" + json
```

### Orden total en `ord`

```
typeRank: nil=0 < number=1 < string=2 < bool=3 < other=4
dentro rank: compareValues
bool: false < other → false < true
other: solo empate por key string
```

Rangos no cruzan ranks. Si bounds de lo/hi caen en ranks distintos → sin matches.

### Lazy sort

- `newIndex` → `ordDirty=true` (build masivo: append + 1 sort en el primer seek).
- `ordAdd` con dirty → append; con clean → insert binario.
- `ordRemove` con dirty → linear scan; con clean → binary search + splice.

---

## 5. Planner (`planCandidates`)

### Clasificación por campo del filter

| Condición | fieldPlan |
|---|---|
| literal / `$eq` / `$in` | equality (`eqVals`) |
| solo `$gt/$gte/$lt/$lte` | range (`rangeBound`) |
| `$or`/`$and`/… u otro op | blocked → `exact=false` |

### Pasos

1. **Compound prefix**: para cada índice con `len(Fields)≥2`, toma equalities single-value en Fields[0..k] y hace `seekPrefix`.
2. **Equality simple** en campo restante: `seekEqualityField` (índice single o leading de compound).
3. **Range** en `Fields[0]` de algún índice: `seekRange` → IDs ordenados; **siempre** `exact=false`.
4. Intersección de candidatos; reconciliación de `order` vs `cand`.
5. `exact=true` solo si **todos** los campos del filter fueron cubiertos por equality.

### Resultado

```go
type planResult struct {
    cand       map[string]struct{} // IDs candidatos
    order      []string            // IDs en orden del índice (range)
    orderField string              // campo de orden (sort-by-index)
    exact      bool                // sin re-match
    used       bool                // hubo seek
    index      []string            // campos del índice usado
    covered    []string            // fields equality-served
}
```

### Find con plan

```
refs = docs de cand/order
if !exact → match(doc, filter) residual sobre cada candidato
if Sort un campo == orderField → reverse o usar orden del seek
si no Sort → sort por _id
si Sort distinto → sortByPartial (heap si Limit acotado)
skip → limit → clone/projection de la página
```

### Count con plan

- Fast path: un solo campo equality/`$in` + índice single → `countKey` (sin scan).
- Plan exact → `len(cand)`.
- Plan no exact → itera candidatos con `match`.
- Sin plan → full scan con `match` (sin clonar).

---

## 6. Sort

| Camino | Cuándo |
|---|---|
| Por índice | `Sort` de **un** campo == `orderField` del plan de rango (asc/desc) |
| Por `_id` | no hay Sort del usuario |
| Heap parcial O(n log k) | Sort single-key + `Limit`/`Skip` acotados (`need = skip+limit`) |
| Full multi-key estable | Sort multi-campo |

Orden de sort multi: primero `_id`, luego keys en orden alfabético inverso con `sort.SliceStable` (la primera key alfabética gana en empates después de `_id`).

Comparadores (`compareValues`): number, string, nil primero… (nil < todo). Bool/otros no ordenan entre sí en sort de usuario (0).

---

## 7. Projection

```go
&FindOptions{Projection: []string{"order_id", "status"}}
```

- Siempre incluye `_id`.
- Solo campos **top-level** existentes.
- Valores copiados en profundidad.

---

## 8. Explain — ejemplos

```go
// sin índice
s.Explain("c", Document{"x": 1})
// Plan=COLLSCAN ReMatch=true Candidates=len(c) 

// equality con índice
s.EnsureIndex("c", []string{"status"}, false)
s.Explain("c", Document{"status": "paid"})
// Plan=IXSCAN Exact=true Covered=[status] 

// rango
s.EnsureIndex("c", []string{"total"}, false)
s.Explain("c", Document{"total": Document{"$gte": 100}})
// Plan=IXSCAN Exact=false ReMatch=true Index=[total]

// compound completo
s.EnsureIndex("m", []string{"user","acct"}, false)
s.Explain("m", Document{"user":"u1","acct":"a1"})
// Plan=IXSCAN Exact=true Index=[user acct]
```

---

## 9. Semántica confirmada por tests

| Regla | Test |
|---|---|
| `$ne` incluye docs sin campo | filter tests |
| null/missing = sentinel `u:` | `TestNullAndMissingIndexSameSentinel` |
| ULID auto si falta `_id` | `TestInsertRequiresID` |
| schemaVersion inicial = 0 | `TestSchemaVersion` |
| Indexed == full-scan (rangos) | `TestRangeSeekIndexedMatchesFullScan` |
| Sort por índice == sort normal | `TestSortByIndexMatchesSortWithoutIndex` |
| Unique en Update con rollback | `TestUniqueIndexOnUpdate` |
| Deep clone en Get/Find/Insert | `TestDeepCloneNestedMutation` |
| dirtyGen no pierde mutación | `TestFlushDoesNotLoseConcurrentMutation` |
| corrupt payload → Snapshot recovery | `TestCorruptMainSnapshotRecovers` |
| Repair id inválido / unique conflict | `TestRepairInvalidIDType`, `TestRepairUniqueConflict` |
| FlushSync / SyncOnWrite durable | `TestFlushSyncPersistsImmediately`, `TestSyncOnWriteAutoPersists` |
| Prefijo compound | `TestCompoundPrefixEquality` |
