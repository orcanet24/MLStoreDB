(function() {
"use strict";

/* ============ helpers ============ */
const $ = (s, el) => (el || document).querySelector(s);
const $$ = (s, el) => Array.from((el || document).querySelectorAll(s));
function esc(s) { return String(s == null ? "" : s).replace(/[&<>"']/g, c => ({ "&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;" }[c])); }
function fmtVal(v) { if (v == null) return "null"; if (typeof v === "object") { try { return JSON.stringify(v); } catch (e) { return String(v); } } return String(v); }

/* ============ i18n ============ */
const I18N = {
  es: {
    "nav.dashboard": "Panel", "nav.collections": "Colecciones", "nav.graph": "Grafos",
    "nav.console": "Consola", "nav.admin": "Administración", "nav.logout": "Salir",
    "login.db": "Base de datos", "login.user": "Usuario", "login.pass": "Contraseña",
    "login.enter": "Entrar", "login.setupHint": "BD sin proteger. Crea el administrador:",
    "login.setupBtn": "Crear administrador", "login.newDb": "+ Crear nueva BD",
    "login.ndName": "Nombre de la BD", "login.ndUser": "Usuario administrador",
    "login.ndPass": "Contraseña", "login.ndBtn": "Crear BD y admin",
    "login.errFields": "Todos los campos obligatorios (clave mín. 6)",
    "login.noDbs": "No hay bases de datos. Crea la primera:",
    "login.select": "-- Seleccionar --",
    "dash.title": "Panel", "dash.sub": "Estado general", "dash.collections": "Colecciones",
    "dash.docs": "Documentos", "dash.file": "Archivo", "dash.cache": "Caché",
    "dash.newColl": "+ Nueva colección", "dash.name": "Nombre", "dash.docsCol": "Docs", "dash.open": "Abrir",
    "dash.empty": "Sin colecciones.", "dash.resident": "residente / presupuesto",
    "colls.title": "Colecciones", "colls.new": "+ Nueva colección", "colls.name": "Nombre",
    "colls.docs": "Docs", "colls.idxs": "Índices", "colls.open": "Abrir", "colls.empty": "Sin colecciones.",
    "coll.empty": "Colección vacía.", "coll.drop": "Eliminar colección",
    "coll.dropConfirm": "¿Eliminar la colección y TODOS sus documentos?",
    "coll.created": "Colección creada",
    "tabs.docs": "Documentos", "tabs.indexes": "Índices", "tabs.io": "Importar/Exportar", "tabs.triggers": "Triggers",
    "docs.filter": "Filtro JSON", "docs.apply": "Aplicar", "docs.clear": "Limpiar",
    "docs.searchPh": "Buscar por valor en cualquier campo…",
    "docs.anyField": "(todos los campos)",
    "docs.advanced": "Filtros",
    "docs.where": "donde",
    "docs.addCond": "+ Condición",
    "docs.clearConds": "Quitar condiciones",
    "docs.jsonFilter": "Filtro JSON (avanzado)",
    "docs.value": "valor",
    "docs.type": "Tipo",
    "docs.columns": "Campos",
    "docs.discover": "Descubrir campos",
    "docs.discovering": "Descubriendo campos…",
    "docs.scanAll": "Escaneo completo",
    "docs.scanDone": "Campos actualizados",
    "docs.fieldsInfo": "{n} campos · {s} docs escaneados",
    "docs.fieldsPartial": "(parcial)",
    "docs.searchFieldPh": "Buscar campo…",
    "docs.all": "Todos",
    "docs.none": "Ninguno",
    "docs.onlyPage": "Solo esta página",
    "docs.addField": "Añadir campo (path)",
    "docs.colsHint": "Marca los campos visibles. Se guarda por colección.",
    "docs.searchLimit": "La búsqueda rápida cubre los primeros {n} campos.",
    "docs.perPage": "por página",
    "docs.refresh": "Recargar",
    "docs.view": "Ver documento",
    "docs.copy": "Copiar JSON",
    "docs.copied": "JSON copiado",
    "docs.noResults": "Sin resultados.",
    "docs.clearAll": "Limpiar filtros",
    "docs.badJson": "Filtro JSON inválido:",
  "docs.badValue": "Valor inválido:",
  "docs.filterHint": "Añade condiciones para filtrar registros.",
    "docs.needFields": "Aún no hay campos conocidos: pulsa «Descubrir campos».",
    "docs.hint": "Clic en la cabecera: ordenar · doble clic en la fila: JSON completo del documento.",
    "pg.first": "Primera página", "pg.prev": "Anterior", "pg.next": "Siguiente", "pg.last": "Última",
    "pg.range": "{a}–{b} de {n}", "pg.page": "Página",
    "op.eq": "= igual", "op.ne": "≠ distinto", "op.gt": "> mayor", "op.gte": "≥ mayor o igual",
    "op.lt": "< menor", "op.lte": "≤ menor o igual", "op.contains": "contiene", "op.starts": "empieza por",
    "op.exists": "existe", "op.nexists": "no existe", "op.in": "está en lista", "op.nin": "no está en lista",
    "op.regex": "regex",
    "type.auto": "auto", "type.text": "texto", "type.number": "número", "type.bool": "booleano",
    "type.json": "JSON", "type.null": "null",
    "docs.newDoc": "+ Nuevo documento", "docs.newColl": "+ Nueva colección",
    "docs.edit": "Editar", "docs.delete": "Eliminar", "docs.delConfirm": "¿Eliminar el documento?",
    "docs.saved": "Guardado", "docs.deleted": "Eliminado",
    "editor.newTitle": "Nuevo documento", "editor.editTitle": "Editar documento",
    "editor.json": "JSON", "editor.save": "Guardar", "editor.cancel": "Cancelar",
    "editor.delete": "Eliminar", "editor.invalid": "JSON inválido: ",
    "editor.mustObject": "Se espera un objeto JSON",
    "idx.fields": "Campos (separados por coma)", "idx.unique": "Único",
    "idx.create": "Crear índice", "idx.empty": "Sin índices.", "idx.name": "Índice",
    "idx.fieldsCol": "Campos", "idx.uniqueCol": "Único", "idx.actions": "Acciones",
    "idx.dropConfirm": "¿Eliminar este índice?", "idx.fieldsReq": "Indica al menos un campo",
    "io.exportTitle": "Exportar colección", "io.format": "Formato",
    "io.download": "Descargar", "io.importTitle": "Importar a la colección",
    "io.file": "Archivo (csv / json / ndjson)", "io.mode": "Modo",
    "io.insert": "insert (falla si duplicado)", "io.upsert": "upsert (reemplaza por _id)",
    "io.importBtn": "Importar", "io.pickFile": "Selecciona un archivo",
    "io.running": "Importando…", "io.done": "Importación terminada", "io.error": "Error de importación",
    "io.processed": "Procesados", "io.inserted": "Insertados", "io.errors": "Errores",
    "trig.title": "Triggers", "trig.new": "+ Nuevo trigger", "trig.empty": "Sin triggers.",
    "trig.event": "Evento", "trig.coll": "Colección (vacío = todas)",
    "trig.filter": "Filtro JSON (opcional)", "trig.actions": "Acciones (JSON)",
    "trig.async": "Asíncrono (solo after_*)", "trig.id": "ID (opcional, se autogenera)",
    "trig.on": "activo", "trig.off": "inactivo", "trig.toggle": "Activar/Desactivar",
    "trig.delConfirm": "¿Eliminar este trigger?", "trig.invalidActions": "Acciones inválidas: ",
    "trig.allColls": "todas",
    "con.title": "Consola", "con.coll": "Colección", "con.filter": "Filtro JSON",
    "con.limit": "Límite", "con.run": "Ejecutar", "con.total": "documentos",
    "con.empty": "Sin resultados.",
    "graph.nav": "Grafos",
    "admin.title": "Administración", "admin.users": "Usuarios", "admin.roles": "Roles",
    "admin.username": "Usuario", "admin.password": "Contraseña", "admin.rolesOf": "Roles",
    "admin.newUser": "+ Nuevo usuario", "admin.newRole": "+ Nuevo rol",
    "admin.delUser": "¿Eliminar el usuario?", "admin.delRole": "¿Eliminar el rol?",
    "admin.emptyUsers": "Sin usuarios.", "admin.emptyRoles": "Sin roles.",
    "admin.name": "Nombre", "admin.perms": "Permisos", "admin.addPerm": "+ Permiso",
    "admin.permColl": "Colección (* = todas)", "admin.read": "Lectura", "admin.write": "Escritura",
    "admin.fieldDeny": "Campos denegados (coma, opcional)",
    "admin.noAdmin": "Requiere un rol con permiso de escritura global.",
    "common.cancel": "Cancelar", "common.ok": "OK", "common.confirm": "Confirmar",
    "common.close": "Cerrar", "common.actions": "Acciones", "common.loading": "Cargando…",
    "common.saved": "Guardado", "common.deleted": "Eliminado", "common.error": "Error",
    "graph.title": "Canvas de grafos", "graph.sub": "datos conectados",
    "graph.searchPh": "Buscar vértice por _id…", "graph.add": "Añadir",
    "graph.traverse": "Traverse", "graph.path": "Ruta más corta", "graph.group": "Agrupar",
    "graph.clear": "Limpiar", "graph.dragHint": "doble clic: expandir · Shift+arrastrar: conectar · rueda: zoom",
    "graph.nodes": "nodos", "graph.edges": "aristas", "graph.empty": "Busca un vértice para empezar.",
    "graph.sideHint": "Selecciona un nodo para ver detalles.", "graph.expand": "Expandir",
    "graph.connect": "Conectar", "graph.connectTo": "Haz clic en el nodo destino",
    "graph.deleteDoc": "Eliminar documento", "graph.notFound": "No encontrado",
    "graph.limitReached": "Límite de nodos alcanzado", "graph.noTypes": "No hay tipos de arista",
    "graph.selectFirst": "Selecciona un nodo primero", "graph.depth": "Profundidad",
    "graph.pickTo": "Haz clic en el nodo destino", "graph.noPath": "Sin ruta",
    "graph.newEdge": "Nueva arista", "graph.nodeType": "Tipo de arista",
    "graph.props": "Propiedades (JSON, opcional)",
    "graph.links": "Relaciones",
    "graph.linksHint": "Agrupa o conecta documentos por el valor de un campo, sin colecciones edges.*: 1 sola colección agrupa (marca, zona…), varias cruzan universos y cada una puede usar su propio campo. Se guardan por BD en este navegador.",
    "graph.linkMode": "Modo de relación",
    "graph.linkPivot": "Por campo (1 o varias colecciones)",
    "graph.linkManual": "Vincular campos (origen y destino)",
    "graph.linkColls": "Colecciones y campos (mínimo 1)",
    "graph.linkField": "Campo",
    "graph.linkLabel": "Nombre (opcional)",
    "graph.linkFrom": "Origen (colección + campo)",
    "graph.linkTo": "Destino (colección + campo)",
    "graph.linkEmpty": "Sin relaciones todavía.",
    "graph.linkAdd": "Añadir",
    "graph.linkAdded": "Relación añadida",
    "graph.linkDeleted": "Relación eliminada",
    "graph.linkEnable": "Activar/desactivar",
    "graph.linkDel": "Eliminar relación",
    "graph.linkNeedColls": "Añade al menos una colección con su campo.",
    "graph.linkFieldReq": "Indica un campo válido.",
    "graph.linkMissing": "Campo no encontrado en: ",
    "graph.linkTypeErr": "Los campos deben ser del mismo tipo (texto o número). Detalle: ",
    "graph.linkSeed": "Buscar valor",
    "graph.linkSeedPh": "Valor del campo",
    "graph.linkSeedEmpty": "Sin coincidencias",
    "graph.linkSeedVal": "Indica un valor.",
    "graph.linkBadNum": "El valor debe ser numérico.",
    "graph.linkAddColl": "Añadir colección",
    "graph.linkDelColl": "Quitar colección",
    "graph.linkColor": "Color de grupo",
    "graph.linkGrouped": "Agrupados: ",
    "graph.linkGroupTrunc": "Límite de nodos alcanzado; agrupación parcial.",
    "graph.linkGroupNone": "Sin documentos para agrupar.",
    "graph.linkValCount": "valores"
  },
  en: {
    "nav.dashboard": "Dashboard", "nav.collections": "Collections", "nav.graph": "Graph",
    "nav.console": "Console", "nav.admin": "Administration", "nav.logout": "Sign out",
    "login.db": "Database", "login.user": "User", "login.pass": "Password",
    "login.enter": "Sign in", "login.setupHint": "Unprotected DB. Create the administrator:",
    "login.setupBtn": "Create administrator", "login.newDb": "+ Create new DB",
    "login.ndName": "Database name", "login.ndUser": "Administrator user",
    "login.ndPass": "Password", "login.ndBtn": "Create DB and admin",
    "login.errFields": "All fields required (password min 6)",
    "login.noDbs": "No databases yet. Create the first one:",
    "login.select": "-- Select --",
    "dash.title": "Dashboard", "dash.sub": "Overview", "dash.collections": "Collections",
    "dash.docs": "Documents", "dash.file": "File", "dash.cache": "Cache",
    "dash.newColl": "+ New collection", "dash.name": "Name", "dash.docsCol": "Docs", "dash.open": "Open",
    "dash.empty": "No collections.", "dash.resident": "resident / budget",
    "colls.title": "Collections", "colls.new": "+ New collection", "colls.name": "Name",
    "colls.docs": "Docs", "colls.idxs": "Indexes", "colls.open": "Open", "colls.empty": "No collections.",
    "coll.empty": "Empty collection.", "coll.drop": "Drop collection",
    "coll.dropConfirm": "Drop the collection and ALL its documents?",
    "coll.created": "Collection created",
    "tabs.docs": "Documents", "tabs.indexes": "Indexes", "tabs.io": "Import/Export", "tabs.triggers": "Triggers",
    "docs.filter": "JSON filter", "docs.apply": "Apply", "docs.clear": "Clear",
    "docs.searchPh": "Search any field by value…",
    "docs.anyField": "(all fields)",
    "docs.advanced": "Filters",
    "docs.where": "where",
    "docs.addCond": "+ Condition",
    "docs.clearConds": "Remove conditions",
    "docs.jsonFilter": "JSON filter (advanced)",
    "docs.value": "value",
    "docs.type": "Type",
    "docs.columns": "Fields",
    "docs.discover": "Discover fields",
    "docs.discovering": "Discovering fields…",
    "docs.scanAll": "Full scan",
    "docs.scanDone": "Fields refreshed",
    "docs.fieldsInfo": "{n} fields · {s} docs scanned",
    "docs.fieldsPartial": "(partial)",
    "docs.searchFieldPh": "Search field…",
    "docs.all": "All",
    "docs.none": "None",
    "docs.onlyPage": "Only this page",
    "docs.addField": "Add field (path)",
    "docs.colsHint": "Check the visible fields. Saved per collection.",
    "docs.searchLimit": "Quick search covers the first {n} fields.",
    "docs.perPage": "per page",
    "docs.refresh": "Reload",
    "docs.view": "View document",
    "docs.copy": "Copy JSON",
    "docs.copied": "JSON copied",
    "docs.noResults": "No results.",
    "docs.clearAll": "Clear filters",
    "docs.badJson": "Invalid JSON filter:",
    "docs.badValue": "Invalid value:",
    "docs.filterHint": "Add conditions to filter records.",
    "docs.needFields": "No known fields yet: click \"Discover fields\".",
    "docs.hint": "Click a header to sort · double click a row for the full document JSON.",
    "pg.first": "First page", "pg.prev": "Previous", "pg.next": "Next", "pg.last": "Last",
    "pg.range": "{a}–{b} of {n}", "pg.page": "Page",
    "op.eq": "= equals", "op.ne": "≠ not equal", "op.gt": "> greater", "op.gte": "≥ greater or equal",
    "op.lt": "< less", "op.lte": "≤ less or equal", "op.contains": "contains", "op.starts": "starts with",
    "op.exists": "exists", "op.nexists": "missing", "op.in": "in list", "op.nin": "not in list",
    "op.regex": "regex",
    "type.auto": "auto", "type.text": "text", "type.number": "number", "type.bool": "boolean",
    "type.json": "JSON", "type.null": "null",
    "docs.newDoc": "+ New document", "docs.newColl": "+ New collection",
    "docs.edit": "Edit", "docs.delete": "Delete", "docs.delConfirm": "Delete this document?",
    "docs.saved": "Saved", "docs.deleted": "Deleted",
    "editor.newTitle": "New document", "editor.editTitle": "Edit document",
    "editor.json": "JSON", "editor.save": "Save", "editor.cancel": "Cancel",
    "editor.delete": "Delete", "editor.invalid": "Invalid JSON: ",
    "editor.mustObject": "A JSON object is expected",
    "idx.fields": "Fields (comma separated)", "idx.unique": "Unique",
    "idx.create": "Create index", "idx.empty": "No indexes.", "idx.name": "Index",
    "idx.fieldsCol": "Fields", "idx.uniqueCol": "Unique", "idx.actions": "Actions",
    "idx.dropConfirm": "Drop this index?", "idx.fieldsReq": "Provide at least one field",
    "io.exportTitle": "Export collection", "io.format": "Format",
    "io.download": "Download", "io.importTitle": "Import into collection",
    "io.file": "File (csv / json / ndjson)", "io.mode": "Mode",
    "io.insert": "insert (fails on duplicate)", "io.upsert": "upsert (replaces by _id)",
    "io.importBtn": "Import", "io.pickFile": "Pick a file",
    "io.running": "Importing…", "io.done": "Import finished", "io.error": "Import error",
    "io.processed": "Processed", "io.inserted": "Inserted", "io.errors": "Errors",
    "trig.title": "Triggers", "trig.new": "+ New trigger", "trig.empty": "No triggers.",
    "trig.event": "Event", "trig.coll": "Collection (empty = all)",
    "trig.filter": "JSON filter (optional)", "trig.actions": "Actions (JSON)",
    "trig.async": "Async (after_* only)", "trig.id": "ID (optional, auto-generated)",
    "trig.on": "on", "trig.off": "off", "trig.toggle": "Enable/Disable",
    "trig.delConfirm": "Delete this trigger?", "trig.invalidActions": "Invalid actions: ",
    "trig.allColls": "all",
    "con.title": "Console", "con.coll": "Collection", "con.filter": "JSON filter",
    "con.limit": "Limit", "con.run": "Run", "con.total": "documents",
    "con.empty": "No results.",
    "graph.nav": "Graph",
    "admin.title": "Administration", "admin.users": "Users", "admin.roles": "Roles",
    "admin.username": "User", "admin.password": "Password", "admin.rolesOf": "Roles",
    "admin.newUser": "+ New user", "admin.newRole": "+ New role",
    "admin.delUser": "Delete this user?", "admin.delRole": "Delete this role?",
    "admin.emptyUsers": "No users.", "admin.emptyRoles": "No roles.",
    "admin.name": "Name", "admin.perms": "Permissions", "admin.addPerm": "+ Permission",
    "admin.permColl": "Collection (* = all)", "admin.read": "Read", "admin.write": "Write",
    "admin.fieldDeny": "Denied fields (comma, optional)",
    "admin.noAdmin": "Requires a role with global write permission.",
    "common.cancel": "Cancel", "common.ok": "OK", "common.confirm": "Confirm",
    "common.close": "Close", "common.actions": "Actions", "common.loading": "Loading…",
    "common.saved": "Saved", "common.deleted": "Deleted", "common.error": "Error",
    "graph.title": "Graph canvas", "graph.sub": "connected data",
    "graph.searchPh": "Search vertex by _id…", "graph.add": "Add",
    "graph.traverse": "Traverse", "graph.path": "Shortest path", "graph.group": "Group",
    "graph.clear": "Clear", "graph.dragHint": "double click: expand · Shift+drag: connect · wheel: zoom",
    "graph.nodes": "nodes", "graph.edges": "edges", "graph.empty": "Search a vertex to begin.",
    "graph.sideHint": "Select a node to see details.", "graph.expand": "Expand",
    "graph.connect": "Connect", "graph.connectTo": "Click the target node",
    "graph.deleteDoc": "Delete document", "graph.notFound": "Not found",
    "graph.limitReached": "Node limit reached", "graph.noTypes": "No edge types",
    "graph.selectFirst": "Select a node first", "graph.depth": "Depth",
    "graph.pickTo": "Click the target node", "graph.noPath": "No path",
    "graph.newEdge": "New edge", "graph.nodeType": "Edge type",
    "graph.props": "Properties (JSON, optional)",
    "graph.links": "Relations",
    "graph.linksHint": "Group or connect documents by field value, no edges.* collections needed: 1 collection groups (brand, zone…), several join universes and each can use its own field. Saved per database in this browser.",
    "graph.linkMode": "Relation mode",
    "graph.linkPivot": "By field (1 or more collections)",
    "graph.linkManual": "Link fields (from and to)",
    "graph.linkColls": "Collections and fields (min 1)",
    "graph.linkField": "Field",
    "graph.linkLabel": "Name (optional)",
    "graph.linkFrom": "From (collection + field)",
    "graph.linkTo": "To (collection + field)",
    "graph.linkEmpty": "No relations yet.",
    "graph.linkAdd": "Add",
    "graph.linkAdded": "Relation added",
    "graph.linkDeleted": "Relation deleted",
    "graph.linkEnable": "Enable/disable",
    "graph.linkDel": "Delete relation",
    "graph.linkNeedColls": "Add at least one collection with its field.",
    "graph.linkFieldReq": "Provide a valid field.",
    "graph.linkMissing": "Field not found in: ",
    "graph.linkTypeErr": "Fields must share the same type (string or number). Details: ",
    "graph.linkSeed": "Search value",
    "graph.linkSeedPh": "Field value",
    "graph.linkSeedEmpty": "No matches",
    "graph.linkSeedVal": "Enter a value.",
    "graph.linkBadNum": "The value must be numeric.",
    "graph.linkAddColl": "Add collection",
    "graph.linkDelColl": "Remove collection",
    "graph.linkColor": "Group color",
    "graph.linkGrouped": "Grouped: ",
    "graph.linkGroupTrunc": "Node limit reached; partial grouping.",
    "graph.linkGroupNone": "No documents to group.",
    "graph.linkValCount": "values"
  }
};
let lang = localStorage.getItem("mls-lang");
if (lang !== "es" && lang !== "en") {
  lang = (navigator.language || "").toLowerCase().indexOf("en") === 0 ? "en" : "es";
}
function t(key) {
  let v = I18N[lang][key];
  if (v === undefined) v = I18N.es[key];
  return v === undefined ? key : v;
}

/* ============ state + api ============ */
const state = {
  me: null, csrf: "", db: "", colls: [], docs: [], total: 0, skip: 0, limit: 25,
  filter: "", sort: "", dir: 1, coll: null, tab: "docs",
  // Vista dinámica de documentos (pestaña "docs" de una colección):
  // q/qf = búsqueda por valor, conds = condiciones guiadas, fields/paths = esquema
  // descubierto (/fields), pageKeys = claves vistas en las páginas cargadas,
  // hidden/extra = selección de columnas persistida por colección.
  q: "", qf: "", conds: [], pageKeys: [], fields: [], paths: [], fieldTypes: {},
  fieldsScanned: 0, fieldsTruncated: false, hidden: new Set(), extra: [], colQuery: ""
};

// resetDocsView limpia la vista de documentos (al cambiar de BD o de colección).
function resetDocsView() {
  state.q = ""; state.qf = ""; state.conds = []; state.pageKeys = [];
  state.fields = []; state.paths = []; state.fieldTypes = {};
  state.fieldsScanned = 0; state.fieldsTruncated = false;
  state.hidden = new Set(); state.extra = []; state.colQuery = "";
  state.filter = ""; state.sort = ""; state.dir = 1; state.skip = 0;
}

function apiUrl(path) {
  if (path.startsWith("/api/") && state.db && path !== "/api/databases" && path !== "/api/me") {
    return "/api/db/" + encodeURIComponent(state.db) + path.slice(4);
  }
  return path;
}

async function api(path, opts) {
  opts = opts || {};
  const o = { method: opts.method || "GET", headers: {} };
  if (opts.body !== undefined) {
    if (opts.body instanceof Blob || opts.body instanceof File) { o.body = opts.body; }
    else { o.headers["Content-Type"] = "application/json"; o.body = JSON.stringify(opts.body); }
  }
  if (o.method !== "GET" && o.method !== "HEAD") o.headers["X-CSRF-Token"] = state.csrf;
  const r = await fetch(apiUrl(path), o);
  const d = await r.json().catch(() => ({}));
  if (r.status === 401) { showLogin(); throw new Error(d.error || "unauthorized"); }
  if (!r.ok) throw new Error(d.error || r.statusText);
  return d;
}

/* ============ i18n static labels ============ */
function applyI18n() {
  $$("[data-i18n]").forEach(el => { el.textContent = t(el.getAttribute("data-i18n")); });
  $$("[data-i18n-ph]").forEach(el => { el.placeholder = t(el.getAttribute("data-i18n-ph")); });
  $("#lang-btn").textContent = lang === "es" ? "EN" : "ES";
  document.title = "MLS Admin — " + (state.me ? t("dash.title") : t("login.enter"));
}

/* ============ boot / auth ============ */
window.addEventListener("hashchange", route);
$("#login-form").addEventListener("submit", doLogin);
$("#logout-btn").addEventListener("click", async () => { try { await api("/api/logout", { method: "POST" }); } catch (e) {} showLogin(); });
$("#lang-btn").addEventListener("click", () => {
  lang = lang === "es" ? "en" : "es";
  localStorage.setItem("mls-lang", lang);
  applyI18n();
  if (state.me) route(); else showLogin();
});
$("#btn-new-db").addEventListener("click", () => { $("#login-form").classList.add("hidden"); $("#new-db-form").classList.remove("hidden"); });
$("#setup-btn").addEventListener("click", async () => {
  const user = $("#setup-user").value.trim(), pass = $("#setup-pass").value;
  if (!user || pass.length < 6) { alert(t("login.errFields")); return; }
  try { await api("/api/setup", { method: "POST", body: { user, pass } }); await boot(); }
  catch (err) { alert(err.message); }
});
$("#new-db-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("#nd-name").value.trim(), user = $("#nd-user").value.trim(), pass = $("#nd-pass").value;
  if (!name || !user || pass.length < 6) { $("#nd-error").textContent = t("login.errFields"); return; }
  try {
    await api("/api/databases", { method: "POST", body: { name, user, password: pass } });
    $("#nd-error").textContent = "";
    state.db = name;
    await boot();
  } catch (err) { $("#nd-error").textContent = err.message; }
});

async function listDBs() {
  try {
    const r = await api("/api/databases");
    return r.databases || [];
  } catch (e) {
    return null; // single-DB server (no multi handler)
  }
}

async function boot() {
  // multi-DB: restore last session (cookie belongs to the per-DB sub-server)
  const lastDb = localStorage.getItem("mls-db");
  if (lastDb) {
    const r = await api("/api/db/" + encodeURIComponent(lastDb) + "/me").catch(() => null);
    if (r && r.user) { state.me = { user: r.user, roles: r.roles || [] }; state.csrf = r.csrf; state.db = lastDb; enterApp(); return; }
  }
  const r = await api("/api/me").catch(() => null);
  if (r && r.user) { state.me = { user: r.user, roles: r.roles || [] }; state.csrf = r.csrf; enterApp(); return; }
  showLogin();
}

function showLogin() {
  state.me = null; state.db = ""; state.csrf = "";
  $("#app-view").classList.add("hidden");
  $("#login-view").classList.remove("hidden");
  $("#login-error").textContent = "";
  $("#login-form").classList.remove("hidden");
  $("#new-db-form").classList.add("hidden");
  applyI18n();
  listDBs().then(dbs => {
    if (dbs === null) { refreshLogin(""); return; } // single-DB mode
    const sel = $("#db-select");
    sel.innerHTML = '<option value="">' + esc(t("login.select")) + "</option>";
    (dbs || []).forEach(d => { const o = document.createElement("option"); o.value = d.name; o.textContent = d.name + (d.authActive ? " 🔒" : ""); sel.appendChild(o); });
    if (!dbs.length) {
      $("#login-form").classList.add("hidden");
      $("#new-db-form").classList.remove("hidden");
      return;
    }
    if (dbs.length === 1) { sel.value = dbs[0].name; refreshLogin(dbs[0].name); }
    sel.addEventListener("change", () => refreshLogin(sel.value));
  }).catch(err => { $("#login-error").textContent = err.message; });
}

function refreshLogin(db) {
  state.db = db || "";
  if (!state.db) { $("#setup-form").classList.add("hidden"); return; }
  api("/api/status").then(st => {
    if (st.authActive) { $("#login-form").classList.remove("hidden"); $("#setup-form").classList.add("hidden"); }
    else { $("#login-form").classList.add("hidden"); $("#setup-form").classList.remove("hidden"); $("#setup-text").textContent = t("login.setupHint"); }
  }).catch(() => {});
}

async function doLogin(e) {
  e.preventDefault();
  const user = $("#login-user").value.trim(), pass = $("#login-pass").value;
  try {
    const r = await api("/api/login", { method: "POST", body: { user, pass } });
    state.me = { user: r.user, roles: r.roles || [] };
    state.csrf = r.csrf;
    enterApp();
  } catch (err) { $("#login-error").textContent = err.message; }
}

/* ============ shell ============ */
function isAdmin() { return !!(state.me && state.me.roles && state.me.roles.indexOf("admin") >= 0); }

async function enterApp() {
  $("#login-view").classList.add("hidden");
  $("#app-view").classList.remove("hidden");
  $("#user-name").textContent = state.me.user;
  $("#nav-admin").classList.toggle("hidden", !isAdmin());
  const dbs = await listDBs();
  const sel = $("#db-select-app");
  sel.innerHTML = "";
  if (dbs && dbs.length) {
    dbs.forEach(d => { const o = document.createElement("option"); o.value = d.name; o.textContent = d.name; if (d.name === state.db) o.selected = true; sel.appendChild(o); });
    if (!state.db) state.db = dbs[0].name;
    sel.value = state.db;
  } else {
    const o = document.createElement("option"); o.value = ""; o.textContent = state.db || "default"; sel.appendChild(o);
  }
  sel.onchange = () => { state.db = sel.value; resetDocsView(); localStorage.setItem("mls-db", state.db); refreshCols(); route(); };
  if (state.db) localStorage.setItem("mls-db", state.db);
  applyI18n();
  refreshCols();
  route();
}

async function refreshCols() {
  if (!state.db && $("#db-select-app").value === "" && !state.me) return;
  let r;
  try { r = await api("/api/collections"); } catch (e) { return; }
  state.colls = r.collections || [];
  const box = $("#coll-list");
  const q = ($("#coll-search").value || "").toLowerCase();
  const list = state.colls.filter(c => c.name.toLowerCase().indexOf(q) >= 0);
  box.innerHTML = list.length
    ? list.map(c => '<a href="#/coll/' + encodeURIComponent(c.name) + '" data-coll="' + esc(c.name) + '">' + esc(c.name) + " <b>" + c.count + "</b></a>").join("")
    : '<div class="gs-empty">—</div>';
  $("#coll-count").textContent = state.colls.length;
}

/* ============ router ============ */
function route() {
  if (!state.me) return;
  const h = location.hash || "#/dashboard";
  const p = h.slice(2).split("/").map(decodeURIComponent);
  // La pestaña "documentos" usa layout de altura completa: cabecera fija y scroll solo en los datos.
  $("#main").classList.toggle("fill", p[0] === "coll" && (p[2] || "docs") === "docs");
  $$(".sidebar nav > a, .sidebar nav .coll-toggle").forEach(a => a.classList.remove("active"));
  if (p[0] === "coll" && p[1]) { viewColl(p[1], p[2] || "docs"); }
  else if (p[0] === "collections") { setActive('a[href="#/collections"]'); viewCollections(); }
  else if (p[0] === "graph") { setActive('a[href="#/graph"]'); viewGraphRoute(); }
  else if (p[0] === "console") { setActive('a[href="#/console"]'); viewConsole(); }
  else if (p[0] === "admin") { setActive('a[href="#/admin"]'); viewAdmin(); }
  else { setActive('a[href="#/dashboard"]'); viewDashboard(); }
}
function setActive(sel) { const el = $(sel); if (el) el.classList.add("active"); }

/* ============ modal / toast ============ */
function openModal(html) { $("#modal").innerHTML = html; $("#modal-backdrop").classList.remove("hidden"); }
function closeModal() { $("#modal-backdrop").classList.add("hidden"); $("#modal").innerHTML = ""; }
$("#modal-backdrop").addEventListener("click", (e) => { if (e.target === $("#modal-backdrop")) closeModal(); });

function confirmModal(title, message, onOk) {
  openModal(
    "<h2>" + esc(title) + "</h2><p>" + esc(message) + "</p>" +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + '</button>' +
    '<button class="btn danger" id="m-ok">' + esc(t("common.confirm")) + "</button></div>"
  );
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-ok").addEventListener("click", async () => { closeModal(); await onOk(); });
}

function toast(m, e) {
  const el = document.createElement("div");
  el.className = "toast" + (e ? " error" : "");
  el.textContent = m;
  $("#toast-area").appendChild(el);
  setTimeout(() => el.remove(), 3200);
}

/* ============ dashboard ============ */
async function viewDashboard() {
  state.coll = null;
  const m = $("#main");
  m.innerHTML = '<p class="page-sub">' + esc(t("common.loading")) + "</p>";
  let st;
  try { st = await api("/api/stats"); } catch (e) { m.innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  m.innerHTML =
    "<h1>" + esc(t("dash.title")) + '</h1><p class="page-sub">' + esc(t("dash.sub")) + "</p>" +
    '<div class="cards">' +
      '<div class="card"><div class="k">' + esc(t("dash.collections")) + '</div><div class="v">' + st.collections.length + "</div></div>" +
      '<div class="card"><div class="k">' + esc(t("dash.docs")) + '</div><div class="v">' + st.totalDocs + "</div></div>" +
      '<div class="card"><div class="k">' + esc(t("dash.file")) + '</div><div class="v small">' + esc(st.fileSize || "—") + "</div></div>" +
      '<div class="card"><div class="k">' + esc(t("dash.cache")) + '</div><div class="v small">' + esc(st.cache ? (st.cache.bytes + " / " + st.cache.budget) : "—") + "</div></div>" +
    "</div>" +
    '<div class="toolbar"><span class="grow"></span><button class="btn primary" id="btn-new-coll">' + esc(t("dash.newColl")) + "</button></div>" +
    (st.collections.length
      ? '<div class="table-wrap"><table><thead><tr><th>' + esc(t("dash.name")) + "</th><th>" + esc(t("dash.docsCol")) + "</th><th></th></tr></thead><tbody>" +
        st.collections.map(c => '<tr><td class="mono">' + esc(c.name) + "</td><td>" + c.count + '</td><td><a class="btn small" href="#/coll/' + encodeURIComponent(c.name) + '">' + esc(t("dash.open")) + "</a></td></tr>").join("") +
        "</tbody></table></div>"
      : '<div class="empty"><div class="big">📁</div>' + esc(t("dash.empty")) + "</div>");
  $("#btn-new-coll").addEventListener("click", newCollModal);
}

/* ============ collections ============ */
async function viewCollections() {
  state.coll = null;
  await refreshCols();
  const m = $("#main");
  m.innerHTML =
    "<h1>" + esc(t("colls.title")) + "</h1>" +
    '<div class="toolbar"><span class="grow"></span><button class="btn primary" id="btn-new-coll">' + esc(t("colls.new")) + "</button></div>" +
    (state.colls.length
      ? '<div class="table-wrap"><table><thead><tr><th>' + esc(t("colls.name")) + "</th><th>" + esc(t("colls.docs")) + "</th><th>" + esc(t("colls.idxs")) + "</th><th></th></tr></thead><tbody>" +
        state.colls.map(c => '<tr><td class="mono">' + esc(c.name) + "</td><td>" + c.count + "</td><td>" + (c.indexes != null ? c.indexes : "—") + '</td><td style="text-align:right"><a class="btn small" href="#/coll/' + encodeURIComponent(c.name) + '">' + esc(t("colls.open")) + "</a></td></tr>").join("") +
        "</tbody></table></div>"
      : '<div class="empty">' + esc(t("colls.empty")) + "</div>");
  $("#btn-new-coll").addEventListener("click", newCollModal);
}

function newCollModal() {
  openModal(
    "<h2>" + esc(t("colls.new")) + '</h2><div class="field"><label>' + esc(t("colls.name")) + '</label><input id="m-name" placeholder="ordenes"></div><div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + '</button><button class="btn primary" id="m-ok">' + esc(t("common.ok")) + "</button></div>"
  );
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-ok").addEventListener("click", async () => {
    const n = $("#m-name").value.trim();
    if (!n) return;
    try { await api("/api/collections", { method: "POST", body: { name: n } }); closeModal(); toast(t("coll.created")); refreshCols(); location.hash = "#/coll/" + encodeURIComponent(n); }
    catch (e) { $("#m-err").textContent = e.message; }
  });
}

/* ============ collection view (tabs) ============ */
function tabLink(name, tab, label, active) {
  return '<button data-tab="' + tab + '" class="' + (active ? "active" : "") + '">' + esc(label) + "</button>";
}

async function viewColl(name, tab) {
  if (state.coll !== name) resetDocsView();
  // Prefs (columnas/límite) se recargan siempre: al cambiar de BD conservan
  // el mismo nombre de colección pero otra clave (db/coll).
  loadDocPrefs(name);
  state.coll = name; state.tab = tab; state.skip = 0;
  setActive(null);
  await refreshCols();
  const m = $("#main");
  m.innerHTML =
    "<h1>" + esc(name) + ' <small>· ' + esc(t("tabs." + tab)) + "</small></h1>" +
    '<div class="tabs">' +
      tabLink(name, "docs", t("tabs.docs"), tab === "docs") +
      tabLink(name, "indexes", t("tabs.indexes"), tab === "indexes") +
      tabLink(name, "io", t("tabs.io"), tab === "io") +
      tabLink(name, "triggers", t("tabs.triggers"), tab === "triggers") +
    "</div>" +
    '<div id="tab-body"><p class="page-sub">' + esc(t("common.loading")) + "</p></div>";
  $$('#main .tabs [data-tab]').forEach(b => b.addEventListener("click", () => {
    location.hash = "#/coll/" + encodeURIComponent(name) + "/" + b.getAttribute("data-tab");
  }));
  if (tab === "indexes") tabIndexes(name);
  else if (tab === "io") tabIO(name);
  else if (tab === "triggers") tabTriggers();
  else tabDocs(name);
}

/* ---- docs tab ---- */
/* ---- docs tab: vista dinámica (columnas, filtros guiados, paginación fija) ---- */
const DOC_LIMITS = [10, 25, 50, 100, 200];
const DOC_OPS = ["eq", "ne", "gt", "gte", "lt", "lte", "contains", "starts", "exists", "nexists", "in", "nin", "regex"];
const DOC_TYPES = ["auto", "text", "number", "bool", "json", "null"];
const DOC_SEARCH_MAX_FIELDS = 64;

async function tabDocs(name) {
  const body = $("#tab-body");
  body.classList.add("doc-view");
  body.innerHTML =
    '<div class="doc-head">' + docsToolbar() + docsViewBar() + '<div id="adv-slot">' + docsAdvPanel() + "</div>" +
      '<div class="doc-hint">' + esc(t("docs.hint")) + "</div></div>" +
    '<div class="doc-grid" id="doc-grid"><p class="page-sub">' + esc(t("common.loading")) + "</p></div>";
  bindDocHead(name);
  syncHead();
  loadDocs(name);
  loadFields(name, 2000);
}

function docsToolbar() {
  return '<div class="toolbar doc-toolbar">' +
    '<input class="grow" id="q-input" placeholder="' + esc(t("docs.searchPh")) + '" value="' + esc(state.q) + '">' +
    '<select id="q-field" style="width:auto;max-width:220px">' + fieldOptions(state.qf, true) + "</select>" +
    '<button class="btn" id="btn-adv">' + esc(t("docs.advanced")) + "</button>" +
    '<span class="grow"></span>' +
    '<button class="btn ghost small" id="btn-drop">' + esc(t("coll.drop")) + "</button>" +
    '<button class="btn primary" id="btn-new-doc">' + esc(t("docs.newDoc")) + "</button></div>";
}

function docsViewBar() {
  const lim = DOC_LIMITS.map(n => '<option value="' + n + '"' + (n === state.limit ? " selected" : "") + ">" + n + "</option>").join("");
  return '<div class="doc-view-bar">' +
    '<span class="cp-anchor"><button class="btn small" id="btn-cols">' + esc(t("docs.columns")) +
      ' <b id="cols-badge">—</b></button>' +
      '<div class="col-panel hidden" id="col-panel"></div></span>' +
    '<span class="doc-fields-info" id="fields-info"></span>' +
    '<span class="pg-info" id="pg-info">—</span>' +
    '<span class="pg-group">' +
      '<button class="btn small" id="pg-first" title="' + esc(t("pg.first")) + '">«</button>' +
      '<button class="btn small" id="pg-prev" title="' + esc(t("pg.prev")) + '">‹</button>' +
      '<input id="pg-num" class="pg-num" inputmode="numeric" value="1" title="' + esc(t("pg.page")) + '">' +
      '<span class="pg-sep">/ <b id="pg-total">1</b></span>' +
      '<button class="btn small" id="pg-next" title="' + esc(t("pg.next")) + '">›</button>' +
      '<button class="btn small" id="pg-last" title="' + esc(t("pg.last")) + '">»</button>' +
    "</span>" +
    '<label class="pg-limit">' + esc(t("docs.perPage")) + ' <select id="pg-limit">' + lim + "</select></label>" +
    '<button class="btn small" id="btn-refresh" title="' + esc(t("docs.refresh")) + '">⟳</button>' +
    '<button class="btn small" id="btn-scan-all" title="' + esc(t("docs.scanAll")) + '">⇅</button>' +
  "</div>";
}

async function loadDocs(name) {
  const grid = $("#doc-grid");
  if (!grid) return;
  let filter;
  try { filter = buildFilter(); }
  catch (e) { showAdvErr(e.message); return; }
  showAdvErr("");
  const p = new URLSearchParams({ skip: state.skip, limit: state.limit });
  if (filter) p.set("filter", JSON.stringify(filter));
  if (state.sort) { p.set("sort", state.sort); p.set("dir", String(state.dir)); }
  let r;
  try { r = await api("/api/collections/" + encodeURIComponent(name) + "/docs?" + p); }
  catch (e) { grid.innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  state.docs = r.docs || [];
  state.total = r.total || 0;
  if (r.limit) state.limit = r.limit;
  // Página fuera de rango (p. ej. tras borrar documentos): saltar a la última válida.
  if (!state.docs.length && state.total > 0 && state.skip >= state.total) {
    state.skip = Math.max(0, (totalPages() - 1) * state.limit);
    return loadDocs(name);
  }
  rememberPageKeys();
  renderGrid(name);
  syncHead();
}

// rememberPageKeys acumula las claves vistas en todas las páginas cargadas: son el
// esquema de reserva cuando /fields todavía no ha respondido.
function rememberPageKeys() {
  const keys = state.pageKeys.slice();
  const seen = new Set(keys);
  state.docs.forEach(d => Object.keys(d).forEach(k => { if (!seen.has(k)) { seen.add(k); keys.push(k); } }));
  state.pageKeys = keys;
}

function renderGrid(name) {
  const grid = $("#doc-grid");
  if (!grid) return;
  const cols = visibleCols();
  updateColsBadge();
  if (!state.docs.length) {
    grid.innerHTML = '<div class="empty"><div class="big">🗂</div>' + esc(hasFilters() ? t("docs.noResults") : t("coll.empty")) +
      (hasFilters() ? '<div style="margin-top:14px"><button class="btn small" id="btn-clear-all">' + esc(t("docs.clearAll")) + "</button></div>" : "") + "</div>";
    const b = $("#btn-clear-all");
    if (b) b.addEventListener("click", () => clearAllFilters(name));
    return;
  }
  const sortMark = (c) => state.sort === c ? (state.dir === 1 ? " sorted-asc" : " sorted-desc") : "";
  const head = cols.map(c => '<th data-sort="' + esc(c) + '" class="' + sortMark(c).trim() + '" title="' + esc(c + (typeOf(c) ? " · " + typeOf(c) : "")) + '">' + esc(c) + "</th>").join("") +
    '<th class="col-actions"></th>';
  const body = state.docs.map((d, i) => {
    const id = String(d._id == null ? "" : d._id);
    const cells = cols.map(c => {
      const raw = getVal(d, c);
      const txt = fmtVal(raw);
      return '<td title="' + esc(txt) + '"' + (raw !== null && typeof raw === "object" ? ' class="cell-json"' : "") + ">" + esc(txt) + "</td>";
    }).join("");
    return '<tr data-row="' + i + '">' + cells + '<td class="col-actions">' +
      '<button data-view="' + i + '" class="icon-btn" title="' + esc(t("docs.view")) + '">🔍</button>' +
      '<button data-edit="' + i + '" class="icon-btn" title="' + esc(t("docs.edit")) + '">✎</button>' +
      '<button data-del="' + esc(id) + '" class="icon-btn danger" title="' + esc(t("docs.delete")) + '">🗑</button></td></tr>';
  }).join("");
  grid.innerHTML = '<table class="doc-table"><thead><tr>' + head + "</tr></thead><tbody>" + body + "</tbody></table>";
  $$("th[data-sort]", grid).forEach(th => th.addEventListener("click", () => {
    const f = th.getAttribute("data-sort");
    if (state.sort === f) state.dir = -state.dir; else { state.sort = f; state.dir = 1; }
    state.skip = 0;
    loadDocs(name);
  }));
  $$("tbody tr", grid).forEach(tr => tr.addEventListener("dblclick", () => docViewer(name, state.docs[+tr.getAttribute("data-row")])));
  $$("[data-view]", grid).forEach(b => b.addEventListener("click", (e) => {
    e.stopPropagation();
    docViewer(name, state.docs[+b.getAttribute("data-view")]);
  }));
  $$("[data-edit]", grid).forEach(b => b.addEventListener("click", (e) => {
    e.stopPropagation();
    docEditor(name, state.docs[+b.getAttribute("data-edit")]);
  }));
  $$("[data-del]", grid).forEach(b => b.addEventListener("click", (e) => {
    e.stopPropagation();
    confirmModal(t("docs.delete"), t("docs.delConfirm"), async () => {
      await api("/api/collections/" + encodeURIComponent(name) + "/doc?id=" + encodeURIComponent(b.getAttribute("data-del")), { method: "DELETE" });
      toast(t("docs.deleted"));
      loadDocs(name); refreshCols();
    });
  }));
}

/* ---- paginación + cabecera fija ---- */
function totalPages() { return Math.max(1, Math.ceil(state.total / state.limit)); }
function currentPage() { return Math.min(totalPages(), Math.floor(state.skip / state.limit) + 1); }
function gotoPage(name, p) {
  const n = Math.max(1, Math.min(totalPages(), parseInt(p, 10) || 1));
  state.skip = (n - 1) * state.limit;
  loadDocs(name);
}

function syncHead() {
  const pages = totalPages(), page = currentPage(), info = $("#pg-info");
  if (info) {
    const from = state.total ? state.skip + 1 : 0;
    const to = state.skip + state.docs.length;
    info.textContent = t("pg.range").replace("{a}", from).replace("{b}", to).replace("{n}", state.total);
    $("#pg-num").value = String(page);
    $("#pg-total").textContent = String(pages);
    $("#pg-first").disabled = page <= 1;
    $("#pg-prev").disabled = page <= 1;
    $("#pg-next").disabled = page >= pages;
    $("#pg-last").disabled = page >= pages;
  }
  updateColsBadge();
  syncFieldsInfo();
}

function updateColsBadge() {
  const el = $("#cols-badge");
  if (el) el.textContent = visibleCols().length + "/" + allCols().length;
}

function syncFieldsInfo() {
  const el = $("#fields-info");
  if (!el) return;
  if (!state.fields.length && !state.pageKeys.length) { el.textContent = ""; return; }
  el.textContent = t("docs.fieldsInfo").replace("{n}", allCols().length).replace("{s}", state.fieldsScanned) +
    (state.fieldsTruncated ? " " + t("docs.fieldsPartial") : "");
}

/* ---- helpers de la vista dinámica de documentos ---- */

// typeOf devuelve el tipo observado (vía /fields) de un campo, si se conoce.
function typeOf(field) { return state.fieldTypes[field] || ""; }

// getVal lee un path con punto de un documento. Los arrays intermedios se
// expanden igual que el motor en los filtros (items.sku): si un tramo es un
// array se recogen los valores de todos sus elementos.
function getVal(obj, path) {
  const parts = String(path).split(".");
  let cur = [obj];
  for (let i = 0; i < parts.length; i++) {
    const p = parts[i], next = [];
    for (let j = 0; j < cur.length; j++) {
      const v = cur[j];
      if (v == null) continue;
      if (Array.isArray(v)) {
        for (let k = 0; k < v.length; k++) {
          const it = v[k];
          if (it !== null && typeof it === "object" && Object.prototype.hasOwnProperty.call(it, p)) next.push(it[p]);
        }
        if (/^\d+$/.test(p) && v[p] !== undefined) next.push(v[p]);
      } else if (typeof v === "object" && Object.prototype.hasOwnProperty.call(v, p)) {
        next.push(v[p]);
      }
    }
    cur = next;
    if (!cur.length) return undefined;
  }
  return cur.length === 1 ? cur[0] : cur;
}

// allCols es el universo de columnas: descubiertas en /fields + vistas en las
// páginas cargadas (pageKeys) + añadidas a mano (extra), con _id primero.
function allCols() {
  const out = [], seen = new Set();
  const push = f => { if (f && !seen.has(f)) { seen.add(f); out.push(f); } };
  push("_id");
  state.fields.forEach(push);
  state.paths.forEach(push);
  state.pageKeys.forEach(push);
  state.extra.forEach(push);
  return out;
}

function visibleCols() { return allCols().filter(c => !state.hidden.has(c)); }

// fieldOptions genera las <option> de los selects de campo; con withAny se
// antepone "(todos los campos)".
function fieldOptions(sel, withAny) {
  const cols = allCols();
  if (sel && cols.indexOf(sel) < 0) cols.unshift(sel);
  let html = withAny ? '<option value="">' + esc(t("docs.anyField")) + "</option>" : "";
  html += cols.map(c => '<option value="' + esc(c) + '"' + (c === sel ? " selected" : "") + ">" +
    esc(c + (typeOf(c) ? " · " + typeOf(c) : "")) + "</option>").join("");
  return html;
}

function escapeRegex(s) { return String(s).replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }

// parseVal convierte el texto de una condición al tipo elegido; lanza Error.
function parseVal(raw, type) {
  const s = String(raw == null ? "" : raw);
  if (type === "null") return null;
  if (type === "text") return s;
  if (type === "number") {
    const n = Number(s);
    if (s.trim() === "" || !Number.isFinite(n)) throw new Error(t("docs.badValue") + " " + s);
    return n;
  }
  if (type === "bool") {
    const b = s.trim().toLowerCase();
    if (b === "true" || b === "1") return true;
    if (b === "false" || b === "0") return false;
    throw new Error(t("docs.badValue") + " " + s);
  }
  if (type === "json") {
    try { return JSON.parse(s); } catch (e) { throw new Error(t("docs.badJson") + " " + e.message); }
  }
  // auto: números/booleanos/objetos si lo son, si no texto literal.
  const tr = s.trim();
  if (tr === "") return s;
  try { return JSON.parse(tr); } catch (e) { return s; }
}

// condToFilter traduce una condición guiada a un filtro del motor.
function condToFilter(c) {
  const f = c.field;
  if (!f) return null;
  const op = c.op || "eq";
  if (op === "exists") return { [f]: { $exists: true } };
  if (op === "nexists") return { [f]: { $exists: false } };
  if (op === "contains" || op === "starts") {
    const re = (op === "starts" ? "^" : "") + escapeRegex(String(c.value == null ? "" : c.value));
    return { [f]: { $regex: re, $options: "i" } };
  }
  if (op === "regex") return { [f]: { $regex: String(c.value == null ? "" : c.value) } };
  if (op === "in" || op === "nin") {
    let list = c.value;
    if (typeof list === "string") {
      const raw = list.trim();
      if (raw.charAt(0) === "[") {
        try { list = JSON.parse(raw); } catch (e) { throw new Error(t("docs.badJson") + " " + e.message); }
      } else {
        list = raw === "" ? [] : raw.split(",").map(x => x.trim()).filter(x => x !== "");
      }
    }
    if (!Array.isArray(list)) throw new Error(t("docs.badValue") + " " + fmtVal(c.value));
    return { [f]: op === "in" ? { $in: list } : { $nin: list } };
  }
  const key = { eq: "$eq", ne: "$ne", gt: "$gt", gte: "$gte", lt: "$lt", lte: "$lte" }[op];
  if (!key) return null;
  const v = parseVal(c.value, c.type || "auto");
  return { [f]: { [key]: v } };
}

// searchFilter cubre la búsqueda rápida por valor (campo concreto o todos):
// regex insensible a mayúsculas y, si el texto es numérico/booleano, también
// igualdad exacta para casar valores no-texto.
function searchFilter() {
  const q = String(state.q == null ? "" : state.q).trim();
  if (!q) return null;
  const fields = state.qf ? [state.qf] : allCols().slice(0, DOC_SEARCH_MAX_FIELDS);
  const re = escapeRegex(q);
  const n = Number(q);
  const num = q !== "" && Number.isFinite(n) ? n : null;
  const alts = [];
  fields.forEach(f => {
    alts.push({ [f]: { $regex: re, $options: "i" } });
    if (num !== null) alts.push({ [f]: { $eq: num } });
    if (q === "true" || q === "false") alts.push({ [f]: { $eq: q === "true" } });
  });
  if (!alts.length) return null;
  return alts.length === 1 ? alts[0] : { $or: alts };
}

// buildFilter combina filtro JSON + condiciones guiadas + búsqueda rápida.
// Lanza Error (mostrado por showAdvErr) si el JSON o un valor no son válidos.
function buildFilter() {
  const parts = [];
  const jf = String(state.filter == null ? "" : state.filter).trim();
  if (jf && jf !== "null") {
    let parsed;
    try { parsed = JSON.parse(jf); } catch (e) { throw new Error(t("docs.badJson") + " " + e.message); }
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(t("docs.badValue") + " " + jf);
    parts.push(parsed);
  }
  state.conds.forEach(c => { const f = condToFilter(c); if (f) parts.push(f); });
  const sf = searchFilter();
  if (sf) parts.push(sf);
  if (!parts.length) return null;
  return parts.length === 1 ? parts[0] : { $and: parts };
}

function hasFilters() {
  return !!(String(state.q || "").trim() || state.conds.length || String(state.filter || "").trim());
}

function openAdv(open) {
  const panel = $("#adv-panel");
  if (panel) panel.classList.toggle("hidden", !open);
}

// showAdvErr pinta el error del filtro JSON (y abre el panel para que se vea).
function showAdvErr(msg) {
  const el = $("#adv-err");
  if (el) el.textContent = msg || "";
  if (msg) openAdv(true);
}

function clearAllFilters(name) {
  state.q = ""; state.qf = ""; state.conds = []; state.filter = ""; state.skip = 0;
  const qin = $("#q-input"); if (qin) qin.value = "";
  const qf = $("#q-field"); if (qf) qf.value = "";
  renderAdvPanel(name);
  loadDocs(name);
}

// docsAdvPanel: condiciones guiadas (campo + operador + tipo + valor) más
// filtro JSON avanzado. Se repinta entero (renderAdvPanel) al añadir/quitar.
function docsAdvPanel() {
  const conds = state.conds.map((c, i) =>
    '<div class="cond-row" data-i="' + i + '">' +
      '<input class="c-field" list="adv-field-list" placeholder="' + esc(t("docs.addField")) + '" value="' + esc(c.field || "") + '">' +
      '<select class="c-op">' + DOC_OPS.map(o => '<option value="' + o + '"' + (o === (c.op || "eq") ? " selected" : "") + ">" + esc(t("op." + o)) + "</option>").join("") + "</select>" +
      '<select class="c-type">' + DOC_TYPES.map(y => '<option value="' + y + '"' + (y === (c.type || "auto") ? " selected" : "") + ">" + esc(t("type." + y)) + "</option>").join("") + "</select>" +
      '<input class="c-val" placeholder="' + esc(t("docs.value")) + '" value="' + esc(c.value == null ? "" : c.value) + '">' +
      '<button class="icon-btn danger c-rm" title="' + esc(t("docs.clearConds")) + '">🗑</button>' +
    "</div>").join("");
  return '<div class="adv-panel hidden" id="adv-panel">' +
    '<div class="adv-block">' +
      '<span class="adv-label">' + esc(t("docs.where")) + "</span>" +
      '<div class="cond-list">' + (conds || '<div class="gs-empty">' + esc(t("docs.filterHint")) + "</div>") + "</div>" +
      '<div class="adv-btns">' +
        '<button class="btn small" id="btn-add-cond">' + esc(t("docs.addCond")) + "</button>" +
        '<button class="btn small" id="btn-clear-conds">' + esc(t("docs.clearConds")) + "</button>" +
      "</div>" +
    "</div>" +
    '<div class="adv-block">' +
      '<span class="adv-label">' + esc(t("docs.jsonFilter")) + "</span>" +
      '<textarea id="adv-json" class="code-small" rows="3" placeholder=\'{"total": {"$gte": 100}}\'>' + esc(state.filter || "") + "</textarea>" +
      '<div class="adv-btns">' +
        '<button class="btn small primary" id="btn-apply-json">' + esc(t("docs.apply")) + "</button>" +
        '<button class="btn small" id="btn-clear-json">' + esc(t("docs.clear")) + "</button>" +
        '<span class="adv-err" id="adv-err"></span>' +
      "</div>" +
    "</div>" +
    '<datalist id="adv-field-list">' + allCols().map(c => '<option value="' + esc(c) + '"></option>').join("") + "</datalist>" +
  "</div>";
}

// renderAdvPanel repinta el panel de filtros conservando su estado abierto/cerrado.
function renderAdvPanel(name) {
  const slot = $("#adv-slot");
  if (!slot) return;
  const prev = $("#adv-panel");
  const open = prev ? !prev.classList.contains("hidden") : false;
  slot.innerHTML = docsAdvPanel();
  if (open) openAdv(true);
  bindAdvPanel(name);
}

function bindAdvPanel(name) {
  const panel = $("#adv-panel");
  if (!panel) return;
  const rerender = () => renderAdvPanel(name);
  const applyConds = () => { state.skip = 0; loadDocs(name); };
  const add = $("#btn-add-cond", panel);
  if (add) add.addEventListener("click", () => {
    state.conds.push({ field: "", op: "eq", type: "auto", value: "" });
    rerender();
    const rows = $$(".cond-row .c-field", $("#adv-panel"));
    if (rows.length) rows[rows.length - 1].focus();
  });
  const clr = $("#btn-clear-conds", panel);
  if (clr) clr.addEventListener("click", () => { state.conds = []; rerender(); applyConds(); });
  $$(".cond-row", panel).forEach(row => {
    const i = +row.getAttribute("data-i");
    const c = state.conds[i];
    if (!c) return;
    const field = $(".c-field", row), op = $(".c-op", row), type = $(".c-type", row),
          val = $(".c-val", row), rm = $(".c-rm", row);
    const upd = () => { c.field = field.value.trim(); applyConds(); };
    field.addEventListener("change", upd);
    field.addEventListener("keydown", e => { if (e.key === "Enter") upd(); });
    op.addEventListener("change", () => { c.op = op.value; applyConds(); });
    type.addEventListener("change", () => { c.type = type.value; applyConds(); });
    const updVal = () => { c.value = val.value; applyConds(); };
    val.addEventListener("change", updVal);
    val.addEventListener("keydown", e => { if (e.key === "Enter") updVal(); });
    rm.addEventListener("click", () => { state.conds.splice(i, 1); rerender(); applyConds(); });
  });
  const jta = $("#adv-json", panel);
  const applyJson = () => { state.filter = jta ? jta.value : ""; state.skip = 0; loadDocs(name); };
  const applyBtn = $("#btn-apply-json", panel);
  if (applyBtn) applyBtn.addEventListener("click", applyJson);
  if (jta) jta.addEventListener("keydown", e => { if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) applyJson(); });
  const clearBtn = $("#btn-clear-json", panel);
  if (clearBtn) clearBtn.addEventListener("click", () => {
    if (jta) jta.value = "";
    state.filter = ""; state.skip = 0;
    showAdvErr("");
    loadDocs(name);
  });
}

// loadFields descubre el esquema de la colección en el servidor (GET /fields).
// maxScan=0 ⇒ escaneo completo (puede tardar en colecciones grandes).
async function loadFields(name, maxScan) {
  const info = $("#fields-info");
  if (info) info.textContent = t("docs.discovering");
  try {
    const scan = maxScan == null ? 2000 : maxScan;
    const r = await api("/api/collections/" + encodeURIComponent(name) + "/fields?" +
      new URLSearchParams({ maxScan: String(scan) }));
    state.fields = r.fields || [];
    state.paths = r.paths || [];
    state.fieldTypes = r.types || {};
    state.fieldsScanned = r.scanned || 0;
    state.fieldsTruncated = !!r.truncated;
    // un path ya conocido deja de ser "campo extra" manual
    const known = new Set(state.fields.concat(state.paths));
    state.extra = state.extra.filter(f => !known.has(f));
    if (scan === 0 && !state.fieldsTruncated) toast(t("docs.scanDone"));
    const qfs = $("#q-field");
    if (qfs) {
      qfs.innerHTML = fieldOptions(state.qf, true);
      state.qf = qfs.value;
    }
    renderColPanel(name);
    if ($("#doc-grid")) renderGrid(name);
    updateColsBadge();
    syncFieldsInfo();
  } catch (e) {
    syncFieldsInfo(); // sin esquema la vista sigue funcionando con pageKeys
  }
}

// renderColPanel pinta el selector de columnas (persistido por colección).
function renderColPanel(name) {
  const panel = $("#col-panel");
  if (!panel || panel.classList.contains("hidden")) return;
  const q = String(state.colQuery || "").toLowerCase();
  const cols = allCols().filter(c => c.toLowerCase().indexOf(q) >= 0);
  const items = cols.map(c =>
    '<label class="col-item"><input type="checkbox" data-col="' + esc(c) + '"' + (state.hidden.has(c) ? "" : " checked") + "> " +
    esc(c) + (typeOf(c) ? " <i>" + esc(typeOf(c)) + "</i>" : "") + "</label>").join("");
  panel.innerHTML =
    '<div class="col-btns">' +
      '<button class="btn small" data-colact="all">' + esc(t("docs.all")) + "</button>" +
      '<button class="btn small" data-colact="none">' + esc(t("docs.none")) + "</button>" +
      '<button class="btn small" data-colact="page">' + esc(t("docs.onlyPage")) + "</button>" +
    "</div>" +
    '<input id="col-q" placeholder="' + esc(t("docs.searchFieldPh")) + '" value="' + esc(state.colQuery) + '">' +
    '<div class="col-list">' + (items || '<div class="gs-empty">' + esc(t("docs.needFields")) + "</div>") + "</div>" +
    '<div class="col-add"><input id="col-add" placeholder="' + esc(t("docs.addField")) + '"><button class="btn small" id="col-add-btn">+</button></div>' +
    '<div class="col-hint">' + esc(t("docs.colsHint")) + "</div>";
  $$("input[data-col]", panel).forEach(cb => cb.addEventListener("change", () => {
    const f = cb.getAttribute("data-col");
    if (cb.checked) state.hidden.delete(f); else state.hidden.add(f);
    saveDocPrefs();
    renderGrid(name);
    updateColsBadge();
  }));
  $$("[data-colact]", panel).forEach(b => b.addEventListener("click", () => {
    const act = b.getAttribute("data-colact");
    if (act === "all") state.hidden = new Set();
    else if (act === "none") state.hidden = new Set(allCols());
    else if (act === "page") {
      const vis = new Set(state.pageKeys);
      state.hidden = new Set(allCols().filter(c => !vis.has(c)));
    }
    saveDocPrefs();
    renderColPanel(name);
    renderGrid(name);
    updateColsBadge();
  }));
  const cq = $("#col-q", panel);
  if (cq) cq.addEventListener("input", () => {
    state.colQuery = cq.value;
    renderColPanel(name);
    const again = $("#col-q");
    if (again) { again.focus(); again.setSelectionRange(again.value.length, again.value.length); }
  });
  const addField = () => {
    const inp = $("#col-add", panel);
    const f = (inp.value || "").trim();
    if (!f) return;
    if (state.extra.indexOf(f) < 0 && allCols().indexOf(f) < 0) state.extra.push(f);
    state.hidden.delete(f);
    inp.value = "";
    saveDocPrefs();
    renderColPanel(name);
    renderGrid(name);
    updateColsBadge();
  };
  const addBtn = $("#col-add-btn", panel), addInp = $("#col-add", panel);
  if (addBtn) addBtn.addEventListener("click", addField);
  if (addInp) addInp.addEventListener("keydown", e => { if (e.key === "Enter") addField(); });
}

function toggleColPanel(name) {
  const panel = $("#col-panel");
  if (!panel) return;
  const opening = panel.classList.contains("hidden");
  panel.classList.toggle("hidden", !opening);
  if (opening) renderColPanel(name);
}

// bindColDocClose cierra el selector de columnas al pulsar fuera (una sola vez).
let colCloseBound = false;
function bindColDocClose() {
  if (colCloseBound) return;
  colCloseBound = true;
  document.addEventListener("click", (e) => {
    const panel = document.getElementById("col-panel");
    if (!panel || panel.classList.contains("hidden")) return;
    if (panel.contains(e.target)) return;
    if (e.target && e.target.closest && e.target.closest("#btn-cols")) return;
    panel.classList.add("hidden");
  });
}

/* ---- prefs por colección (columnas visibles + regs/página) ---- */
function docPrefsKey(name) { return "mls-docview:" + (state.db || "") + "/" + name; }

function loadDocPrefs(name) {
  let raw = null;
  try { raw = localStorage.getItem(docPrefsKey(name)); } catch (e) { return; }
  if (!raw) return;
  try {
    const p = JSON.parse(raw);
    state.hidden = new Set(Array.isArray(p.hidden) ? p.hidden : []);
    state.extra = Array.isArray(p.extra) ? p.extra : [];
    const lim = Number(p.limit);
    if (lim > 0 && DOC_LIMITS.indexOf(lim) >= 0) state.limit = lim;
  } catch (e) { /* prefs corruptos → defaults */ }
}

function saveDocPrefs() {
  try {
    localStorage.setItem(docPrefsKey(state.coll || ""), JSON.stringify({
      hidden: Array.from(state.hidden), extra: state.extra, limit: state.limit
    }));
  } catch (e) { /* sin storage */ }
}

/* ---- cabecera de la vista: toolbar + barra de vista + paginación ---- */
function bindDocHead(name) {
  bindColDocClose();
  const qin = $("#q-input");
  let tmr = null;
  const doSearch = () => { state.q = qin ? qin.value : ""; state.skip = 0; loadDocs(name); };
  if (qin) {
    qin.addEventListener("input", () => { clearTimeout(tmr); tmr = setTimeout(doSearch, 300); });
    qin.addEventListener("keydown", e => { if (e.key === "Enter") { clearTimeout(tmr); doSearch(); } });
  }
  const qf = $("#q-field");
  if (qf) qf.addEventListener("change", () => { state.qf = qf.value; state.skip = 0; loadDocs(name); });
  const adv = $("#btn-adv");
  if (adv) adv.addEventListener("click", () => openAdv($("#adv-panel").classList.contains("hidden")));
  const cols = $("#btn-cols");
  if (cols) cols.addEventListener("click", (e) => { e.stopPropagation(); toggleColPanel(name); });
  const nd = $("#btn-new-doc");
  if (nd) nd.addEventListener("click", () => docEditor(name, null));
  const drop = $("#btn-drop");
  if (drop) drop.addEventListener("click", () => confirmModal(t("coll.drop"), t("coll.dropConfirm"), async () => {
    await api("/api/collections/" + encodeURIComponent(name), { method: "DELETE" });
    toast(t("common.deleted"));
    location.hash = "#/dashboard";
    refreshCols();
  }));
  const pg = (id, fn) => { const el = $(id); if (el) el.addEventListener("click", () => fn()); };
  pg("#pg-first", () => gotoPage(name, 1));
  pg("#pg-prev", () => gotoPage(name, currentPage() - 1));
  pg("#pg-next", () => gotoPage(name, currentPage() + 1));
  pg("#pg-last", () => gotoPage(name, totalPages()));
  const num = $("#pg-num");
  if (num) {
    num.addEventListener("change", () => gotoPage(name, num.value));
    num.addEventListener("keydown", e => { if (e.key === "Enter") gotoPage(name, num.value); });
  }
  const lim = $("#pg-limit");
  if (lim) lim.addEventListener("change", () => {
    const n = parseInt(lim.value, 10);
    if (n > 0) { state.limit = n; state.skip = 0; saveDocPrefs(); loadDocs(name); }
  });
  const ref = $("#btn-refresh");
  if (ref) ref.addEventListener("click", () => { loadDocs(name); loadFields(name, 2000); });
  const scan = $("#btn-scan-all");
  if (scan) scan.addEventListener("click", () => loadFields(name, 0));
  bindAdvPanel(name);
}

// docViewer muestra el JSON completo del documento (doble clic / 🔍).
function docViewer(name, doc) {
  const json = JSON.stringify(doc, null, 2);
  openModal(
    "<h2>" + esc(t("docs.view")) + "</h2>" +
    '<pre class="doc-json">' + esc(json) + "</pre>" +
    '<div class="actions"><button class="btn" id="m-copy">' + esc(t("docs.copy")) + '</button><span class="grow"></span>' +
    '<button class="btn" id="m-cancel">' + esc(t("common.close")) + '</button>' +
    '<button class="btn primary" id="m-edit">' + esc(t("docs.edit")) + "</button></div>"
  );
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-copy").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(json); toast(t("docs.copied")); }
    catch (e) { toast(t("common.error"), true); }
  });
  $("#m-edit").addEventListener("click", () => { closeModal(); docEditor(name, doc); });
}

function docEditor(name, doc) {

  const isNew = !doc;
  const init = doc ? JSON.stringify(doc, null, 2) : "{\n  \n}";
  openModal(
    "<h2>" + esc(isNew ? t("editor.newTitle") : t("editor.editTitle") + " · " + String(doc._id)) + "</h2>" +
    '<div class="field"><label>' + esc(t("editor.json")) + '</label><textarea class="code" id="m-json">' + esc(init) + "</textarea></div>" +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions">' + (isNew ? "" : '<button class="btn danger" id="m-del">' + esc(t("editor.delete")) + "</button>") +
    '<span class="grow"></span><button class="btn" id="m-cancel">' + esc(t("editor.cancel")) + '</button><button class="btn primary" id="m-ok">' + esc(t("editor.save")) + "</button></div>"
  );
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-ok").addEventListener("click", async () => {
    let p;
    try { p = JSON.parse($("#m-json").value); }
    catch (e) { $("#m-err").textContent = t("editor.invalid") + e.message; return; }
    if (typeof p !== "object" || p === null || Array.isArray(p)) { $("#m-err").textContent = t("editor.mustObject"); return; }
    try {
      if (isNew) await api("/api/collections/" + encodeURIComponent(name) + "/docs", { method: "POST", body: p });
      else await api("/api/collections/" + encodeURIComponent(name) + "/doc?id=" + encodeURIComponent(String(doc._id)), { method: "PUT", body: p });
      closeModal(); toast(t("docs.saved")); loadDocs(name); refreshCols();
    } catch (e) { $("#m-err").textContent = e.message; }
  });
  const del = $("#m-del");
  if (del) del.addEventListener("click", () => confirmModal(t("docs.delete"), t("docs.delConfirm"), async () => {
    await api("/api/collections/" + encodeURIComponent(name) + "/doc?id=" + encodeURIComponent(String(doc._id)), { method: "DELETE" });
    closeModal(); toast(t("docs.deleted")); loadDocs(name); refreshCols();
  }));
}

