package adminweb

import (
	"net/http"

	"mlstoredb/db"
)

func adminOnly(ws *webSession, w http.ResponseWriter) bool {
	if ws.sess.CanWrite("*") {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin role required"})
	return false
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": s.store.ListUsers()})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	var body struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Username) < 3 || len(body.Password) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "username min 3, password min 6"})
		return
	}
	if err := s.store.CreateUser(body.Username, body.Password, body.Roles); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	var body struct {
		Username string   `json:"username"`
		Roles    []string `json:"roles"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	users := s.store.ListUsers()
	exists := false
	for _, u := range users {
		if u.Username == body.Username {
			exists = true
			break
		}
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "user not found"})
		return
	}
	// Reemplazar los roles in situ: conserva el hash de la contraseña (y cualquier
	// otro campo) en lugar del antiguo borrar+recrear que reiniciaba la contraseña.
	if err := s.store.SetUserRoles(body.Username, body.Roles); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	username := r.PathValue("username")
	if username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "username required"})
		return
	}
	if err := s.store.DeleteUser(username); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": s.store.ListRoles()})
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	var body struct {
		Name        string          `json:"name"`
		Permissions []db.Permission `json:"permissions"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Name) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name min 2"})
		return
	}
	if err := s.store.CreateRole(body.Name, body.Permissions); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	var body struct {
		Name        string          `json:"name"`
		Permissions []db.Permission `json:"permissions"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	existing := s.store.ListRoles()
	found := false
	for _, rl := range existing {
		if rl.Name == body.Name {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "role not found"})
		return
	}
	if err := s.store.DeleteRole(body.Name); err != nil && err != db.ErrNotFound {
		writeError(w, err)
		return
	}
	if err := s.store.CreateRole(body.Name, body.Permissions); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request, ws *webSession) {
	if !adminOnly(ws, w) {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	if err := s.store.DeleteRole(name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request, ws *webSession) {
	var body struct {
		Coll   string      `json:"coll"`
		Filter db.Document `json:"filter"`
		Limit  int         `json:"limit"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Limit <= 0 || body.Limit > 500 {
		body.Limit = 50
	}
	opts := &db.FindOptions{Limit: body.Limit}
	docs, err := ws.sess.Find(body.Coll, body.Filter, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	total, err := ws.sess.Count(body.Coll, body.Filter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"docs":  docs,
		"total": total,
	})
}
