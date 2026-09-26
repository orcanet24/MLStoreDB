(function () {
"use strict";

const { api, t, esc, fmtVal, state, toast, openModal, closeModal, confirmModal } = window.MLS;

const MAX_NODES = 400;

// Coll especial de los nodos-valor ("hubs") de las relaciones por campo:
// cuadrados, con el color definido en la relación (p. ej. "Toyota" ▣).
const HUB_COLL = "valor";

let G = null;

function collColor(name) {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) % 360;
  return "hsl(" + h + ", 62%, 55%)";
}
function typeColor(name) {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 37 + name.charCodeAt(i) * 7) % 360;
  return "hsl(" + h + ", 75%, 62%)";
}

function keyOf(coll, id) { return coll + "/" + id; }

// PALETTE/labelColor/randColor: colores hex para las relaciones por campo.
// labelColor da un color estable por hash del label (relaciones antiguas sin
// campo color); randColor elige uno fijo al crear una nueva (editable luego).
const PALETTE = ["#4f8cff", "#7ee0a3", "#ffb454", "#ff6b6b", "#b48cff", "#3fd0d4", "#f47cc3", "#9adc4a"];
function labelColor(label) {
  let h = 0;
  const s = String(label || "");
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % PALETTE.length;
  return PALETTE[h];
}
function randColor() { return PALETTE[Math.floor(Math.random() * PALETTE.length)]; }

function viewGraph() {
  state.coll = null;
  const main = document.getElementById("main");
  main.innerHTML =
    "<h1>" + t("graph.title") + ' <small>· ' + t("graph.sub") + "</small></h1>" +
    '<div class="graph-toolbar">' +
    '<input id="g-search" class="grow" placeholder="' + esc(t("graph.searchPh")) + '">' +
    '<button class="btn" id="g-add">' + t("graph.add") + "</button>" +
    '<span class="g-sep"></span>' +
    '<button class="btn" id="g-traverse">' + t("graph.traverse") + "</button>" +
    '<button class="btn" id="g-path">' + t("graph.path") + "</button>" +
    '<span class="g-sep"></span>' +
    '<button class="btn" id="g-group">' + t("graph.group") + "</button>" +
    '<button class="btn" id="g-links">🔗 ' + t("graph.links") + "</button>" +
    '<button class="btn danger" id="g-clear">' + t("graph.clear") + "</button>" +
    "</div>" +
    '<div class="graph-wrap">' +
    '<div class="graph-side" id="g-side"></div>' +
    '<canvas id="g-canvas"></canvas>' +
    '<div class="graph-legend" id="g-legend"></div>' +
    '<div class="graph-hint">' + esc(t("graph.dragHint")) + "</div>" +
    '<div class="graph-badge" id="g-count"></div>' +
    "</div>";

  G = {
    nodes: new Map(),
    edges: new Map(),
    edgeTypes: [],
    vertexColls: [],
    activeTypes: new Set(),
    selected: null,
    highlightNodes: new Set(),
    highlightEdges: new Set(),
    pickMode: null,
    groupMode: false,
    alpha: 0,
    pan: { x: 0, y: 0 },
    zoom: 1,
    drag: null,
    connectFrom: null,
    mouse: { x: 0, y: 0 },
    raf: 0,
    canvas: document.getElementById("g-canvas"),
    ctx: document.getElementById("g-canvas").getContext("2d"),
    // Relaciones por campo (links): vínculos derivados del valor de un campo,
    // persistidos por BD en localStorage. fieldCache = /fields por colección.
    links: [],
    fieldCache: new Map(),
    // byId: índice _id → nodo para lookups O(1) en física/render (antes O(N)
    // por arista, que era el principal consumo de CPU con muchos enlaces).
    byId: new Map(),
    chromePending: false,
  };
  loadLinks();

  api("/api/graph").then((meta) => {
    G.edgeTypes = meta.edgeTypes || [];
    G.vertexColls = meta.vertexColls || [];
    G.edgeTypes.forEach((e) => G.activeTypes.add(e));
    renderLegend();
    renderSide();
    // Agrupación automática de las relaciones activas persistidas: al abrir
    // la vista ya se ven los grupos (nodos-valor) sin pulsar nada.
    (async () => {
      for (const l of G.links.filter((x) => x.enabled)) {
        if (!G || !G.nodes) return;
        try { await autoGroup(l, true); } catch (e) { /* vista cambiada */ }
      }
    })();
  }).catch((e) => toast(e.message, true));

  document.getElementById("g-add").addEventListener("click", doSearch);
  document.getElementById("g-search").addEventListener("keydown", (e) => {
    if (e.key === "Enter") doSearch();
  });
  document.getElementById("g-links").addEventListener("click", linksModal);
  document.getElementById("g-traverse").addEventListener("click", doTraverse);
  document.getElementById("g-path").addEventListener("click", startPathPick);
  document.getElementById("g-group").addEventListener("click", () => {
    G.groupMode = !G.groupMode;
    document.getElementById("g-group").classList.toggle("primary", G.groupMode);
    G.alpha = 1;
  });
  document.getElementById("g-clear").addEventListener("click", () => {
    G.nodes.clear();
    G.edges.clear();
    G.byId.clear();
    G.selected = null;
    G.highlightNodes.clear();
    G.highlightEdges.clear();
    G.alpha = 0;
    renderSide();
    updateCount();
  });

  bindCanvas();
  resize();
  window.addEventListener("resize", resize);
  loop();
}
window.viewGraph = viewGraph;
function resize() {
  if (!G || !G.canvas) return;
  const rect = G.canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  G.canvas.width = rect.width * dpr;
  G.canvas.height = rect.height * dpr;
  G.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  G.w = rect.width;
  G.h = rect.height;
}

function renderLegend() {
  const el = document.getElementById("g-legend");
  if (!el) return;
  let html = "";
  for (const c of new Set(Array.from(G.nodes.values()).filter((n) => !n.hub).map((n) => n.coll))) {
    html += '<span class="lg"><i style="background:' + collColor(c) + '"></i>' + esc(c) + "</span>";
  }
  for (const ty of G.activeTypes) {
    html += '<span class="lg"><i class="line" style="background:' + typeColor(ty) + '"></i>' + esc(ty) + "</span>";
  }
  for (const l of G.links || []) {
    if (!l.enabled) continue;
    const lc = l.color || typeColor(l.label);
    html += '<span class="lg"><i class="line" style="background:' + lc + '"></i>' + esc(l.label) + "</span>";
  }
  el.innerHTML = html;
}

// scheduleChrome aplaza updateCount+renderLegend al siguiente frame: durante
// agrupaciones masivas evita reconstruir la leyenda por cada nodo/arista.
function scheduleChrome() {
  if (!G || G.chromePending) return;
  G.chromePending = true;
  requestAnimationFrame(() => {
    if (!G) return;
    G.chromePending = false;
    updateCount();
    renderLegend();
  });
}

