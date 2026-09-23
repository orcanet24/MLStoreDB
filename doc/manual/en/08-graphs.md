# 08 — Graphs

Typed edges between documents, with in-RAM CSR adjacency (M6).

## Creating and removing edges

```go
// AddEdge(type, from, to, props) → edge _id (ULID)
id, _ := store.AddEdge("knows", "ana", "bob", db.Document{"since": 2024})
store.AddEdge("knows", "bob", "carla", nil)
store.AddEdge("knows", "ana", "carla", db.Document{"since": 2020})

store.RemoveEdge("knows", id)   // by edge _id
```

Each edge is a document in the `edges.<type>` collection:

```go
store.Find("edges.knows", db.Document{"since": db.Document{"$gte": 2020}}, nil)
```

## Neighbors

```go
friends, _ := store.Neighbors("knows", "ana", db.Outgoing) // ["bob", "carla"]
_, _      = store.Neighbors("knows", "bob",  db.Incoming)  // ["ana"]
all, _    := store.Neighbors("knows", "ana", db.Both)      // deduped union
```

## BFS traversal — `Traverse`

```go
nodes, _ := store.Traverse(db.TraverseOptions{
    EdgeType:  "knows",
    Start:     "ana",
    Direction: db.Outgoing, // default
    MaxDepth:  2,           // default 5, max 32
    MinDepth:  1,           // exclude the start node (depth 0)
    Limit:     100,         // default 10000, max 100000
})
for _, n := range nodes {
    fmt.Println(n.Vertex, "at", n.Depth, "hops")
}
// bob 1 · carla 1 · (depth-2 nodes if any)
```

BFS discovery order; the start node is included at depth 0.

## Shortest path — `ShortestPath`

```go
path, _ := store.ShortestPath("knows", "ana", "carla", 4)
// ["ana", "carla"]  (direct edge)

path, _ = store.ShortestPath("knows", "carla", "bob", 4)
// ["carla", "ana", "bob"]  (2 hops)
// nil when no path within maxDepth (default 6, max 32)
```

## Graph + RBAC

```go
sess, _ := store.Authenticate("ana", "secret")
sess.AddEdge("knows", "ana", "dave", nil)
sess.Neighbors("knows", "ana", db.Outgoing)
sess.Traverse(db.TraverseOptions{EdgeType: "knows", Start: "ana"})
sess.ShortestPath("knows", "ana", "dave", 3)
```

Permissions are checked against the `edges.<type>` collection.

## Full example: recommendation network

```go
// friends of friends of ana (exactly depth 2)
nodes, _ := store.Traverse(db.TraverseOptions{
    EdgeType: "knows", Start: "ana", MinDepth: 2, MaxDepth: 2,
})
for _, n := range nodes {
    fmt.Println("suggestion:", n.Vertex)
}

// how to connect? shortest path to each suggestion
for _, n := range nodes {
    if path, _ := store.ShortestPath("knows", "ana", n.Vertex, 3); path != nil {
        fmt.Printf("%s ← %v\n", n.Vertex, path)
    }
}
```

## Notes

- Multiple edges between the same pair are deduplicated in neighbors.
- The adjacency cache self-invalidates on any mutation of `edges.*`
  (insert/update/delete/AddEdge/RemoveEdge).
- Different edge types (`knows`, `bought`, `follows`…) are independent
  graphs.
