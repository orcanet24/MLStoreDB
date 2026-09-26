# 10 — Consola web de administración (M9)

`mls-server` puede servir una consola web completa (estilo phpMyAdmin +
Neo4j Browser) desde el propio binario — sin node, sin CDN, sin instalar
nada más.

## Arrancar

```bash
go run ./tools/mls-server -addr 127.0.0.1:28917 -web 127.0.0.1:28918 \
    -path datos.mlstore -key "mi-clave-maestra-de-32-bytes!!" -db midb
```

En Windows: **`iniciar-servidor.bat`** (arranca wire + web juntos, con
menú multi-BD en `databases\<nombre>\`).

Abre **http://127.0.0.1:28918** en el navegador.

## Primer uso

Si la BD no tiene usuarios, la consola pide crear el **usuario
administrador** (usuario ≥ 3 caracteres, contraseña ≥ 6): se crea el rol
`admin` (acceso total) y tu usuario, y quedas logueado. A partir de ahí,
login normal — todos los permisos del RBAC del motor se aplican por
pantalla (un usuario de solo lectura puede ver pero no tocar).

## Qué puedes hacer

| Sección | Funciones |
|---|---|
| **Panel** | Colecciones, total de documentos, tamaño del archivo, caché RAM |
| **Colecciones** | Crear, eliminar, explorar con paginación + filtro JSON + sort |
| **Documentos** | Vista dinámica: columnas configurables, búsqueda por valor, filtros guiados, paginación en la cabecera, visor JSON + editor con validación |
| **Índices** | Crear (campos + unique), listar, eliminar |
| **Importar / Exportar** | CSV (Excel, con BOM e inferencia de tipos) · JSON · NDJSON — con **preview** antes de importar y modo insert/upsert |
| **Triggers** | Crear/editar con builder (evento, colección, filtro, acciones JSON), activar/desactivar, eliminar |
| **Grafos** | Canvas interactivo (ver abajo) |

## Vista dinámica de documentos

La pestaña **Documentos** de cada colección es una tabla viva, pensada para
explorar registros sin tocar código:

- **Todas las columnas**: el esquema se descubre en el servidor con
  `GET /api/collections/{coll}/fields` (muestra de 2000 docs; el botón `⇅`
  lanza el escaneo completo) y se combina con los campos vistos en la página.
  El botón **Campos** abre el selector: marcar/desmarcar columnas, buscar
  campo, *Todos / Ninguno / Solo esta página* y añadir paths a mano
  (`item.sku`). La selección y los registros por página se guardan por
  colección en `localStorage`.
- **Búsqueda por valor**: escribe un valor y filtra en todos los campos o en
  el campo que elijas (regex insensible a mayúsculas; números y booleanos
  casan también como valor exacto).
- **Filtros guiados** (botón *Filtros*): condiciones
  `campo + operador + tipo + valor` — operadores `= ≠ > ≥ < ≤ contiene
  empieza por existe no existe está en lista no está en lista regex`, tipos
  `auto / texto / número / booleano / JSON / null` — más un **filtro JSON
  avanzado** para `$and`, `$or`, `$not`, etc. (aplicar con el botón o
  Ctrl+Enter).
- **Cabecera fija**: la barra de herramientas, los filtros, la paginación y
  la cabecera de columnas no se mueven; el scroll solo desplaza las filas
  (y la columna de acciones queda fija a la derecha). Clic en una cabecera
  para ordenar (asc/desc), doble clic en una fila para el JSON completo.
- **Paginación en la cabecera**: `« ‹ 1 / N › »` con salto a página y
  selector de **registros por página** (10/25/50/100/200) siempre visible.
- **Visor**: 🔍 o doble clic abre el documento completo con botón de copiar
  al portapapeles y acceso directo al editor.

## Canvas de grafos (estilo Neo4j)

- **Buscar** un `_id` → el nodo aparece centrado (esferas coloreadas por
  colección, aristas con flecha por tipo).
- **Doble clic** en un nodo → expande sus vecinos.
- **Shift + arrastrar** de un nodo a otro → crea una arista (elige tipo +
  propiedades).
- **Clic** en un nodo → panel lateral con el JSON del documento, sus
  aristas y acciones (expandir, conectar, eliminar).
- **Recorrer (BFS)** con profundidad configurable → resultados resaltados.
- **Ruta más corta** entre dos nodos → camino resaltado en verde.
- **Agrupar** → los nodos se agrupan en clusters por colección.
- Pan (arrastrar fondo), zoom (rueda), leyenda de colores, límite de 400
  nodos.

### Relaciones por campo (🔗 Relaciones)

Además de las aristas nativas `edges.*`, el grafo admite vínculos derivados
del **valor de un campo** de los documentos (no crean colecciones nuevas):

- **Por campo (1 o varias colecciones)**: filas de `colección.campo` (mínimo
  una); con **una sola colección agrupa** (p. ej. productos por marca,
  zonas, proveedores…) y con varias **cruza universos**; cada colección puede
  usar **su propio nombre de campo** (p. ej. `products.brand` ⇄ `marks.name`),
  no hace falta que se llamen igual.
- **Vincular campos (origen y destino)**: une `coleccionA.campo` con
  `coleccionB.campo` **solo si los tipos coinciden** (texto↔texto,
  número/float↔número); origen y destino pueden ser la **misma colección**
  (auto-relación, p. ej. `orders.parent` → `orders._id`) y el tipo se valida
  con `/fields` al guardar, mostrando el desajuste como error (p. ej.
  `orders.total=number` frente a `customers.code=string`).
- **Agrupación automática**: al crear una relación, al activarla y al abrir
  la vista de grafo, los documentos se agrupan solos según los valores
  actuales del campo (pagina `GET /docs` con 200 por página, máximo 5
  páginas por pareja); no hace falta buscar valores a mano. El alcance se
  limita a los 400 nodos del lienzo (si se alcanza, el aviso indica
  agrupación parcial).
- **Nodos-valor ▣ (cuadrados)**: cada valor distinto (Toyota, Corolla…) es
  un nodo **cuadrado** con el **color definido en la relación** (selector de
  color en el formulario y en la lista; por defecto uno de la paleta). Los
  registros que cuelgan de él siguen siendo **círculos** con el color de su
  colección, y sus aristas son **discontinuas** con el color de la relación.
- **🔍 Buscar valor** (en cada relación de la lista): siembra el grafo con
  todos los documentos cuyo campo coincide con el valor indicado (p. ej. un
  `customerId`), sin conocer ningún `_id`. El doble clic sobre un nodo-valor
  carga más documentos de ese valor.
- **Doble clic / Expandir** usa también las relaciones activas además de las
  aristas `edges.*` (aunque no exista ninguna colección `edges.*`).
  Desactivar o borrar una relación retira sus nodos-valor y aristas
  derivadas —todo en cliente, sin llamadas `DELETE` a la API—, y su nombre y
  color aparecen en la leyenda.
- **Rendimiento**: no hay servidor de grafo tipo Neo4j: las vinculaciones son
  igualdad de valores intrínseca al dominio (Toyota = Toyota), sin recorridos
  ni clústeres de aristas. Todo el cálculo es cliente: el lienzo se limita a
  400 nodos, las búsquedas por `_id` usan un índice en memoria O(1), la
  leyenda se actualiza una vez por frame durante cargas masivas y la
  simulación de fuerzas decae hasta detenerse, dejando la CPU en reposo
  mínima.
- Se persisten por BD en `localStorage` (`mls-graph-links:<bd>`), igual que
  las preferencias de la vista de documentos; solo afectan a este navegador.

## Seguridad

- Cookie de sesión HttpOnly + SameSite=Strict
- Token **CSRF** obligatorio en toda operación de escritura
- Rate-limit de login: 5 intentos fallidos por minuto por IP
- Headers: CSP, X-Frame-Options DENY, nosniff
- Bind `127.0.0.1` por defecto (para acceso remoto usa un reverse proxy)

## Usuarios y roles (M9d)

En **Administración**: crear/editar/eliminar usuarios (asignando roles) y
roles (matriz de permisos por colección: lectura, escritura y `fieldDeny`).
El cambio de roles de un usuario **preserva su contraseña** (`SetUserRoles`
in-place). Requiere un rol con permiso de escritura global (`*`).