/* ---- indexes tab ---- */
async function tabIndexes(name) {
  const body = $("#tab-body");
  body.innerHTML =
    '<div class="toolbar"><input id="idx-fields" placeholder="' + esc(t("idx.fields")) + '" style="width:320px">' +
    '<label class="idx-check"><input type="checkbox" id="idx-unique"> ' + esc(t("idx.unique")) + "</label>" +
    '<button class="btn primary" id="idx-create">' + esc(t("idx.create")) + "</button></div>" +
    '<div id="idx-area"><p class="page-sub">' + esc(t("common.loading")) + "</p></div>";
  $("#idx-create").addEventListener("click", async () => {
    const fields = $("#idx-fields").value.split(",").map(s => s.trim()).filter(Boolean);
    if (!fields.length) { toast(t("idx.fieldsReq"), true); return; }
    try {
      await api("/api/collections/" + encodeURIComponent(name) + "/indexes", { method: "POST", body: { fields, unique: $("#idx-unique").checked } });
      toast(t("common.saved")); loadIndexes(name);
    } catch (e) { toast(e.message, true); }
  });
  loadIndexes(name);
}

async function loadIndexes(name) {
  const area = $("#idx-area");
  if (!area) return;
  let r;
  try { r = await api("/api/collections/" + encodeURIComponent(name) + "/indexes"); }
  catch (e) { area.innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  const idxs = r.indexes || [];
  area.innerHTML = idxs.length
    ? '<div class="table-wrap"><table><thead><tr><th>' + esc(t("idx.name")) + "</th><th>" + esc(t("idx.fieldsCol")) + "</th><th>" + esc(t("idx.uniqueCol")) + "</th><th>" + esc(t("common.actions")) + "</th></tr></thead><tbody>" +
      idxs.map(ix => '<tr><td class="mono">' + esc(ix.name) + '</td><td class="mono">' + esc(ix.fields.join(", ")) + "</td><td>" + (ix.unique ? "✔" : "—") + '</td><td><button class="btn small danger" data-drop="' + esc(ix.fields.join(",")) + '">' + esc(t("docs.delete")) + "</button></td></tr>").join("") +
      "</tbody></table></div>"
    : '<div class="empty">' + esc(t("idx.empty")) + "</div>";
  $$("[data-drop]", area).forEach(b => b.addEventListener("click", () => confirmModal(t("idx.dropConfirm"), b.getAttribute("data-drop"), async () => {
    await api("/api/collections/" + encodeURIComponent(name) + "/indexes?fields=" + encodeURIComponent(b.getAttribute("data-drop")), { method: "DELETE" });
    toast(t("docs.deleted")); loadIndexes(name);
  })));
}

/* ---- import/export tab ---- */
function tabIO(name) {
  const body = $("#tab-body");
  body.innerHTML =
    '<div class="io-grid">' +
      '<div class="card"><h3>' + esc(t("io.exportTitle")) + "</h3>" +
        '<div class="field"><label>' + esc(t("io.format")) + '</label><select id="exp-format"><option value="json">JSON</option><option value="ndjson">NDJSON</option><option value="csv">CSV</option></select></div>' +
        '<button class="btn primary" id="exp-btn">' + esc(t("io.download")) + "</button></div>" +
      '<div class="card"><h3>' + esc(t("io.importTitle")) + "</h3>" +
        '<div class="field"><label>' + esc(t("io.file")) + '</label><input type="file" id="imp-file"></div>' +
        '<div class="field"><label>' + esc(t("io.format")) + '</label><select id="imp-format"><option value="auto">auto</option><option value="json">JSON</option><option value="ndjson">NDJSON</option><option value="csv">CSV</option></select></div>' +
        '<div class="field"><label>' + esc(t("io.mode")) + '</label><select id="imp-mode"><option value="insert">' + esc(t("io.insert")) + '</option><option value="upsert">' + esc(t("io.upsert")) + "</option></select></div>" +
        '<button class="btn primary" id="imp-btn">' + esc(t("io.importBtn")) + "</button></div>" +
    "</div>";
  $("#exp-btn").addEventListener("click", async () => {
    const format = $("#exp-format").value;
    try {
      const r = await fetch(apiUrl("/api/collections/" + encodeURIComponent(name) + "/export?format=" + format), { credentials: "same-origin" });
      if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || r.statusText); }
      const blob = await r.blob();
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = name + "." + format;
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 2000);
    } catch (e) { toast(e.message, true); }
  });
  $("#imp-btn").addEventListener("click", async () => {
    const f = $("#imp-file").files[0];
    if (!f) { toast(t("io.pickFile"), true); return; }
    let format = $("#imp-format").value;
    if (format === "auto") {
      const ext = (f.name.split(".").pop() || "").toLowerCase();
      format = ext === "csv" ? "csv" : ext === "ndjson" ? "ndjson" : "json";
    }
    try {
      const r = await api("/api/collections/" + encodeURIComponent(name) + "/import?format=" + format + "&mode=" + $("#imp-mode").value, { method: "POST", body: f });
      pollImport(r.jobId);
    } catch (e) { toast(e.message, true); }
  });
}

