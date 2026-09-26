package adminweb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mlstoredb/db"
)

type multiDB struct {
	mu      sync.Mutex
	servers map[string]*Server
}

func newMultiDB() *multiDB {
	return &multiDB{servers: make(map[string]*Server)}
}

func (m *multiDB) set(name string, srv *Server) {
	m.mu.Lock()
	m.servers[name] = srv
	m.mu.Unlock()
}

func (m *multiDB) get(name string) (*Server, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[name]
	return s, ok
}

func (m *multiDB) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.servers))
	for n := range m.servers {
		out = append(out, n)
	}
	return out
}

type MultiServer struct {
	opts   ServerOptions
	dbDir  string
	multi  *multiDB
	defSrv *Server
	_empty *db.Store
}

func NewMultiServer(defaultStore *db.Store, dbDir string, o ServerOptions) *MultiServer {
	m := &MultiServer{opts: o, dbDir: dbDir, multi: newMultiDB()}
	if defaultStore != nil {
		// El store por defecto es el que sirve el protocolo Mongo (-path/-db). Se registra
		// con el nombre real de la base para que la consola muestre y edite ESA misma base
		// (y sus credenciales) en lugar de un genérico "default"; sin nombre, "default".
		name := o.DBName
		if name == "" {
			name = "default"
		}
		srv := *m.defServer(defaultStore)
		m.multi.set(name, &srv)
	}
	return m
}

func (m *MultiServer) defServer(store *db.Store) *Server {
	return &Server{store: store, opts: m.opts, auth: newAuthRegistry(), jobs: make(map[importJobID]*importJob)}
}

func (m *MultiServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/databases", m.handleListDatabases)
	mux.HandleFunc("POST /api/databases", m.handleCreateDatabase)
	mux.HandleFunc("/", m.handleAll)
	return m.securityHeaders(mux)
}

