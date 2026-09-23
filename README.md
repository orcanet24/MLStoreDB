# mlstoredb — Motor de base de datos JSON con grafo embebido

> **Origen del proyecto:** Este proyecto nace de la necesidad de fabricar un CRM para gestiones de Mercado Libre. Al trabajar con respuestas JSON, me di cuenta que era más simple almacenar y procesar directamente desde JSON en lugar de usar un motor SQL tradicional. Para las relaciones entre datos, busqué algo similar a Neo4j que permitiera conexiones espaciales eficientes, y así surgieron los grafos integrados en este motor.

**Autor:** Marcos Espinoza  
**Ubicación:** Yaracuy, Venezuela  
**Contacto:** orcanet1724@gmail.com  
**Licencia:** MIT (ver [LICENSE](LICENSE))

---

## ¿Qué es mlstoredb?

Un motor de base de datos multipropósito embebido escrito en Go, diseñado para trabajar nativamente con documentos JSON e incluir capacidades de grafo para relaciones complejas.

### Características principales

- **Document Store JSON**: Almacena y consulta documentos JSON con `_id` string
- **Índices avanzados**: Hash, ordenados, compuestos, matriz CSR persistida
- **Grafo integrado**: Colecciones `edges.<tipo>` con operaciones `Neighbors`, `Traverse` y `ShortestPath`
- **Persistencia cifrada**: Un solo archivo `.mlstore` con AES-256-GCM + Argon2id
- **RBAC embebido**: Usuarios, roles, permisos y sesiones con TTL
- **Hooks y Triggers**: Extensibilidad mediante hooks en Go y triggers declarativos JSON
- **Concurrencia segura**: RWMutex + flock + backpressure de escrituras

### Estado actual

| Componente | Estado |
|---|---|
| CRUD básico | ✅ |
| Filtros + sort/limit/skip/projection | ✅ |
| Índices + planner de consultas | ✅ |
| Formato v2 paginado + page cache | ✅ |
| RBAC (usuarios/roles/sesiones) | ✅ |
| Hooks Go + Triggers JSON | ✅ |
| Grafo (Neighbors/Traverse/ShortestPath) | ✅ |
| Tests (-race) | ✅ 167 PASS / 0 FAIL |

---

## Inicio rápido

```go
package main

import (
    "fmt"
    "mlstoredb/db"
)

func main() {
    s, err := db.OpenWithLock("datos.db", db.Options{
        MasterKey: []byte("clave-maestra-32-bytes-long!!!!"),
        MachineID: "mi-app",
        LightKDF:  false,
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
}
```

---

## Documentación completa

| Documento | Descripción |
|---|---|
| [ARQUITECTURA.md](doc/ARQUITECTURA.md) | Componentes, modelo de datos, concurrencia, ciclo de vida |
| [DISTRIBUCION_ARCHIVOS.md](doc/DISTRIBUCION_ARCHIVOS.md) | Mapa de archivos, líneas, responsabilidad de cada módulo |
| [FORMATO_ARCHIVO.md](doc/FORMATO_ARCHIVO.md) | Layout del `.mlstore`, jerarquía de claves, flush atómico |
| [API.md](doc/API.md) | API pública Go completa (CRUD, auth, hooks, triggers, grafo) |
| [CONSULTAS.md](doc/CONSULTAS.md) | Filtros, índices, planner, sort, Explain |
| [PRUEBAS.md](doc/PRUEBAS.md) | Suites de tests, cobertura, benchmarks |
| [RECREAR.md](doc/RECREAR.md) | Guía para extraer el motor a un módulo propio |
| [PLAN_V2.md](doc/PLAN_V2.md) | Bitácora del desarrollo v2 (M0→M8) |

---

## Casos de uso

- **CRM para eCommerce**: Gestión de pedidos, clientes y productos de Mercado Libre
- **Almacenamiento JSON nativo**: Sin mapeo ORM, directo a documento
- **Relaciones complejas**: Grafos para conexiones espaciales o redes de entidades
- **Aplicaciones embebidas**: Base de datos en un solo archivo cifrado
- **Prototipado rápido**: Esquemas flexibles con migraciones forward-only

---

## Requisitos

- Go 1.26+
- CGO solo para tests con `-race` (requiere GCC/MSYS2 en Windows)

---

## Instalación

```bash
go get mlstoredb/db
```

---

## Contribuciones

Las contribuciones son bienvenidas. Si encuentras bugs o tienes sugerencias, por favor abre un issue o envía un pull request.

---

## Licencia

Este proyecto está bajo la licencia MIT. Ver el archivo [LICENSE](LICENSE) para más detalles.