function pollImport(jobId) {
  openModal(
    "<h2>" + esc(t("io.running")) + "</h2>" +
    '<div class="prog-stats"><span id="pg-label">0 / 0</span><b id="pg-pct">0%</b></div>' +
    '<div class="prog-bar"><div class="prog-fill" id="pg-fill"></div></div>' +
    '<div class="prog-detail" id="pg-detail"></div>' +
    '<div class="actions"><button class="btn" id="m-close">' + esc(t("common.close")) + '</button><button class="btn primary" id="m-done" disabled>' + esc(t("common.ok")) + "</button></div>"
  );
  $("#m-close").addEventListener("click", closeModal);
  const timer = setInterval(async () => {
    let d;
    try { d = await api("/api/import/" + encodeURIComponent(jobId)); }
    catch (e) { clearInterval(timer); $("#pg-detail").textContent = e.message; return; }
    const pct = d.total ? Math.min(100, Math.round((d.processed / d.total) * 100)) : 0;
    $("#pg-fill").style.width = pct + "%";
    $("#pg-pct").textContent = pct + "%";
    $("#pg-label").textContent = d.processed + " / " + (d.total || "?");
    $("#pg-detail").textContent = t("io.processed") + ": " + d.processed + " · " + t("io.inserted") + ": " + d.inserted + " · " + t("io.errors") + ": " + d.errors;
    if (d.status !== "running") {
      clearInterval(timer);
      $("#pg-detail").textContent = (d.status === "error" ? t("io.error") + ": " + (d.error || "") : t("io.done")) + " — " +
        t("io.inserted") + ": " + d.inserted + " · " + t("io.errors") + ": " + d.errors;
      $("#m-done").disabled = false;
      $("#m-done").addEventListener("click", () => { closeModal(); refreshCols(); });
    }
  }, 400);
}

