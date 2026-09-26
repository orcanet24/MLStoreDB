# 10 — Web admin console (M9)

`mls-server` can serve a full web console (phpMyAdmin + Neo4j Browser
style) from the binary itself — no node, no CDN, nothing else to install.

## Starting it

```bash
go run ./tools/mls-server -addr 127.0.0.1:28917 -web 127.0.0.1:28918 \
    -path data.mlstore -key "my-32-bytes-long-master-key!!" -db mydb
```

On Windows: **`iniciar-servidor.bat`** (starts wire + web together, with
a multi-DB menu in `databases\<name>\`).

Open **http://127.0.0.1:28918** in your browser.

## First use

If the DB has no users, the console asks you to create the
**administrator user** (username ≥ 3 chars, password ≥ 6): it creates the
`admin` role (full access) plus your user, and you are logged in. From
then on it's a normal login — every engine RBAC permission applies per
screen (a read-only user can look but not touch).

## What you can do

| Section | Features |
|---|---|
| **Dashboard** | Collections, total documents, file size, RAM cache |
| **Collections** | Create, drop, browse with pagination + JSON filter + sort |
| **Documents** | Dynamic view: configurable columns, value search, guided filters, header pagination, JSON viewer + validated editor |
| **Indexes** | Create (fields + unique), list, drop |
| **Import / Export** | CSV (Excel, BOM + type inference) · JSON · NDJSON — with **preview** before importing and insert/upsert mode |
| **Triggers** | Create/edit with a builder (event, collection, filter, JSON actions), enable/disable, delete |
| **Graphs** | Interactive canvas (see below) |

## Dynamic document view

Each collection's **Documents** tab is a live table built for exploring
records without touching code:

- **All columns**: the schema is discovered server-side via
  `GET /api/collections/{coll}/fields` (sample of 2000 docs; the `⇅` button
  runs a full scan) and merged with the fields seen on the current page. The
  **Fields** button opens the column picker: check/uncheck columns, search a
  field, *All / None / Only this page* and add paths by hand (`item.sku`).
  The selection and the records-per-page are persisted per collection in
  `localStorage`.
- **Value search**: type a value to filter across all fields or a specific
  one (case-insensitive regex; numbers and booleans also match as an exact
  value).
- **Guided filters** (the *Filters* button): conditions of
  `field + operator + type + value` — operators `= ≠ > ≥ < ≤ contains starts
  with exists missing in list not in list regex`, types
  `auto / text / number / boolean / JSON / null` — plus an **advanced JSON
  filter** for `$and`, `$or`, `$not`, etc. (apply with the button or
  Ctrl+Enter).
- **Fixed header**: toolbar, filters, pagination and the column header row
  never move; scrolling only shifts the data rows (the actions column stays
  pinned to the right). Click a header to sort (asc/desc), double-click a row
  for the full JSON.
- **Header pagination**: `« ‹ 1 / N › »` with page jumping and a
  **records-per-page** selector (10/25/50/100/200) always visible.
- **Viewer**: 🔍 or double-click opens the full document with a copy-to-
  clipboard button and a shortcut to the editor.

## Graph canvas (Neo4j style)

- **Search** an `_id` → the node appears centered (spheres colored by
  collection, edges with arrowheads by type).
- **Double-click** a node → expands its neighbors.
- **Shift + drag** from one node to another → creates an edge (pick type
  + properties).
- **Click** a node → side panel with the document JSON, its edges and
  actions (expand, connect, delete).
- **Traverse (BFS)** with configurable depth → results highlighted.
- **Shortest path** between two nodes → path highlighted in green.
- **Group** → nodes cluster by collection.
- Pan (drag background), zoom (wheel), color legend, 400-node limit.

### Field-based relations (🔗 Relations)

Besides the native `edges.*` arcs, the graph supports links derived from the
**value of a document field** (they create no new collections):

- **By field (1 or more collections)**: rows of `collection.field` (at least
  one); with **a single collection it groups** (e.g. products by brand,
  zones, suppliers…) and with several it **joins universes**; each collection
  can use **its own field name** (e.g. `products.brand` ⇄ `marks.name`), they
  don't have to match.
- **Link fields (from and to)**: joins `collectionA.field` with
  `collectionB.field` **only if the types match** (string↔string,
  number/float↔number); from and to may be the **same collection**
  (self-relation, e.g. `orders.parent` → `orders._id`) and the type is
  validated through `/fields` on save, reporting the mismatch as an error
  (e.g. `orders.total=number` vs `customers.code=string`).
- **Automatic grouping**: when a relation is created, enabled or the graph
  view is opened, documents are grouped automatically from the current field
  values (pages `GET /docs` with 200 per page, at most 5 pages per pair); no
  manual value search is needed. Scope is capped by the 400-node canvas (the
  toast reports a partial grouping when the cap is hit).
- **Value nodes ▣ (squares)**: each distinct value (Toyota, Corolla…) is a
  **square** node with the **color set on the relation** (color picker in the
  form and the list; a palette color is picked by default). The records
  hanging from it remain **circles** colored by their collection, and their
  arcs are **dashed** in the relation color.
- **🔍 Search value** (on each relation in the list): seeds the graph with
  every document whose field equals the given value (e.g. a `customerId`),
  with no need to know any `_id`. Double-clicking a value node loads more
  documents for that value.
- **Double-click / Expand** also uses the active relations in addition to
  `edges.*` arcs (even when no `edges.*` collection exists). Disabling or
  deleting a relation removes its value nodes and derived arcs — all
  client-side, no `DELETE` API calls — and its name and color appear in the
  legend.
- **Performance**: there is no Neo4j-style graph server: links are plain
  value equality intrinsic to the domain (Toyota = Toyota), with no
  traversals or edge clustering. Everything runs client-side: the canvas is
  capped at 400 nodes, `_id` lookups use an in-memory O(1) index, the legend
  is refreshed once per frame during bulk loads, and the force simulation
  decays to a stop, keeping idle CPU minimal.
- They are persisted per database in `localStorage`
  (`mls-graph-links:<db>`), like the document view preferences; they only
  affect this browser.

## Security

- HttpOnly + SameSite=Strict session cookie
- Mandatory **CSRF** token on every write
- Login rate-limit: 5 failed attempts per minute per IP
- Headers: CSP, X-Frame-Options DENY, nosniff
- Binds 127.0.0.1 by default (use a reverse proxy for remote access)

## Users & roles (M9d)

Under **Administration**: create/edit/delete users (assigning roles) and
roles (per-collection permission matrix: read, write and `fieldDeny`).
Changing a user's roles **preserves the password** (`SetUserRoles`
in-place). Requires a role with global write permission (`*`).