function updateCount() {
  const el = document.getElementById("g-count");
  if (el) el.textContent = G.nodes.size + " " + t("graph.nodes") + " · " + G.edges.size + " " + t("graph.edges");
}

function addNode(coll, id, doc, x, y) {
  const key = keyOf(coll, id);
  if (G.nodes.has(key)) {
    if (doc) G.nodes.get(key).doc = doc;
    return G.nodes.get(key);
  }
  if (G.nodes.size >= MAX_NODES) return null;
  const cx = x !== undefined ? x : (G.w || 800) / 2 + (Math.random() - 0.5) * 120;
  const cy = y !== undefined ? y : (G.h || 500) / 2 + (Math.random() - 0.5) * 120;
  const n = { key, coll, id, doc: doc || null, x: cx, y: cy, vx: 0, vy: 0, fixed: false };
  G.nodes.set(key, n);
  if (!G.byId.has(id)) G.byId.set(id, n);
  G.alpha = 1;
  scheduleChrome();
  return n;
}

function addEdge(e) {
  if (G.edges.has(e.id)) return;
  G.edges.set(e.id, e);
  G.alpha = 1;
  scheduleChrome();
}

function renderSide() {
  const side = document.getElementById("g-side");
  if (!side) return;
  if (!G.selected) {
    side.innerHTML = '<div class="gs-empty">' +
      (G.nodes.size === 0 ? t("graph.empty") : t("graph.sideHint")) + "</div>";
    return;
  }
  const n = G.selected;
  const hub = !!n.hub;
  const hubLink = hub ? (G.links || []).find((l) => l.id === n.linkId) : null;
  const edges = Array.from(G.edges.values()).filter((e) => e.from === n.id || e.to === n.id);
  // Otro extremo de una arista: si apunta a un nodo-valor muestra el valor.
  const otherLabel = (id) => {
    const nd = nodeById(id);
    return nd && nd.hub ? "▣ " + nd.val : id;
  };
  side.innerHTML =
    '<div class="gs-head"><i style="background:' + (hub ? n.vcolor : collColor(n.coll)) + '"></i>' +
    "<b>" + esc(hub ? n.val : n.id) + "</b><small>" +
    esc(hub ? "🔗 " + (hubLink ? hubLink.label : "") : n.coll) + "</small></div>" +
    '<div class="gs-actions">' +
    '<button class="btn small" id="gs-expand">⤢ ' + t("graph.expand") + "</button>" +
    (hub ? "" :
      '<button class="btn small" id="gs-connect">↔ ' + t("graph.connect") + "</button>" +
      '<button class="btn small danger" id="gs-deldoc">🗑 ' + t("graph.deleteDoc") + "</button>") +
    "</div>" +
    '<div class="gs-doc"><pre>' + esc(JSON.stringify(
      hub ? { value: n.val, link: hubLink ? hubLink.label : "", type: hubLink ? hubLink.type : "" }
          : (n.doc || { _id: n.id }), null, 2)) + "</pre></div>" +
    "<h4>" + t("graph.edges") + " (" + edges.length + ")</h4>" +
    '<div class="gs-edges">' + (edges.map((e) =>
      '<div class="gs-edge"><i style="background:' + (e.lcolor || typeColor(e.type)) + '"></i>' +
      "<span>" + (e.from === n.id ? "→ " : "← ") + esc(otherLabel(e.from === n.id ? e.to : e.from)) + "</span>" +
      '<button class="icon-btn danger" data-deledge="' + esc(e.id) + '" data-deltype="' + esc(e.type) + '">🗑</button></div>'
    ).join("") || '<div class="gs-empty">—</div>') + "</div>";

  document.getElementById("gs-expand").addEventListener("click", () =>
    hub ? expandHub(n) : expandNode(n));
  const btnConnect = document.getElementById("gs-connect");
  if (btnConnect) btnConnect.addEventListener("click", () => {
    G.connectFrom = n;
    toast(t("graph.connectTo"));
  });
  const btnDel = document.getElementById("gs-deldoc");
  if (btnDel) btnDel.addEventListener("click", () => {
    confirmModal(t("graph.deleteDoc"), n.id, async () => {
      await api("/api/collections/" + encodeURIComponent(n.coll) + "/doc?id=" + encodeURIComponent(n.id), { method: "DELETE" });
      removeNode(n.key);
      toast(window.MLS.t("common.deleted"));
    });
  });
  side.querySelectorAll("[data-deledge]").forEach((b) => {
    b.addEventListener("click", async () => {
      const e = G.edges.get(b.dataset.deledge);
      if (e && e.virtual) {
        // Arista derivada de una relación por campo: solo existe en el canvas.
        G.edges.delete(e.id);
      } else {
        await api("/api/graph/edge?type=" + encodeURIComponent(b.dataset.deltype) + "&id=" + encodeURIComponent(b.dataset.deledge), { method: "DELETE" });
        G.edges.delete(b.dataset.deledge);
      }
      renderSide();
      updateCount();
      renderLegend();
    });
  });
}

function removeNode(key) {
  const n = G.nodes.get(key);
  if (!n) return;
  for (const [eid, e] of Array.from(G.edges)) {
    if (e.from === n.id || e.to === n.id) G.edges.delete(eid);
  }
  G.nodes.delete(key);
  // Reconstruye el índice id→node (primer nodo con ese _id, como antes).
  G.byId.clear();
  for (const m of G.nodes.values()) if (!G.byId.has(m.id)) G.byId.set(m.id, m);
  if (G.selected && G.selected.key === key) G.selected = null;
  renderSide();
  updateCount();
  renderLegend();
}

async function doSearch() {
  const input = document.getElementById("g-search");
  const id = input.value.trim();
  if (!id) return;
  try {
    const r = await api("/api/graph/resolve", { method: "POST", body: { ids: [id], colls: G.vertexColls } });
    const found = r.found || [];
    if (!found.length) { toast(t("graph.notFound"), true); return; }
    let added = null;
    for (const f of found) {
      added = addNode(f.coll, f.id, f.doc) || added;
    }
    if (added) {
      G.selected = added;
      G.pan.x = 0; G.pan.y = 0; G.zoom = 1;
      renderSide();
    } else {
      toast(t("graph.limitReached"), true);
    }
    input.value = "";
  } catch (e) { toast(e.message, true); }
}

