package adminweb

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"mlstoredb/db"
)

// browseFieldsMaxDepth limita la profundidad de los paths anidados que reporta
// /fields (mismo espíritu que los filtros punteados del motor: from.name, items.sku).
const browseFieldsMaxDepth = 3

// browseFieldsMaxScan es el tope duro de documentos escaneados por /fields (evita
// bloquear la consola en colecciones enormes); el cliente puede pedir menos.
const browseFieldsMaxScan = 200000

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, ws *webSession) {
	colls := ws.sess.Collections()
	type collInfo struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	list := make([]collInfo, 0, len(colls))
	totalDocs := 0
	for _, c := range colls {
		n, err := ws.sess.Count(c, nil)
		if err != nil {
			continue
		}
		list = append(list, collInfo{Name: c, Count: n})
		totalDocs += n
	}
	cache := s.store.CacheStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"brand":       s.opts.Brand,
		"collections": list,
		"totalDocs":   totalDocs,
		"fileSize":    fileHumanSize(s.store.Path()),
		"authActive":  s.store.AuthActive(),
		"cache": map[string]any{
			"resident": cache.Resident,
			"cold":     cache.Cold,
			"bytes":    humanBytes(cache.Bytes),
			"budget":   humanBytes(cache.Budget),
		},
	})
}