func (m *MultiServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func (m *MultiServer) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	if m.dbDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no dbDir configured"})
		return
	}
	var body struct {
		Name     string `json:"name"`
		User     string `json:"user,omitempty"`
		Password string `json:"password,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || strings.ContainsAny(name, "/\\:$") || len(name) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid database name"})
		return
	}
	m.multi.mu.Lock()
	if _, exists := m.multi.servers[name]; exists {
		m.multi.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "database already exists"})
		return
	}
	path := filepath.Join(m.dbDir, name, name+".mlstore")
	if _, err := os.Stat(path); err == nil {
		m.multi.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "database file already exists"})
		return
	}
	m.multi.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "cannot create directory: " + err.Error()})
		return
	}
	store, err := db.OpenWithLock(path, db.Options{MasterKey: m.opts.MasterKey})
	if err != nil {
		writeError(w, err)
		return
	}

	// Crear admin en la misma BD si se proporcionaron credenciales
	if body.User != "" || body.Password != "" {
		if len(body.User) < 3 || len(body.Password) < 6 {
			store.Close()
			os.Remove(path)
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user min 3, pass min 6"})
			return
		}
		if err := store.CreateRole("admin", []db.Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
			store.Close()
			os.Remove(path)
			writeError(w, err)
			return
		}
		if err := store.CreateUser(body.User, body.Password, []string{"admin"}); err != nil {
			store.Close()
			os.Remove(path)
			writeError(w, err)
			return
		}
		if err := store.FlushSync(); err != nil {
			store.Close()
			os.Remove(path)
			writeError(w, err)
			return
		}
	}

	m.multi.set(name, m.defServer(store))
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name})
}

func (m *MultiServer) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	dbs := make([]map[string]any, 0)
	seen := map[string]bool{}
	for name, srv := range m.mapServers() {
		seen[name] = true
		dbs = append(dbs, map[string]any{
			"name": name, "path": srv.store.Path(), "authActive": srv.store.AuthActive(),
		})
	}
	if m.dbDir != "" {
		entries, err := os.ReadDir(m.dbDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				if seen[name] {
					continue
				}
				candidate := filepath.Join(m.dbDir, name, name+".mlstore")
				if _, err := os.Stat(candidate); err == nil {
					dbs = append(dbs, map[string]any{"name": name, "path": candidate, "authActive": false})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": dbs})
}

func (m *MultiServer) mapServers() map[string]*Server {
	m.multi.mu.Lock()
	defer m.multi.mu.Unlock()
	out := make(map[string]*Server, len(m.multi.servers))
	for k, v := range m.multi.servers {
		out[k] = v
	}
	return out
}

func (m *MultiServer) handleAll(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/api/") {
		staticHandler(m.webServer()).ServeHTTP(w, r)
		return
	}
	rest := strings.TrimPrefix(path, "/api/db/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify database: /api/db/{name}/..."})
		return
	}
	dbname := rest[:slash]
	// El Server por base de datos registra sus rutas bajo /api/... — se mantiene el
	// prefijo para que la petición reenviada llegue al handler real (corrige 405/HTML
	// en las llamadas de API reenviadas).
	subpath := "/api" + rest[slash:]

	srv, err := m.resolve(dbname)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	forwardRequest(srv, w, r, subpath)
}

func (m *MultiServer) resolve(name string) (*Server, error) {
	if s, ok := m.multi.get(name); ok {
		return s, nil
	}
	if m.dbDir == "" {
		return nil, errors.New("no db dir configured")
	}
	m.multi.mu.Lock()
	defer m.multi.mu.Unlock()
	if s, ok := m.multi.servers[name]; ok {
		return s, nil
	}
	path := filepath.Join(m.dbDir, name, name+".mlstore")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database file not exist: %s", path)
	}
	if len(m.opts.MasterKey) == 0 {
		return nil, errors.New("database is encrypted — start with -key flag")
	}
	store, err := db.OpenWithLock(path, db.Options{MasterKey: m.opts.MasterKey})
	if err != nil {
		return nil, fmt.Errorf("cannot open database: %w", err)
	}
	srv := m.defServer(store)
	m.multi.servers[name] = srv
	return srv, nil
}

func (m *MultiServer) webServer() *Server {
	if m.defSrv != nil {
		return m.defSrv
	}
	return &Server{store: m.emptyStore(), opts: m.opts}
}

func (m *MultiServer) emptyStore() *db.Store {
	if m._empty == nil {
		m._empty = db.New()
	}
	return m._empty
}

func staticHandler(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.serveIndex)
	mux.HandleFunc("GET /app.css", srv.serveAssetFunc("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /app.js", srv.serveAssetFunc("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /graph.js", srv.serveAssetFunc("graph.js", "text/javascript; charset=utf-8"))
	return mux
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeAsset(w, r, b, "text/html; charset=utf-8")
}

func (s *Server) serveAssetFunc(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeAsset(w, r, b, ctype)
	}
}

// forwardMaxBodyBytes acota el cuerpo que se almacena en búfer para las peticiones
// reenviadas. El cuerpo se almacena entero porque el servidor por base de datos puede
// consumirlo de forma asíncrona (p. ej. el worker de importación) después de que el
// handler externo retorne; un io.Pipe vivo se cortaría cuando net/http cierra el cuerpo.
const forwardMaxBodyBytes = 512 << 20

func forwardRequest(srv *Server, w http.ResponseWriter, r *http.Request, subpath string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, forwardMaxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "forward: " + err.Error()})
		return
	}
	_ = r.Body.Close()
	// Conservar la query string: los handlers por base de datos dependen de ella
	// (formato de exportación, skip/limit/filter/sort de exploración, arista/vértice
	// del grafo, campos de índice, importación).
	full := subpath
	if r.URL.RawQuery != "" {
		full = subpath + "?" + r.URL.RawQuery
	}
	r2, err := http.NewRequest(r.Method, full, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "forward: " + err.Error()})
		return
	}
	r2.Header = r.Header.Clone()
	srv.Handler().ServeHTTP(w, r2)
}

func (m *MultiServer) Close() error {
	m.multi.mu.Lock()
	defer m.multi.mu.Unlock()
	var lastErr error
	for _, srv := range m.multi.servers {
		if err := srv.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (m *MultiServer) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 10}
	return httpSrv.Serve(ln)
}