/* ---- triggers tab ---- */
const TRIG_EVENTS = ["before_insert", "after_insert", "before_update", "after_update", "before_delete", "after_delete", "before_upsert", "after_upsert"];

async function tabTriggers() {
  const body = $("#tab-body");
  body.innerHTML =
    '<div class="toolbar"><span class="grow"></span><button class="btn primary" id="trig-new">' + esc(t("trig.new")) + "</button></div>" +
    '<div id="trig-area"><p class="page-sub">' + esc(t("common.loading")) + "</p></div>";
  $("#trig-new").addEventListener("click", () => triggerForm(null));
  loadTriggers();
}

async function loadTriggers() {
  const area = $("#trig-area");
  if (!area) return;
  let r;
  try { r = await api("/api/triggers"); }
  catch (e) { area.innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  const list = r.triggers || [];
  area.innerHTML = list.length
    ? '<div class="trig-grid">' + list.map(tr =>
        '<div class="trig-card"><div class="head"><b class="mono">' + esc(tr._id) + "</b>" +
        '<span class="badge ev">' + esc(tr.event) + "</span>" +
        (tr.async ? '<span class="badge ev">async</span>' : "") +
        '<span class="badge ' + (tr.enabled === false ? "off" : "on") + '">' + (tr.enabled === false ? esc(t("trig.off")) : esc(t("trig.on"))) + "</span></div>" +
        '<div class="meta">' + esc(tr.collection || t("trig.allColls")) + "</div>" +
        '<div class="actions"><button class="btn small" data-toggle="' + esc(tr._id) + '">' + (tr.enabled === false ? "✔" : "⏸") + "</button>" +
        '<button class="btn small" data-edit="' + esc(tr._id) + '">✎</button>' +
        '<button class="btn small danger" data-del="' + esc(tr._id) + '">🗑</button></div></div>').join("") + "</div>"
    : '<div class="empty">' + esc(t("trig.empty")) + "</div>";
  const byId = {};
  list.forEach(tr => { byId[tr._id] = tr; });
  $$("[data-toggle]", area).forEach(b => b.addEventListener("click", () => {
    const tr = byId[b.getAttribute("data-toggle")];
    triggerForm(Object.assign({}, tr, { enabled: tr.enabled === false }));
  }));
  $$("[data-edit]", area).forEach(b => b.addEventListener("click", () => triggerForm(byId[b.getAttribute("data-edit")])));
  $$("[data-del]", area).forEach(b => b.addEventListener("click", () => confirmModal(t("trig.title"), t("trig.delConfirm"), async () => {
    await api("/api/triggers/" + encodeURIComponent(b.getAttribute("data-del")), { method: "DELETE" });
    toast(t("docs.deleted")); loadTriggers();
  })));
}

function triggerForm(tr) {
  const isNew = !tr;
  tr = tr || { _id: "", event: "after_insert", collection: "", filter: {}, actions: [], async: false, enabled: true };
  openModal(
    "<h2>" + esc(t("trig.new")) + "</h2>" +
    '<div class="field"><label>' + esc(t("trig.id")) + '</label><input id="m-id" value="' + esc(tr._id) + '"></div>' +
    '<div class="field"><label>' + esc(t("trig.event")) + '</label><select id="m-event">' + TRIG_EVENTS.map(e => '<option value="' + e + '"' + (e === tr.event ? " selected" : "") + ">" + e + "</option>").join("") + "</select></div>" +
    '<div class="field"><label>' + esc(t("trig.coll")) + '</label><input id="m-coll" value="' + esc(tr.collection || "") + '"></div>' +
    '<div class="field"><label>' + esc(t("trig.filter")) + '</label><textarea class="code" id="m-filter" style="min-height:70px">' + esc(JSON.stringify(tr.filter || {}, null, 2)) + "</textarea></div>" +
    '<div class="field"><label>' + esc(t("trig.actions")) + '</label><textarea class="code" id="m-actions" style="min-height:180px">' + esc(JSON.stringify(tr.actions && tr.actions.length ? tr.actions : [{ type: "insert", collection: "audit", doc: { ref: { $get: "_id" } } }], null, 2)) + "</textarea></div>" +
    '<div class="field"><label class="idx-check"><input type="checkbox" id="m-async"' + (tr.async ? " checked" : "") + "> " + esc(t("trig.async")) + "</label></div>" +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + '</button><button class="btn primary" id="m-ok">' + esc(t("editor.save")) + "</button></div>"
  );
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-ok").addEventListener("click", async () => {
    let filter, actions;
    try { filter = JSON.parse($("#m-filter").value || "{}"); }
    catch (e) { $("#m-err").textContent = t("editor.invalid") + e.message; return; }
    try { actions = JSON.parse($("#m-actions").value); }
    catch (e) { $("#m-err").textContent = t("trig.invalidActions") + e.message; return; }
    if (!Array.isArray(actions)) { $("#m-err").textContent = t("trig.invalidActions") + "[]"; return; }
    const bodyT = {
      _id: $("#m-id").value.trim(),
      event: $("#m-event").value,
      collection: $("#m-coll").value.trim(),
      enabled: true,
      async: $("#m-async").checked,
      filter: filter,
      actions: actions
    };
    try { await api("/api/triggers", { method: "PUT", body: bodyT }); closeModal(); toast(t("common.saved")); loadTriggers(); }
    catch (e) { $("#m-err").textContent = e.message; }
  });
}