func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request, ws *webSession) {
	colls := ws.sess.Collections()
	type collInfo struct {
		Name    string `json:"name"`
		Count   int    `json:"count"`
		Indexes int    `json:"indexes"`
	}
	list := make([]collInfo, 0, len(colls))
	for _, c := range colls {
		n, err := ws.sess.Count(c, nil)
		if err != nil {
			n = -1
		}
		idxs, _ := ws.sess.ListIndexes(c)
		list = append(list, collInfo{Name: c, Count: n, Indexes: len(idxs)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": list})
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request, ws *webSession) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := sanitizeCollName(body.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := ws.sess.CreateCollection(body.Name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": body.Name})
}

func (s *Server) handleDropCollection(w http.ResponseWriter, r *http.Request, ws *webSession) {
	name := r.PathValue("name")
	if err := ws.sess.DropCollection(name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func parseFilter(raw string) (db.Document, error) {
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var f db.Document
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, errors.New("invalid filter JSON: " + err.Error())
	}
	return f, nil
}

func (s *Server) handleBrowseDocs(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	q := r.URL.Query()
	skip, _ := strconv.Atoi(q.Get("skip"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	if skip < 0 {
		skip = 0
	}
	filter, err := parseFilter(q.Get("filter"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	opts := &db.FindOptions{Limit: limit, Skip: skip}
	if sort := q.Get("sort"); sort != "" {
		dir := 1
		if q.Get("dir") == "-1" {
			dir = -1
		}
		opts.Sort = map[string]int{sort: dir}
	}
	docs, err := ws.sess.Find(coll, filter, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	total, err := ws.sess.Count(coll, filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"docs":  docs,
		"total": total,
		"skip":  skip,
		"limit": limit,
	})
}

// jsonTypeOf clasifica un valor del motor para que la consola pueda construir
// filtros con el tipo correcto (número vs texto vs booleano).
func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return "number"
	case db.Document, map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "other"
	}
}

// noteFieldType guarda el primer tipo no nulo observado para un campo.
func noteFieldType(types map[string]string, key string, val any) {
	t := jsonTypeOf(val)
	if t == "null" {
		return
	}
	prev, seen := types[key]
	if !seen || prev == "null" || prev == "other" {
		types[key] = t
	}
}

// collectDocFields acumula los campos de primer nivel (top), los paths punteados
// (paths) y el tipo observado por path (types) de un documento o subdocumento.
// Los arrays propagan el mismo prefijo que el motor en los filtros (items.sku).
func collectDocFields(prefix string, v any, top, paths map[string]bool, types map[string]string, depth int) {
	switch d := v.(type) {
	case db.Document:
		collectDocMap(prefix, d, top, paths, types, depth)
	case map[string]any:
		collectDocMap(prefix, db.Document(d), top, paths, types, depth)
	case []any:
		for _, it := range d {
			collectDocFields(prefix, it, top, paths, types, depth)
		}
	}
}

func collectDocMap(prefix string, d db.Document, top, paths map[string]bool, types map[string]string, depth int) {
	for k, val := range d {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		if prefix == "" {
			top[k] = true
		} else {
			paths[full] = true
		}
		noteFieldType(types, full, val)
		switch val.(type) {
		case db.Document, map[string]any, []any:
			if depth+1 <= browseFieldsMaxDepth {
				collectDocFields(full, val, top, paths, types, depth+1)
			}
		}
	}
}

// handleBrowseFields descubre los campos de la colección para que la consola pueda
// mostrar columnas dinámicas y ofrecer filtros por campo (incluidos paths anidados).
//
//	GET /api/collections/{name}/fields?maxScan=2000&batch=500
//
// maxScan=0 ⇒ escanea todo (con tope duro browseFieldsMaxScan). Una colección con
// fieldDeny activo no expone los campos denegados (los redacta la sesión).
func (s *Server) handleBrowseFields(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	q := r.URL.Query()
	maxScan := 2000
	if raw := q.Get("maxScan"); raw != "" {
		maxScan = intParam(raw)
	}
	if maxScan <= 0 || maxScan > browseFieldsMaxScan {
		maxScan = browseFieldsMaxScan
	}
	batch := 500
	if b := intParam(q.Get("batch")); b > 0 && b <= 5000 {
		batch = b
	}

	top := map[string]bool{}
	paths := map[string]bool{}
	types := map[string]string{}
	scanned := 0
	for scanned < maxScan {
		n := batch
		if rem := maxScan - scanned; rem < n {
			n = rem
		}
		docs, err := ws.sess.Find(coll, nil, &db.FindOptions{Limit: n, Skip: scanned})
		if err != nil {
			writeError(w, err)
			return
		}
		for _, d := range docs {
			collectDocFields("", d, top, paths, types, 0)
		}
		scanned += len(docs)
		if len(docs) < n {
			break
		}
	}
	truncated := false
	if scanned >= maxScan {
		if total, err := ws.sess.Count(coll, nil); err == nil && total > scanned {
			truncated = true
		}
	}

	fields := make([]string, 0, len(top))
	for k := range top {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	if i := sort.SearchStrings(fields, "_id"); i < len(fields) && fields[i] == "_id" {
		ordered := make([]string, 0, len(fields)+1)
		ordered = append(ordered, "_id")
		ordered = append(ordered, fields[:i]...)
		ordered = append(ordered, fields[i+1:]...)
		fields = ordered
	}
	pathList := make([]string, 0, len(paths))
	for k := range paths {
		pathList = append(pathList, k)
	}
	sort.Strings(pathList)

	writeJSON(w, http.StatusOK, map[string]any{
		"fields":    fields,
		"paths":     pathList,
		"types":     types,
		"scanned":   scanned,
		"truncated": truncated,
	})
}

func (s *Server) handleGetDoc(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	doc, err := ws.sess.Get(coll, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"doc": doc})
}

func (s *Server) handleInsertDoc(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	var doc db.Document
	if !readJSON(w, r, &doc) {
		return
	}
	if v, ok := doc["_id"]; ok {
		if _, isStr := v.(string); !isStr {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "_id must be a string"})
			return
		}
	}
	if err := ws.sess.Insert(coll, doc); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": doc["_id"]})
}

func (s *Server) handleReplaceDoc(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	var doc db.Document
	if !readJSON(w, r, &doc) {
		return
	}
	if v, ok := doc["_id"]; ok {
		if sid, isStr := v.(string); !isStr || sid != id {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "_id mismatch"})
			return
		}
	} else {
		doc["_id"] = id
	}
	if err := ws.sess.Upsert(coll, id, doc); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteDoc(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	if err := ws.sess.Delete(coll, id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