async function expandNode(n) {
  const types = Array.from(G.activeTypes);
  const hasLinks = G.links.some((l) => l.enabled);
  if (!types.length && !hasLinks) { toast(t("graph.noTypes"), true); return; }
  const newIds = [];
  for (const ty of types) {
    try {
      const r = await api("/api/graph/neighbors?edge=" + encodeURIComponent(ty) + "&vertex=" + encodeURIComponent(n.id));
      for (const e of r.edges || []) {
        addEdge({ id: e.id, type: ty, from: e.from, to: e.to, props: e.props });
      }
      for (const id of r.neighbors || []) newIds.push(id);
    } catch (err) { toast(err.message, true); }
  }
  if (newIds.length) await resolveAndAdd(newIds);
  await expandLinks(n);
}

async function resolveAndAdd(ids) {
  const unique = Array.from(new Set(ids));
  try {
    const r = await api("/api/graph/resolve", { method: "POST", body: { ids: unique, colls: G.vertexColls } });
    let added = 0;
    for (const f of r.found || []) {
      if (addNode(f.coll, f.id, f.doc)) added++;
    }
    const missing = unique.filter((id) => !Array.from(G.nodes.values()).some((n) => n.id === id));
    for (const id of missing) {
      if (addNode("_", id, null)) { G.nodes.get("_/" + id).ghost = true; added++; }
    }
    if (!added) toast(t("graph.limitReached"), true);
  } catch (e) { toast(e.message, true); }
}

function doTraverse() {
  if (!G.selected) { toast(t("graph.selectFirst"), true); return; }
  openModal(
    "<h2>" + t("graph.traverse") + " · " + esc(G.selected.id) + "</h2>" +
    '<div class="field"><label>' + t("graph.depth") + ' (1–10)</label><input id="m-depth" type="number" min="1" max="10" value="2"></div>' +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + t("common.cancel") + '</button>' +
    '<button class="btn primary" id="m-ok">OK</button></div>'
  );
  document.getElementById("m-cancel").addEventListener("click", closeModal);
  document.getElementById("m-ok").addEventListener("click", async () => {
    const depth = parseInt(document.getElementById("m-depth").value, 10) || 2;
    closeModal();
    const types = Array.from(G.activeTypes);
    if (!types.length) { toast(t("graph.noTypes"), true); return; }
    const ids = [];
    for (const ty of types) {
      try {
        const r = await api("/api/graph/traverse?edge=" + encodeURIComponent(ty) +
          "&start=" + encodeURIComponent(G.selected.id) + "&maxDepth=" + depth + "&limit=200");
        for (const nd of r.nodes || []) if (nd.vertex) ids.push(nd.vertex);
      } catch (e) { toast(e.message, true); }
    }
    await resolveAndAdd(ids);
    G.highlightNodes.clear();
    G.highlightEdges.clear();
    for (const id of ids) {
      for (const n of G.nodes.values()) if (n.id === id) G.highlightNodes.add(n.key);
    }
  });
}

function startPathPick() {
  if (!G.selected) { toast(t("graph.selectFirst"), true); return; }
  G.pickMode = { from: G.selected, stage: 2 };
  toast(t("graph.pickTo"));
}

async function runPath(from, to) {
  const types = Array.from(G.activeTypes);
  if (!types.length) { toast(t("graph.noTypes"), true); return; }
  G.highlightNodes.clear();
  G.highlightEdges.clear();
  let path = null;
  for (const ty of types) {
    try {
      const r = await api("/api/graph/path?edge=" + encodeURIComponent(ty) +
        "&from=" + encodeURIComponent(from.id) + "&to=" + encodeURIComponent(to.id) + "&maxDepth=8");
      if (r.path && r.path.length) { path = r.path; break; }
    } catch (e) { /* try next type */ }
  }
  if (!path) { toast(t("graph.noPath"), true); return; }
  await resolveAndAdd(path);
  const keys = [];
  for (const id of path) {
    for (const n of G.nodes.values()) if (n.id === id) { G.highlightNodes.add(n.key); keys.push(n); }
  }
  for (let i = 0; i < keys.length - 1; i++) {
    for (const e of G.edges.values()) {
      if ((e.from === keys[i].id && e.to === keys[i + 1].id) || (e.to === keys[i].id && e.from === keys[i + 1].id)) {
        G.highlightEdges.add(e.id);
      }
    }
  }
}

/* ---------- canvas: coordinates ---------- */

function worldToScreen(x, y) {
  return { x: (x - G.w / 2) * G.zoom + G.w / 2 + G.pan.x, y: (y - G.h / 2) * G.zoom + G.h / 2 + G.pan.y };
}
function screenToWorld(x, y) {
  return { x: (x - G.pan.x - G.w / 2) / G.zoom + G.w / 2, y: (y - G.pan.y - G.h / 2) / G.zoom + G.h / 2 };
}

function bindCanvas() {
  const c = G.canvas;
  c.addEventListener("mousedown", (ev) => {
    const p = ev.offsetX === undefined ? { x: ev.layerX, y: ev.layerY } : { x: ev.offsetX, y: ev.offsetY };
    const n = hitNode(p.x, p.y);
    if (ev.shiftKey && n) {
      G.connectFrom = n;
      G.drag = null;
      return;
    }
    if (n) {
      G.drag = { node: n, dx: p.x, dy: p.y };
      n.fixed = true;
    } else {
      G.drag = { pan: true, x: p.x, y: p.y, panX: G.pan.x, panY: G.pan.y };
    }
  });
  c.addEventListener("mousemove", (ev) => {
    const p = ev.offsetX === undefined ? { x: ev.layerX, y: ev.layerY } : { x: ev.offsetX, y: ev.offsetY };
    G.mouse = p;
    if (!G.drag) return;
    if (G.drag.pan) {
      G.pan.x = G.drag.panX + (p.x - G.drag.x);
      G.pan.y = G.drag.panY + (p.y - G.drag.y);
    } else {
      const w = screenToWorld(p.x, p.y);
      G.drag.node.x = w.x;
      G.drag.node.y = w.y;
      G.drag.node.vx = 0;
      G.drag.node.vy = 0;
      G.alpha = Math.max(G.alpha, 0.3);
    }
  });
  c.addEventListener("mouseup", (ev) => {
    const p = ev.offsetX === undefined ? { x: ev.layerX, y: ev.layerY } : { x: ev.offsetX, y: ev.offsetY };
    if (G.connectFrom) {
      const target = hitNode(p.x, p.y);
      if (target && target !== G.connectFrom) {
        edgeModal(G.connectFrom, target);
      }
      G.connectFrom = null;
    }
    if (G.drag && G.drag.node) G.drag.node.fixed = false;
    G.drag = null;
  });
  c.addEventListener("click", (ev) => {
    const p = ev.offsetX === undefined ? { x: ev.layerX, y: ev.layerY } : { x: ev.offsetX, y: ev.offsetY };
    const n = hitNode(p.x, p.y);
    if (G.pickMode && G.pickMode.stage === 2) {
      if (n) {
        runPath(G.pickMode.from, n);
        G.pickMode = null;
        return;
      }
    }
    G.selected = n || null;
    renderSide();
  });
  c.addEventListener("dblclick", (ev) => {
    const p = ev.offsetX === undefined ? { x: ev.layerX, y: ev.layerY } : { x: ev.offsetX, y: ev.offsetY };
    const n = hitNode(p.x, p.y);
    if (!n) return;
    // Nodo-valor → carga más documentos con ese valor; documento → expansión.
    if (n.hub) expandHub(n);
    else expandNode(n);
  });
  c.addEventListener("wheel", (ev) => {
    ev.preventDefault();
    const factor = ev.deltaY < 0 ? 1.12 : 0.89;
    G.zoom = Math.min(3, Math.max(0.15, G.zoom * factor));
  }, { passive: false });
}