/* ============ console ============ */
async function viewConsole() {
  state.coll = null;
  const m = $("#main");
  let colls;
  try { const r = await api("/api/collections"); colls = (r.collections || []).map(c => c.name); }
  catch (e) { m.innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; return; }
  m.innerHTML =
    "<h1>" + esc(t("con.title")) + "</h1>" +
    '<div class="toolbar">' +
      '<select id="q-coll" style="width:auto">' + colls.map(c => '<option value="' + esc(c) + '">' + esc(c) + "</option>").join("") + "</select>" +
      '<input id="q-filter" class="grow" placeholder=\'' + esc(t("con.filter")) + "' value=\"{}\">" +
      '<input id="q-limit" type="number" min="1" max="500" value="50" style="width:90px" title="' + esc(t("con.limit")) + '">' +
      '<button class="btn primary" id="q-run">' + esc(t("con.run")) + "</button></div>" +
    '<div id="q-result"></div>';
  $("#q-run").addEventListener("click", async () => {
    const coll = $("#q-coll").value;
    let filter;
    try { filter = JSON.parse($("#q-filter").value || "{}"); }
    catch (e) { $("#q-result").innerHTML = '<div class="empty">' + esc(t("editor.invalid") + e.message) + "</div>"; return; }
    const limit = Math.max(1, Math.min(500, parseInt($("#q-limit").value, 10) || 50));
    try {
      const r = await api("/api/query", { method: "POST", body: { coll, filter, limit } });
      $("#q-result").innerHTML =
        '<p class="page-sub"><b>' + r.total + "</b> " + esc(t("con.total")) + "</p>" +
        (r.docs && r.docs.length ? resultsTable(r.docs) : '<div class="empty">' + esc(t("con.empty")) + "</div>");
    } catch (e) { $("#q-result").innerHTML = '<div class="empty">' + esc(e.message) + "</div>"; }
  });
}

