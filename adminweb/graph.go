package adminweb

import (
	"net/http"
	"strconv"
	"strings"

	"mlstoredb/db"
)

func intParam(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (s *Server) handleGraphMeta(w http.ResponseWriter, r *http.Request, ws *webSession) {
	colls := ws.sess.Collections()
	edgeTypes := []string{}
	vertexColls := []string{}
	for _, c := range colls {
		if strings.HasPrefix(c, "edges.") {
			edgeTypes = append(edgeTypes, strings.TrimPrefix(c, "edges."))
		} else {
			vertexColls = append(vertexColls, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"edgeTypes":   edgeTypes,
		"vertexColls": vertexColls,
	})
}

func (s *Server) handleGraphNeighbors(w http.ResponseWriter, r *http.Request, ws *webSession) {
	q := r.URL.Query()
	edge := q.Get("edge")
	vertex := q.Get("vertex")
	if edge == "" || vertex == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "edge and vertex required"})
		return
	}
	coll := "edges." + edge
	out, err := ws.sess.Find(coll, db.Document{"_from": vertex}, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	in, err := ws.sess.Find(coll, db.Document{"_to": vertex}, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	seen := map[string]bool{}
	type gEdge struct {
		ID    string      `json:"id"`
		Type  string      `json:"type"`
		From  string      `json:"from"`
		To    string      `json:"to"`
		Dir   string      `json:"dir"`
		Props db.Document `json:"props,omitempty"`
	}
	edges := make([]gEdge, 0)
	neighbors := make([]string, 0)
	add := func(list []db.Document, dir string) {
		for _, d := range list {
			id, _ := d["_id"].(string)
			if seen[id] {
				continue
			}
			seen[id] = true
			from, _ := d["_from"].(string)
			to, _ := d["_to"].(string)
			props := db.Document{}
			for k, v := range d {
				if k != "_id" && k != "_from" && k != "_to" {
					props[k] = v
				}
			}
			edges = append(edges, gEdge{ID: id, Type: edge, From: from, To: to, Dir: dir, Props: props})
			other := to
			if dir == "in" {
				other = from
			}
			if other != vertex {
				neighbors = append(neighbors, other)
			}
		}
	}
	add(out, "out")
	add(in, "in")
	writeJSON(w, http.StatusOK, map[string]any{
		"edges":     edges,
		"neighbors": neighbors,
	})
}

func (s *Server) handleGraphResolve(w http.ResponseWriter, r *http.Request, ws *webSession) {
	var body struct {
		IDs   []string `json:"ids"`
		Colls []string `json:"colls"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ids required"})
		return
	}
	colls := body.Colls
	if len(colls) == 0 {
		colls = ws.sess.Collections()
	}
	ids := make([]any, len(body.IDs))
	for i, id := range body.IDs {
		ids[i] = id
	}
	type resolved struct {
		ID   string      `json:"id"`
		Coll string      `json:"coll"`
		Doc  db.Document `json:"doc"`
	}
	found := make([]resolved, 0)
	seen := map[string]bool{}
	for _, c := range colls {
		if strings.HasPrefix(c, "edges.") {
			continue
		}
		docs, err := ws.sess.Find(c, db.Document{"_id": db.Document{"$in": ids}}, nil)
		if err != nil {
			continue
		}
		for _, d := range docs {
			id, _ := d["_id"].(string)
			if seen[id] {
				continue
			}
			seen[id] = true
			found = append(found, resolved{ID: id, Coll: c, Doc: d})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": found})
}

func (s *Server) handleGraphEdgeCreate(w http.ResponseWriter, r *http.Request, ws *webSession) {
	var body struct {
		Type  string      `json:"type"`
		From  string      `json:"from"`
		To    string      `json:"to"`
		Props db.Document `json:"props"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Type == "" || body.From == "" || body.To == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type, from and to required"})
		return
	}
	id, err := ws.sess.AddEdge(body.Type, body.From, body.To, body.Props)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleGraphEdgeDelete(w http.ResponseWriter, r *http.Request, ws *webSession) {
	q := r.URL.Query()
	edge := q.Get("type")
	id := q.Get("id")
	if edge == "" || id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type and id required"})
		return
	}
	if err := ws.sess.RemoveEdge(edge, id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleGraphTraverse(w http.ResponseWriter, r *http.Request, ws *webSession) {
	q := r.URL.Query()
	edge := q.Get("edge")
	start := q.Get("start")
	if edge == "" || start == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "edge and start required"})
		return
	}
	dir := db.Outgoing
	switch q.Get("dir") {
	case "in":
		dir = db.Incoming
	case "both":
		dir = db.Both
	}
	nodes, err := ws.sess.Traverse(db.TraverseOptions{
		EdgeType:  edge,
		Start:     start,
		Direction: dir,
		MaxDepth:  intParam(q.Get("maxDepth")),
		MinDepth:  intParam(q.Get("minDepth")),
		Limit:     intParam(q.Get("limit")),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if nodes == nil {
		nodes = []db.TraverseNode{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (s *Server) handleGraphPath(w http.ResponseWriter, r *http.Request, ws *webSession) {
	q := r.URL.Query()
	edge := q.Get("edge")
	from := q.Get("from")
	to := q.Get("to")
	if edge == "" || from == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "edge, from and to required"})
		return
	}
	path, err := ws.sess.ShortestPath(edge, from, to, intParam(q.Get("maxDepth")))
	if err != nil {
		writeError(w, err)
		return
	}
	if path == nil {
		path = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path})
}
