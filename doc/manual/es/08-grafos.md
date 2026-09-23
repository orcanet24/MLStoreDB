# 08 — Grafos

Aristas tipadas entre documentos, con adyacencia CSR en RAM (M6).

## Crear y borrar aristas

```go
// AddEdge(tipo, desde, hasta, props) → _id de la arista (ULID)
id, _ := store.AddEdge("knows", "ana", "bob", db.Document{"desde": 2024})
store.AddEdge("knows", "bob", "carla", nil)
store.AddEdge("knows", "ana", "carla", db.Document{"desde": 2020})

store.RemoveEdge("knows", id)   // por _id de arista
```

Cada arista es un documento en la colección `edges.<tipo>`:

```go
store.Find("edges.knows", db.Document{"desde": db.Document{"$gte": 2020}}, nil)
```

## Vecinos

```go
amigos, _ := store.Neighbors("knows", "ana", db.Outgoing) // ["bob", "carla"]
_, _     = store.Neighbors("knows", "bob",  db.Incoming)  // ["ana"]
todos, _ := store.Neighbors("knows", "ana", db.Both)      // unión dedupe
```

## Recorrido BFS — `Traverse`

```go
nodes, _ := store.Traverse(db.TraverseOptions{
    EdgeType:  "knows",
    Start:     "ana",
    Direction: db.Outgoing, // default
    MaxDepth:  2,           // default 5, máx 32
    MinDepth:  1,           // excluir el nodo inicial (depth 0)
    Limit:     100,         // default 10000, máx 100000
})
for _, n := range nodes {
    fmt.Println(n.Vertex, "a", n.Depth, "saltos")
}
// bob 1 · carla 1 · (nodos a depth 2 si los hubiera)
```

Orden de descubrimiento BFS; el inicio se incluye en depth 0.

## Ruta más corta — `ShortestPath`

```go
path, _ := store.ShortestPath("knows", "ana", "carla", 4)
// ["ana", "carla"]  (arista directa)

path, _ = store.ShortestPath("knows", "carla", "bob", 4)
// ["carla", "ana", "bob"]  (2 saltos)
// nil si no hay ruta dentro de maxDepth (default 6, máx 32)
```

## Grafo + RBAC

```go
sess, _ := store.Authenticate("ana", "secreto")
sess.AddEdge("knows", "ana", "dave", nil)
sess.Neighbors("knows", "ana", db.Outgoing)
sess.Traverse(db.TraverseOptions{EdgeType: "knows", Start: "ana"})
sess.ShortestPath("knows", "ana", "dave", 3)
```

Los permisos se chequean sobre la colección `edges.<tipo>`.

## Ejemplo completo: red de recomendación

```go
// amigos de amigos de ana (depth exactamente 2)
nodes, _ := store.Traverse(db.TraverseOptions{
    EdgeType: "knows", Start: "ana", MinDepth: 2, MaxDepth: 2,
})
for _, n := range nodes {
    fmt.Println("sugerencia:", n.Vertex)
}

// ¿por quién conectar? la ruta más corta hasta cada sugerencia
for _, n := range nodes {
    if path, _ := store.ShortestPath("knows", "ana", n.Vertex, 3); path != nil {
        fmt.Printf("%s ← %v\n", n.Vertex, path)
    }
}
```

## Notas

- Múltiples aristas entre el mismo par se deduplican en vecinos.
- La caché de adyacencia se invalida sola ante cualquier mutación de
  `edges.*` (insert/update/delete/AddEdge/RemoveEdge).
- Tipos de arista distintos (`knows`, `compro`, `sigue`…) son grafos
  independientes.