function resultsTable(docs) {
  const cols = new Set();
  docs.forEach(d => Object.keys(d).forEach(k => cols.add(k)));
  const cc = Array.from(cols).slice(0, 14);
  return '<div class="table-wrap"><table><thead><tr>' + cc.map(c => "<th>" + esc(c) + "</th>").join("") + "</tr></thead><tbody>" +
    docs.map(d => "<tr>" + cc.map(c => "<td>" + esc(fmtVal(d[c])) + "</td>").join("") + "</tr>").join("") + "</tbody></table></div>";
}

/* ============ graph route ============ */
function viewGraphRoute() {
  state.coll = null;
  if (typeof window.viewGraph === "function") { window.viewGraph(); return; }
  $("#main").innerHTML = '<div class="empty">graph.js ' + esc(t("common.error")) + "</div>";
}

/* ============ admin: users + roles ============ */
async function viewAdmin() {
  state.coll = null;
  const m = $("#main");
  m.innerHTML =
    "<h1>" + esc(t("admin.title")) + "</h1>" +
    '<p class="page-sub">' + esc(t("admin.perms")) + " · " + esc(t("admin.read")) + " / " + esc(t("admin.write")) + " / fieldDeny</p>" +
    '<div class="io-grid">' +
      '<div class="card"><h3>' + esc(t("admin.users")) + '</h3> <button class="btn small primary" id="u-new">' + esc(t("admin.newUser")) + '</button><div id="u-area" style="margin-top:10px"></div></div>' +
      '<div class="card"><h3>' + esc(t("admin.roles")) + '</h3> <button class="btn small primary" id="r-new">' + esc(t("admin.newRole")) + '</button><div id="r-area" style="margin-top:10px"></div></div>' +
    "</div>";
  $("#u-new").addEventListener("click", async () => {
    let roles = [];
    try { roles = (await api("/api/roles")).roles || []; }
    catch (e) {}
    userForm(null, roles);
  });
  $("#r-new").addEventListener("click", () => roleForm(null));
  loadUsers();
  loadRoles();
}