function hitNode(sx, sy) {
  let best = null, bestD = 1e9;
  for (const n of G.nodes.values()) {
    const s = worldToScreen(n.x, n.y);
    const d = Math.hypot(s.x - sx, s.y - sy);
    if (d < 26 * G.zoom + 6 && d < bestD) { best = n; bestD = d; }
  }
  return best;
}

function edgeModal(from, to) {
  const typeOpts = (G.edgeTypes.length ? G.edgeTypes : ["knows"]).map((ty) =>
    '<option value="' + esc(ty) + '">' + esc(ty) + "</option>").join("");
  openModal(
    "<h2>" + t("graph.newEdge") + "</h2>" +
    '<p class="page-sub" style="margin-bottom:12px"><b>' + esc(from.id) + "</b> → <b>" + esc(to.id) + "</b></p>" +
    '<div class="field"><label>' + t("graph.nodeType") + '</label><select id="m-type">' + typeOpts + "</select></div>" +
    '<div class="field"><label>' + t("graph.props") + ' (JSON, opcional)</label><textarea id="m-props" style="min-height:90px">{}</textarea></div>' +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + t("common.cancel") + '</button>' +
    '<button class="btn primary" id="m-ok">' + t("editor.save") + "</button></div>"
  );
  document.getElementById("m-cancel").addEventListener("click", closeModal);
  document.getElementById("m-ok").addEventListener("click", async () => {
    let props = {};
    try { props = JSON.parse(document.getElementById("m-props").value || "{}"); }
    catch (err) { document.getElementById("m-err").textContent = t("editor.invalid") + err.message; return; }
    try {
      const r = await api("/api/graph/edge", { method: "POST", body: { type: document.getElementById("m-type").value, from: from.id, to: to.id, props } });
      addEdge({ id: r.id, type: document.getElementById("m-type").value, from: from.id, to: to.id, props });
      closeModal();
      toast(t("common.saved"));
      renderSide();
    } catch (err) { document.getElementById("m-err").textContent = err.message; }
  });
}

/* ---------- relaciones por campo (links) ---------- */

// Relación derivada del valor de un campo (no usa colecciones edges.*):
//   mode "pivot":  1..N parejas [{coll, field}]; todos los documentos con el
//                  mismo valor en SU campo quedan conectados. Con una sola
//                  colección agrupa (productos por marca, zonas…); con varias
//                  cruza universos y cada colección usa su propio nombre de
//                  campo (p. ej. products.brand ⇄ marks.name).
//   mode "manual": link.from.coll.link.from.field ⇄ link.to.coll.link.to.field,
//                  ambos campos del mismo tipo (string o number, cubre float);
//                  origen y destino pueden ser la misma colección.
// link.type es el tipo común validado en /fields al crearla.

function linksKey() { return "mls-graph-links:" + (state.db || "default"); }

function loadLinks() {
  try {
    const raw = localStorage.getItem(linksKey());
    G.links = raw ? JSON.parse(raw) : [];
  } catch (e) { G.links = []; }
  if (!Array.isArray(G.links)) G.links = [];
  let dirty = false;
  // Migración: pivot antiguo {colls, field} → pares [{coll, field}] (campo
  // propio por colección y mínimo 1 colección).
  for (const l of G.links) {
    if (l && l.mode === "pivot" && !Array.isArray(l.pairs)) {
      l.pairs = (Array.isArray(l.colls) ? l.colls : []).map((c) => ({ coll: c, field: l.field }));
      delete l.colls;
      delete l.field;
      dirty = true;
    }
    // Migración: relaciones sin color → color hex estable por label.
    if (l && !l.color) { l.color = labelColor(l.label); dirty = true; }
  }
  if (dirty) saveLinks();
}

function saveLinks() {
  try { localStorage.setItem(linksKey(), JSON.stringify(G.links)); } catch (e) { /* cuota llena */ }
}

function newLinkId() {
  return "lnk-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2, 6);
}

// linkPairs devuelve las parejas coll.campo de una relación pivot.
function linkPairs(link) {
  return Array.isArray(link.pairs) ? link.pairs : [];
}

function linkSpec(link) {
  if (link.mode === "pivot") return linkPairs(link).map((p) => p.coll + "." + p.field).join(" ⇄ ");
  return link.from.coll + "." + link.from.field + " ⇄ " + link.to.coll + "." + link.to.field;
}