async function loadUsers() {
  const area = $("#u-area");
  if (!area) return;
  let r;
  try { r = await api("/api/users"); }
  catch (e) { area.innerHTML = '<div class="gs-empty">' + esc(e.message) + "</div>"; return; }
  const users = r.users || [];
  area.innerHTML = users.length
    ? users.map(u => '<div class="idx-row"><span class="idx-name">' + esc(u.username) + "</span><span>" + (u.roles || []).map(x => '<span class="badge ev">' + esc(x) + "</span> ").join("") + "</span>" +
        '<button class="icon-btn" data-edit="' + esc(u.username) + '">✎</button><button class="icon-btn danger" data-del="' + esc(u.username) + '">🗑</button></div>').join("")
    : '<div class="gs-empty">' + esc(t("admin.emptyUsers")) + "</div>";
  $$("[data-del]", area).forEach(b => b.addEventListener("click", () => confirmModal(t("admin.users"), t("admin.delUser"), async () => {
    await api("/api/users/" + encodeURIComponent(b.getAttribute("data-del")), { method: "DELETE" });
    toast(t("docs.deleted")); loadUsers();
  })));
  $$("[data-edit]", area).forEach(b => b.addEventListener("click", async () => {
    let roles = [];
    try { roles = (await api("/api/roles")).roles || []; } catch (e) {}
    const u = (r.users || []).find(x => x.username === b.getAttribute("data-edit"));
    userForm(u, roles);
  }));
}

function userForm(user, allRoles) {
  const isNew = !user;
  user = user || { username: "", roles: [] };
  openModal(
    "<h2>" + esc(isNew ? t("admin.newUser") : t("admin.users") + " · " + user.username) + "</h2>" +
    (isNew ? '<div class="field"><label>' + esc(t("admin.username")) + '</label><input id="m-user"></div>' +
             '<div class="field"><label>' + esc(t("admin.password")) + '</label><input id="m-pass" type="password"></div>' : "") +
    '<div class="field"><label>' + esc(t("admin.rolesOf")) + "</label>" +
      allRoles.map(rl => '<label class="idx-check"><input type="checkbox" value="' + esc(rl.name) + '"' + ((user.roles || []).indexOf(rl.name) >= 0 ? " checked" : "") + "> " + esc(rl.name) + "</label>").join("") +
    '</div><div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + '</button><button class="btn primary" id="m-ok">' + esc(t("editor.save")) + "</button></div>"
  );
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-ok").addEventListener("click", async () => {
    const roles = $$('#modal .idx-check input:checked').map(c => c.value);
    try {
      if (isNew) {
        const username = $("#m-user").value.trim(), pass = $("#m-pass").value;
        if (username.length < 3 || pass.length < 6) { $("#m-err").textContent = "user min 3, pass min 6"; return; }
        await api("/api/users", { method: "POST", body: { username, password: pass, roles } });
      } else {
        await api("/api/users", { method: "PUT", body: { username: user.username, roles } });
      }
      closeModal(); toast(t("common.saved")); loadUsers();
    } catch (e) { $("#m-err").textContent = e.message; }
  });
}

async function loadRoles() {
  const area = $("#r-area");
  if (!area) return;
  let r;
  try { r = await api("/api/roles"); }
  catch (e) { area.innerHTML = '<div class="gs-empty">' + esc(e.message) + "</div>"; return; }
  const roles = r.roles || [];
  area.innerHTML = roles.length
    ? roles.map(rl => '<div class="idx-row"><span class="idx-name">' + esc(rl.name) + "</span><span>" +
        (rl.permissions || []).map(p => esc(p.collection) + (p.read ? " R" : "") + (p.write ? " W" : "") + (p.fieldDeny && p.fieldDeny.length ? " ✋" : "")).join(" · ") + "</span>" +
        '<button class="icon-btn" data-edit="' + esc(rl.name) + '">✎</button><button class="icon-btn danger" data-del="' + esc(rl.name) + '">🗑</button></div>').join("")
    : '<div class="gs-empty">' + esc(t("admin.emptyRoles")) + "</div>";
  $$("[data-del]", area).forEach(b => b.addEventListener("click", () => confirmModal(t("admin.roles"), t("admin.delRole"), async () => {
    await api("/api/roles/" + encodeURIComponent(b.getAttribute("data-del")), { method: "DELETE" });
    toast(t("docs.deleted")); loadRoles();
  })));
  $$("[data-edit]", area).forEach(b => b.addEventListener("click", () => {
    const rl = (r.roles || []).find(x => x.name === b.getAttribute("data-edit"));
    roleForm(rl);
  }));
}

function roleForm(role) {
  const isNew = !role;
  role = role || { name: "", permissions: [{ collection: "", read: true, write: false }] };
  const permRow = (p) => '<div class="idx-row perm-row">' +
    '<input class="p-coll" placeholder="' + esc(t("admin.permColl")) + '" value="' + esc(p.collection || "") + '">' +
    '<label class="idx-check"><input type="checkbox" class="p-read"' + (p.read ? " checked" : "") + "> " + esc(t("admin.read")) + "</label>" +
    '<label class="idx-check"><input type="checkbox" class="p-write"' + (p.write ? " checked" : "") + "> " + esc(t("admin.write")) + "</label>" +
    '<input class="p-deny" placeholder="' + esc(t("admin.fieldDeny")) + '" value="' + esc((p.fieldDeny || []).join(",")) + '" style="width:180px">' +
    '<button class="icon-btn danger p-rm">🗑</button></div>';
  openModal(
    "<h2>" + esc(isNew ? t("admin.newRole") : t("admin.roles") + " · " + role.name) + "</h2>" +
    '<div class="field"><label>' + esc(t("admin.name")) + '</label><input id="m-name" value="' + esc(role.name) + '"' + (isNew ? "" : " disabled") + "></div>" +
    '<div class="field"><label>' + esc(t("admin.perms")) + '</label><div id="m-perms">' + (role.permissions || []).map(permRow).join("") + "</div>" +
    '<button class="btn small" id="m-addperm">' + esc(t("admin.addPerm")) + "</button></div>" +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + '</button><button class="btn primary" id="m-ok">' + esc(t("editor.save")) + "</button></div>"
  );
  const bindRemove = () => $$("#m-perms .p-rm").forEach(b => b.addEventListener("click", () => b.closest(".perm-row").remove()));
  bindRemove();
  $("#m-addperm").addEventListener("click", () => {
    $("#m-perms").insertAdjacentHTML("beforeend", permRow({ collection: "", read: true, write: false }));
    bindRemove();
  });
  $("#m-cancel").addEventListener("click", closeModal);
  $("#m-ok").addEventListener("click", async () => {
    const permissions = $$("#m-perms .perm-row").map(row => ({
      collection: $(".p-coll", row).value.trim(),
      read: $(".p-read", row).checked,
      write: $(".p-write", row).checked,
      fieldDeny: $(".p-deny", row).value.split(",").map(s => s.trim()).filter(Boolean)
    })).filter(p => p.collection);
    if (!permissions.length) { $("#m-err").textContent = t("admin.perms"); return; }
    const name = $("#m-name").value.trim();
    if (name.length < 2) { $("#m-err").textContent = "name min 2"; return; }
    try {
      await api("/api/roles", { method: isNew ? "POST" : "PUT", body: { name, permissions } });
      closeModal(); toast(t("common.saved")); loadRoles();
    } catch (e) { $("#m-err").textContent = e.message; }
  });
}

/* ============ sidebar extras ============ */
$("#coll-toggle").addEventListener("click", () => {
  $("#coll-panel").classList.toggle("hidden");
  $(".coll-section").classList.toggle("open");
});
$("#coll-add").addEventListener("click", (e) => { e.stopPropagation(); newCollModal(); });
$("#coll-search").addEventListener("input", () => {
  const q = ($("#coll-search").value || "").toLowerCase();
  $$("#coll-list a").forEach(a => { a.style.display = a.getAttribute("data-coll").toLowerCase().indexOf(q) >= 0 ? "" : "none"; });
});

/* ============ public API for graph.js ============ */
window.MLS = { api, esc, fmt: fmtVal, fmtVal, t, state, toast, openModal, closeModal, confirmModal };

boot();

})();