// getLinkVal lee un path con punto del documento con la misma semántica que el
// motor de filtros (arrays intermedios se expanden, p. ej. items.sku).
function getLinkVal(obj, path) {
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

// fieldTypeInfo cachea /fields (campos + tipos observados) por colección.
async function fieldTypeInfo(coll) {
  if (G.fieldCache.has(coll)) return G.fieldCache.get(coll);
  const r = await api("/api/collections/" + encodeURIComponent(coll) + "/fields?maxScan=2000");
  const info = { fields: r.fields || [], types: r.types || {} };
  G.fieldCache.set(coll, info);
  return info;
}

// linkFieldType devuelve el tipo observado del campo o null si no existe.
function linkFieldType(info, field) {
  if (field === "_id") return "string";
  if (!info.fields.includes(field) && !Object.prototype.hasOwnProperty.call(info.types, field)) return null;
  return info.types[field] || "null"; // sin tipo observado (todo null)
}

// validateLinkFields comprueba que cada par [coll, field] exista y que todos
// compartan el mismo tipo permitido (string o number). Devuelve {type} o {err}.
async function validateLinkFields(pairs) {
  let ty = null, tyLabel = "";
  const missing = [];
  for (const pair of pairs) {
    const coll = pair[0], field = pair[1];
    if (!field || field[0] === "$") return { err: t("graph.linkFieldReq") };
    const info = await fieldTypeInfo(coll);
    const ct = linkFieldType(info, field);
    if (ct === null) { missing.push(coll); continue; }
    if (ty === null) { ty = ct; tyLabel = coll + "." + field; }
    else if (ct !== ty) {
      return { err: t("graph.linkTypeErr") + tyLabel + "=" + ty + ", " + coll + "." + field + "=" + ct };
    }
  }
  if (missing.length) return { err: t("graph.linkMissing") + missing.join(", ") };
  if (ty !== "string" && ty !== "number") {
    return { err: t("graph.linkTypeErr") + (ty || "?") };
  }
  return { type: ty };
}

// linkQueries devuelve los objetivos (coll, field) donde buscar el valor del
// nodo, con el campo de origen (src) que hay que leer de su documento. null =
// la relación no aplica a esta colección.
function linkQueries(link, coll) {
  if (link.mode === "pivot") {
    const pairs = linkPairs(link);
    const src = pairs.find((p) => p.coll === coll);
    if (!src) return null; // la relación no aplica a esta colección
    // El valor se lee del campo propio de la colección del nodo y se busca en
    // el campo de cada pareja (cada colección puede llamarlo distinto).
    return pairs.map((p) => ({ coll: p.coll, field: p.field, src: src.field }));
  }
  const out = [];
  if (coll === link.from.coll) out.push({ coll: link.to.coll, field: link.to.field, src: link.from.field });
  if (coll === link.to.coll && (link.from.coll !== link.to.coll || link.from.field !== link.to.field)) {
    out.push({ coll: link.from.coll, field: link.from.field, src: link.to.field });
  }
  return out.length ? out : null;
}

// linkFind busca en coll los documentos cuyo field == value (máx. 100).
async function linkFind(coll, field, value) {
  const r = await api("/api/collections/" + encodeURIComponent(coll) + "/docs?limit=100&filter=" +
    encodeURIComponent(JSON.stringify({ [field]: value })));
  return r.docs || [];
}

// linkValues normaliza lo leído de un documento a la lista de valores válidos
// del tipo de la relación (string/number; un array expande a varios valores).
function linkValues(link, raw) {
  const out = [];
  const list = Array.isArray(raw) ? raw : [raw];
  for (const v of list) {
    if (v === undefined || v === null || typeof v === "object") continue;
    if (link.type === "number" && typeof v !== "number") continue;
    if (link.type === "string" && typeof v !== "string") continue;
    out.push(v);
  }
  return out;
}

// ensureHub crea/recupera el nodo-valor (cuadrado, con el color de la
// relación) que agrupa todos los documentos con ese valor, p. ej. Toyota ▣.
function ensureHub(link, value) {
  const hid = "#v:" + link.id + ":" + value;
  const key = keyOf(HUB_COLL, hid);
  const prev = G.nodes.get(key);
  if (prev) return prev;
  const n = addNode(HUB_COLL, hid, null);
  if (!n) return null;
  n.hub = true;
  n.linkId = link.id;
  n.val = String(value);
  n.vcolor = link.color || typeColor(link.label);
  return n;
}

// addValEdge conecta un documento con el nodo-valor de su relación (arista
// virtual punteada que solo existe en el canvas, nunca en la API).
function addValEdge(link, coll, id, hub) {
  if (!hub) return;
  const k = keyOf(coll, id);
  if (!G.nodes.has(k)) return;
  addEdge({
    id: "L:" + link.id + ":" + k + "→" + hub.key,
    type: link.label,
    from: id,
    to: hub.id,
    virtual: true,
    lcolor: link.color || typeColor(link.label),
  });
}

// removeLinkGraph borra las aristas y nodos-valor de una relación (al
// desactivarla o eliminarla): es todo cliente, sin llamadas a la API.
function removeLinkGraph(link) {
  const pfx = "L:" + link.id + ":";
  for (const eid of Array.from(G.edges.keys())) {
    if (eid.startsWith(pfx)) G.edges.delete(eid);
  }
  for (const n of Array.from(G.nodes.values())) {
    if (n.hub && n.linkId === link.id) removeNode(n.key);
  }
  G.alpha = 1;
  updateCount();
  renderLegend();
}

// expandLinks conecta el nodo con su nodo-valor y con los documentos que
// comparten valor en las relaciones activas (además de las aristas edges.*).
async function expandLinks(n) {
  const active = G.links.filter((l) => l.enabled);
  if (!active.length) return;
  let doc = n.doc;
  if (!doc && n.coll && n.coll !== "_") {
    try {
      const r = await api("/api/collections/" + encodeURIComponent(n.coll) + "/doc?id=" + encodeURIComponent(n.id));
      doc = r.doc || null;
      if (doc) n.doc = doc;
    } catch (e) { return; }
  }
  if (!doc) return;
  for (const link of active) {
    const qs = linkQueries(link, n.coll);
    if (!qs) continue;
    try {
      for (const q of qs) {
        const vals = linkValues(link, getLinkVal(doc, q.src));
        for (const v of vals) {
          const hub = ensureHub(link, v);
          if (!hub) { toast(t("graph.limitReached"), true); return; }
          addValEdge(link, n.coll, n.id, hub);
          const docs = await linkFind(q.coll, q.field, v);
          for (const m of docs) {
            const mid = m["_id"];
            if (typeof mid !== "string") continue;
            if (q.coll === n.coll && mid === n.id) continue;
            if (!addNode(q.coll, mid, m)) continue;
            addValEdge(link, q.coll, mid, hub);
          }
        }
      }
    } catch (e) { toast(e.message, true); }
  }
  renderLegend();
}

// autoGroup agrupa automáticamente todos los valores actuales del campo:
// pagina los documentos de cada pareja (200/página, máx. 5 páginas) y crea un
// nodo-valor por cada valor distinto, conectando cada registro. Se ejecuta al
// crear la relación, al activarla y al abrir la vista. El tope de nodos es
// MAX_NODES (el alcance queda parcial si se alcanza).
async function autoGroup(link, quiet) {
  const qs = link.mode === "pivot" ? linkPairs(link) : [link.from, link.to];
  let addedDocs = 0, truncated = false;
  outer: for (const q of qs) {
    for (let skip = 0; skip < 1000; skip += 200) {
      if (!G || !G.nodes) return;
      let docs = [];
      try {
        const r = await api("/api/collections/" + encodeURIComponent(q.coll) + "/docs?limit=200&skip=" + skip);
        docs = r.docs || [];
      } catch (e) { if (!quiet) toast(e.message, true); return; }
      for (const m of docs) {
        const mid = m["_id"];
        if (typeof mid !== "string") continue;
        const vals = linkValues(link, getLinkVal(m, q.field));
        if (!vals.length) continue;
        const isNew = !G.nodes.has(keyOf(q.coll, mid));
        if (isNew && G.nodes.size >= MAX_NODES) { truncated = true; break outer; }
        const nd = addNode(q.coll, mid, m);
        if (!nd) { truncated = true; break outer; }
        if (isNew) addedDocs++;
        for (const v of vals) {
          const hub = ensureHub(link, v);
          if (!hub) { truncated = true; break outer; }
          addValEdge(link, q.coll, mid, hub);
        }
      }
      if (docs.length < 200) break;
    }
  }
  if (!G || !G.nodes) return;
  G.alpha = 1;
  renderLegend();
  updateCount();
  if (!quiet) {
    const hubsN = Array.from(G.nodes.values()).filter((n) => n.hub && n.linkId === link.id).length;
    let msg;
    if (truncated) msg = t("graph.linkGroupTrunc") + " (" + addedDocs + " + " + hubsN + " " + t("graph.linkValCount") + ")";
    else if (addedDocs) msg = t("graph.linkGrouped") + addedDocs + " · " + hubsN + " " + t("graph.linkValCount");
    else msg = t("graph.linkGroupNone");
    toast(msg, !addedDocs);
  }
}

// expandHub carga más documentos que comparten el valor de un nodo-valor
// (doble clic sobre el cuadrado, o botón ⤢ con un nodo-valor seleccionado).
async function expandHub(hub) {
  const link = (G.links || []).find((l) => l.id === hub.linkId);
  if (!link) return;
  const qs = link.mode === "pivot" ? linkPairs(link) : [link.from, link.to];
  const value = link.type === "number" ? Number(hub.val) : hub.val;
  let found = 0;
  for (const q of qs) {
    try {
      const docs = await linkFind(q.coll, q.field, value);
      for (const m of docs) {
        const mid = m["_id"];
        if (typeof mid !== "string") continue;
        if (!addNode(q.coll, mid, m)) continue;
        addValEdge(link, q.coll, mid, hub);
        found++;
      }
    } catch (e) { toast(e.message, true); return; }
  }
  G.alpha = 1;
  renderLegend();
  if (!found) toast(t("graph.linkSeedEmpty"), true);
}

function renderLinksList() {
  const el = document.getElementById("m-lnk-list");
  if (!el) return;
  if (!G.links.length) {
    el.innerHTML = '<div class="lnk-empty">' + esc(t("graph.linkEmpty")) + "</div>";
    return;
  }
  el.innerHTML = G.links.map((l, i) =>
    '<div class="lnk-item' + (l.enabled ? "" : " off") + '">' +
    '<input type="checkbox" data-lnk-toggle="' + i + '"' + (l.enabled ? " checked" : "") +
    ' title="' + esc(t("graph.linkEnable")) + '">' +
    '<span class="lnk-spec"><input type="color" class="lnk-color" data-lnk-color="' + i +
    '" value="' + esc(l.color || labelColor(l.label)) + '" title="' + esc(t("graph.linkColor")) + '"><b>' +
    esc(l.label) + "</b> · " + esc(linkSpec(l)) + "</span>" +
    '<button class="icon-btn" data-lnk-seed="' + i + '" title="' + esc(t("graph.linkSeed")) + '">🔍</button>' +
    '<button class="icon-btn danger" data-lnk-del="' + i + '" title="' + esc(t("graph.linkDel")) + '">🗑</button>' +
    "</div>").join("");
  el.querySelectorAll("[data-lnk-toggle]").forEach((b) => {
    b.addEventListener("change", () => {
      const l = G.links[+b.dataset.lnkToggle];
      l.enabled = b.checked;
      saveLinks();
      renderLinksList();
      renderLegend();
      // Activar agrupa automáticamente; desactivar retira sus grupos.
      if (l.enabled) autoGroup(l).catch((e) => toast(e.message, true));
      else removeLinkGraph(l);
    });
  });
  el.querySelectorAll("[data-lnk-color]").forEach((inp) => {
    inp.addEventListener("change", () => {
      const l = G.links[+inp.dataset.lnkColor];
      l.color = inp.value;
      saveLinks();
      // Propaga el nuevo color a nodos-valor y aristas ya dibujados.
      const pfx = "L:" + l.id + ":";
      for (const n of G.nodes.values()) if (n.hub && n.linkId === l.id) n.vcolor = l.color;
      for (const e of G.edges.values()) if (e.id.startsWith(pfx)) e.lcolor = l.color;
      renderLinksList();
      renderLegend();
    });
  });
  el.querySelectorAll("[data-lnk-del]").forEach((b) => {
    b.addEventListener("click", () => {
      const l = G.links[+b.dataset.lnkDel];
      removeLinkGraph(l);
      G.links.splice(+b.dataset.lnkDel, 1);
      saveLinks();
      renderLinksList();
      renderLegend();
      toast(t("graph.linkDeleted"));
    });
  });
  el.querySelectorAll("[data-lnk-seed]").forEach((b) => {
    b.addEventListener("click", () => seedLink(G.links[+b.dataset.lnkSeed]));
  });
}

function linkPairRow(prefix) {
  const opts = G.vertexColls.map((c) => '<option value="' + esc(c) + '">' + esc(c) + "</option>").join("");
  return '<div class="lnk-pair"><select id="' + prefix + '-coll">' + opts + '</select>' +
    '<input id="' + prefix + '-field" placeholder="' + esc(t("graph.linkField")) + '"></div>';
}

function linksModal() {
  openModal(
    "<h2>🔗 " + esc(t("graph.links")) + "</h2>" +
    '<p class="lnk-hint">' + esc(t("graph.linksHint")) + "</p>" +
    '<div class="lnk-list" id="m-lnk-list"></div>' +
    '<hr class="lnk-hr">' +
    '<div class="field"><label>' + esc(t("graph.linkMode")) + '</label><select id="m-lnk-mode">' +
    '<option value="pivot">' + esc(t("graph.linkPivot")) + "</option>" +
    '<option value="manual">' + esc(t("graph.linkManual")) + "</option></select></div>" +
    '<div id="m-lnk-pivot">' +
    '<div class="field"><label>' + esc(t("graph.linkColls")) + '</label>' +
    '<div class="lnk-rows" id="m-lnk-rows"></div>' +
    '<button class="btn lnk-add-coll" id="m-lnk-addrow">＋ ' + esc(t("graph.linkAddColl")) + "</button></div>" +
    "</div>" +
    '<div id="m-lnk-manual" class="hidden">' +
    '<div class="field"><label>' + esc(t("graph.linkFrom")) + "</label>" + linkPairRow("m-lnk-f") + "</div>" +
    '<div class="field"><label>' + esc(t("graph.linkTo")) + "</label>" + linkPairRow("m-lnk-t") + "</div>" +
    "</div>" +
    '<div class="field"><label>' + esc(t("graph.linkLabel")) + '</label><input id="m-lnk-label"></div>' +
    '<div class="field"><label>' + esc(t("graph.linkColor")) + '</label><input type="color" id="m-lnk-color" value="' + randColor() + '"></div>' +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + "</button>" +
    '<button class="btn primary" id="m-lnk-add">' + esc(t("graph.linkAdd")) + "</button></div>"
  );
  renderLinksList();
  // Filas dinámicas "colección + campo" (mínimo 1): agrupar en una sola
  // colección o cruzar varias, cada una con su propio nombre de campo.
  const rowsEl = document.getElementById("m-lnk-rows");
  const addRow = (collSel) => {
    const row = document.createElement("div");
    row.className = "lnk-row";
    const opts = G.vertexColls.map((c) =>
      '<option value="' + esc(c) + '"' + (c === collSel ? " selected" : "") + ">" + esc(c) + "</option>").join("");
    row.innerHTML = '<select class="lnk-row-coll">' + opts + "</select>" +
      '<input class="lnk-row-field" placeholder="' + esc(t("graph.linkField")) + '">' +
      '<button class="icon-btn danger" title="' + esc(t("graph.linkDelColl")) + '">✕</button>';
    row.querySelector("button").addEventListener("click", () => {
      if (rowsEl.children.length > 1) row.remove();
    });
    rowsEl.appendChild(row);
  };
  addRow();
  document.getElementById("m-lnk-addrow").addEventListener("click", () => addRow());
  const modeSel = document.getElementById("m-lnk-mode");
  modeSel.addEventListener("change", () => {
    const pivot = modeSel.value === "pivot";
    document.getElementById("m-lnk-pivot").classList.toggle("hidden", !pivot);
    document.getElementById("m-lnk-manual").classList.toggle("hidden", pivot);
  });
  document.getElementById("m-cancel").addEventListener("click", closeModal);
  document.getElementById("m-lnk-add").addEventListener("click", async () => {
    const err = document.getElementById("m-err");
    err.textContent = "";
    const labelRaw = document.getElementById("m-lnk-label").value.trim();
    const color = document.getElementById("m-lnk-color").value;
    try {
      let link;
      if (modeSel.value === "pivot") {
        const pairs = Array.from(rowsEl.querySelectorAll(".lnk-row")).map((r) => ({
          coll: r.querySelector(".lnk-row-coll").value,
          field: r.querySelector(".lnk-row-field").value.trim(),
        })).filter((p) => p.coll);
        if (!pairs.length) { err.textContent = t("graph.linkNeedColls"); return; }
        if (pairs.some((p) => !p.field)) { err.textContent = t("graph.linkFieldReq"); return; }
        const v = await validateLinkFields(pairs.map((p) => [p.coll, p.field]));
        if (v.err) { err.textContent = v.err; return; }
        const defLabel = Array.from(new Set(pairs.map((p) => p.field))).join("=");
        link = { id: newLinkId(), mode: "pivot", enabled: true, label: labelRaw || defLabel, pairs, type: v.type, color };
      } else {
        const from = {
          coll: document.getElementById("m-lnk-f-coll").value,
          field: document.getElementById("m-lnk-f-field").value.trim(),
        };
        const to = {
          coll: document.getElementById("m-lnk-t-coll").value,
          field: document.getElementById("m-lnk-t-field").value.trim(),
        };
        if (!from.field || !to.field) { err.textContent = t("graph.linkFieldReq"); return; }
        const v = await validateLinkFields([[from.coll, from.field], [to.coll, to.field]]);
        if (v.err) { err.textContent = v.err; return; }
        link = { id: newLinkId(), mode: "manual", enabled: true, label: labelRaw || (from.field + "=" + to.field), from, to, type: v.type, color };
      }
      G.links.push(link);
      saveLinks();
      renderLinksList();
      renderLegend();
      toast(t("graph.linkAdded"));
      // Agrupación automática: crea los nodos-valor y conecta los documentos
      // existentes sin necesidad de buscar un valor a mano.
      autoGroup(link).catch((e) => toast(e.message, true));
    } catch (e) { err.textContent = e.message; }
  });
}

// seedLink siembra el grafo con todos los documentos cuyo campo coincide con un
// valor dado (p. ej. un customerId), sin necesidad de conocer ningún _id.
function seedLink(link) {
  openModal(
    "<h2>🔍 " + esc(t("graph.linkSeed")) + " · " + esc(link.label) + "</h2>" +
    '<p class="lnk-hint">' + esc(linkSpec(link)) + "</p>" +
    '<div class="field"><label>' + esc(t("graph.linkSeedPh")) + '</label><input id="m-lnk-val"></div>' +
    '<div class="modal-error" id="m-err"></div>' +
    '<div class="actions"><button class="btn" id="m-cancel">' + esc(t("common.cancel")) + "</button>" +
    '<button class="btn primary" id="m-seed-ok">' + esc(t("graph.linkSeed")) + "</button></div>"
  );
  document.getElementById("m-cancel").addEventListener("click", closeModal);
  document.getElementById("m-seed-ok").addEventListener("click", async () => {
    const err = document.getElementById("m-err");
    err.textContent = "";
    const raw = document.getElementById("m-lnk-val").value.trim();
    let value = raw;
    if (link.type === "number") {
      const num = Number(raw);
      if (raw === "" || !isFinite(num)) { err.textContent = t("graph.linkBadNum"); return; }
      value = num;
    } else if (raw === "") { err.textContent = t("graph.linkSeedVal"); return; }
    closeModal();
    // Agrupa todos los documentos coincidentes bajo el nodo-valor (cuadrado)
    // de la relación; pivot y manual comparten el mismo valor de unión.
    const found = [];
    const seen = new Set();
    const grab = async (q) => {
      const docs = await linkFind(q.coll, q.field, value);
      for (const m of docs) {
        const mid = m["_id"];
        if (typeof mid !== "string") continue;
        const key = keyOf(q.coll, mid);
        if (seen.has(key)) continue;
        seen.add(key);
        found.push({ coll: q.coll, id: mid, doc: m });
      }
    };
    try {
      if (link.mode === "pivot") {
        for (const p of linkPairs(link)) await grab(p);
      } else {
        await grab(link.from);
        await grab(link.to);
      }
    } catch (e) { toast(e.message, true); return; }
    if (!found.length) { toast(t("graph.linkSeedEmpty"), true); return; }
    const hub = ensureHub(link, value);
    if (!hub) { toast(t("graph.limitReached"), true); return; }
    let added = 0;
    for (const f of found) {
      if (addNode(f.coll, f.id, f.doc)) added++;
      addValEdge(link, f.coll, f.id, hub);
    }
    if (!added) { toast(t("graph.limitReached"), true); return; }
    renderLegend();
    updateCount();
  });
}

/* ---------- physics + render loop ---------- */

function physics() {
  if (G.alpha < 0.003 && !G.drag) return;
  G.alpha *= 0.985;
  const nodes = Array.from(G.nodes.values());
  const n = nodes.length;
  if (!n) return;
  const rep = 3200 * G.alpha;
  const spring = 0.02;
  const rest = 150;
  for (let i = 0; i < n; i++) {
    const a = nodes[i];
    for (let j = i + 1; j < n; j++) {
      const b = nodes[j];
      let dx = a.x - b.x, dy = a.y - b.y;
      let d2 = dx * dx + dy * dy;
      if (d2 < 1) { dx = Math.random() - 0.5; dy = Math.random() - 0.5; d2 = 1; }
      if (d2 > 340 * 340) continue;
      const d = Math.sqrt(d2);
      const f = rep / d2;
      const fx = (dx / d) * f, fy = (dy / d) * f;
      a.vx += fx; a.vy += fy;
      b.vx -= fx; b.vy -= fy;
    }
  }
  for (const e of G.edges.values()) {
    const a = nodeById(e.from), b = nodeById(e.to);
    if (!a || !b || a === b) continue;
    const dx = b.x - a.x, dy = b.y - a.y;
    const d = Math.hypot(dx, dy) || 1;
    const f = (d - rest) * spring;
    const fx = (dx / d) * f, fy = (dy / d) * f;
    a.vx += fx; a.vy += fy;
    b.vx -= fx; b.vy -= fy;
  }
  const cx = G.w / 2, cy = G.h / 2;
  for (const nd of nodes) {
    nd.vx += (cx - nd.x) * 0.0015 * G.alpha * 10;
    nd.vy += (cy - nd.y) * 0.0015 * G.alpha * 10;
    if (G.groupMode) {
      let h = 0;
      for (let i = 0; i < nd.coll.length; i++) h = (h * 31 + nd.coll.charCodeAt(i)) % 360;
      const ang = (h / 360) * Math.PI * 2;
      const gx = cx + Math.cos(ang) * 220, gy = cy + Math.sin(ang) * 160;
      nd.vx += (gx - nd.x) * 0.01;
      nd.vy += (gy - nd.y) * 0.01;
    }
    if (!nd.fixed) {
      nd.vx *= 0.82; nd.vy *= 0.82;
      const v = Math.hypot(nd.vx, nd.vy);
      if (v > 14) { nd.vx = nd.vx / v * 14; nd.vy = nd.vy / v * 14; }
      nd.x += nd.vx; nd.y += nd.vy;
    }
  }
}

function nodeById(id) {
  return G.byId.get(id) || null;
}

function loop() {
  if (!G || !G.canvas || !G.canvas.isConnected) {
    if (G) {
      window.removeEventListener("resize", resize);
      G.raf = 0;
    }
    return;
  }
  physics();
  draw();
  G.raf = requestAnimationFrame(loop);
}

function draw() {
  const ctx = G.ctx;
  ctx.clearRect(0, 0, G.w, G.h);
  const dimmed = G.highlightNodes.size > 0 || G.highlightEdges.size > 0;
  ctx.lineWidth = 2 * G.zoom;
  for (const e of G.edges.values()) {
    const a = nodeById(e.from), b = nodeById(e.to);
    if (!a || !b) continue;
    const sa = worldToScreen(a.x, a.y), sb = worldToScreen(b.x, b.y);
    const hl = G.highlightEdges.has(e.id);
    // Las aristas virtuales usan el color de la relación (lcolor).
    const ecol = e.lcolor || typeColor(e.type);
    ctx.strokeStyle = ecol;
    ctx.globalAlpha = dimmed && !hl ? 0.15 : 0.8;
    ctx.lineWidth = (hl ? 4 : 2) * G.zoom;
    ctx.setLineDash(e.virtual ? [7, 5] : []);
    ctx.beginPath();
    ctx.moveTo(sa.x, sa.y);
    ctx.lineTo(sb.x, sb.y);
    ctx.stroke();
    ctx.setLineDash([]);
    const ang = Math.atan2(sb.y - sa.y, sb.x - sa.x);
    const r = 26 * G.zoom;
    const ex = sb.x - Math.cos(ang) * r, ey = sb.y - Math.sin(ang) * r;
    const sz = 7 * G.zoom;
    ctx.beginPath();
    ctx.moveTo(ex, ey);
    ctx.lineTo(ex - Math.cos(ang - 0.45) * sz, ey - Math.sin(ang - 0.45) * sz);
    ctx.lineTo(ex - Math.cos(ang + 0.45) * sz, ey - Math.sin(ang + 0.45) * sz);
    ctx.closePath();
    ctx.fillStyle = ecol;
    ctx.fill();
  }
  ctx.globalAlpha = 1;
  for (const n of G.nodes.values()) {
    const s = worldToScreen(n.x, n.y);
    const r = 26 * G.zoom;
    const hl = G.highlightNodes.has(n.key);
    ctx.globalAlpha = dimmed && !hl ? 0.25 : 1;
    ctx.beginPath();
    if (n.hub) {
      // Nodo-valor: cuadrado con el color de la relación (▣ Toyota). Los
      // registros siguen siendo círculos con el color de su colección.
      ctx.rect(s.x - r, s.y - r, r * 2, r * 2);
      ctx.fillStyle = n.vcolor || typeColor(n.val || n.id);
    } else {
      ctx.arc(s.x, s.y, r, 0, Math.PI * 2);
      ctx.fillStyle = n.ghost ? "rgba(139,155,184,0.35)" : collColor(n.coll);
    }
    ctx.fill();
    if (n === G.selected) {
      ctx.strokeStyle = "#ffffff";
      ctx.lineWidth = 3 * G.zoom;
      ctx.stroke();
    } else if (hl) {
      ctx.strokeStyle = "#7ee0a3";
      ctx.lineWidth = 3 * G.zoom;
      ctx.stroke();
    }
    if (G.zoom > 0.55) {
      ctx.fillStyle = "#0f1420";
      ctx.font = "bold " + Math.round(11 * Math.min(G.zoom, 1.4)) + "px system-ui";
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      const rawLabel = n.hub ? n.val : n.id;
      const label = rawLabel.length > 14 ? rawLabel.slice(0, 13) + "…" : rawLabel;
      ctx.fillText(label, s.x, s.y + r + 11 * G.zoom);
    }
  }
  ctx.globalAlpha = 1;
  if (G.connectFrom && G.drag === null) {
    const s = worldToScreen(G.connectFrom.x, G.connectFrom.y);
    ctx.strokeStyle = "#4f8cff";
    ctx.setLineDash([6, 5]);
    ctx.lineWidth = 2;
    ctx.beginPath();
    ctx.moveTo(s.x, s.y);
    ctx.lineTo(G.mouse.x, G.mouse.y);
    ctx.stroke();
    ctx.setLineDash([]);
  }
}

})();
