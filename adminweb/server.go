package adminweb

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mlstoredb/db"
)

//go:embed static
var staticFS embed.FS

type ServerOptions struct {
	DBName    string
	Brand     string
	MasterKey []byte
	// Logger recibe los diagnósticos de la consola (logins fallidos con su motivo real,
	// bloqueos por intentos repetidos, errores de arranque del servidor). nil = log
	// estándar (stderr con fecha y hora).
	Logger *log.Logger
}

type importJobID string

type importJob struct {
	ID        importJobID `json:"id"`
	Format    string      `json:"format"`
	Mode      string      `json:"mode"`
	Status    string      `json:"status"` // running|done|error
	Total     int         `json:"total"`
	Processed int         `json:"processed"`
	Inserted  int         `json:"inserted"`
	Errors    int         `json:"errors"`
	ErrMsg    string      `json:"error,omitempty"`
	mu        sync.Mutex  `json:"-"`
}

func (j *importJob) snapshot() *importJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	return &importJob{
		ID:        j.ID,
		Format:    j.Format,
		Mode:      j.Mode,
		Status:    j.Status,
		Total:     j.Total,
		Processed: j.Processed,
		Inserted:  j.Inserted,
		Errors:    j.Errors,
		ErrMsg:    j.ErrMsg,
	}
}

type Server struct {
	store   *db.Store
	opts    ServerOptions
	auth    *authRegistry
	mu      sync.Mutex
	ln      net.Listener
	httpSrv *http.Server
	closed  bool
	jobsMu  sync.Mutex
	jobs    map[importJobID]*importJob
	jobSeq  uint64
}

func NewServer(store *db.Store, o ServerOptions) *Server {
	if o.Brand == "" {
		o.Brand = "MLS Admin"
	}
	s := &Server{
		store: store,
		opts:  o,
		auth:  newAuthRegistry(),
		jobs:  make(map[importJobID]*importJob),
	}
	return s
}

// logf escribe un diagnóstico en el logger configurado (log estándar si no se inyectó uno).
func (s *Server) logf(format string, args ...any) {
	lg := s.opts.Logger
	if lg == nil {
		lg = log.Default()
	}
	lg.Printf(format, args...)
}

// dbLabel identifica la base de datos de este servidor en los diagnósticos.
func (s *Server) dbLabel() string {
	if s.opts.DBName != "" {
		return s.opts.DBName
	}
	if p := s.store.Path(); p != "" {
		return p
	}
	return "<memoria>"
}

func (s *Server) importJobCreate(format, mode string) *importJob {
	s.jobsMu.Lock()
	s.jobSeq++
	id := importJobID(fmt.Sprintf("imp-%d-%d", time.Now().UnixMilli(), s.jobSeq))
	job := &importJob{ID: id, Format: format, Mode: mode, Status: "running"}
	s.jobs[id] = job
	s.jobsMu.Unlock()
	return job
}

func (s *Server) importJobGet(id importJobID) *importJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	return s.jobs[id]
}

func (s *Server) importJobCleanup(maxAge time.Duration) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	now := time.Now()
	for id, j := range s.jobs {
		j.mu.Lock()
		done := j.Status != "running"
		j.mu.Unlock()
		if done {
			if p, err := time.ParseDuration("0s"); err == nil {
				_ = p
			}
			_ = now
			delete(s.jobs, id)
		}
	}
}

func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()
	return srv.Serve(ln)
}

// Handler devuelve el handler HTTP totalmente montado (rutas + middleware);
// útil para tests y para incrustarlo bajo un mux propio.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.securityHeaders(s.panicRecover(mux))
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
	return nil
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.authRequired(s.handleMe))

	mux.HandleFunc("GET /api/stats", s.authRequired(s.handleStats))
	mux.HandleFunc("GET /api/collections", s.authRequired(s.handleListCollections))
	mux.HandleFunc("POST /api/collections", s.authRequired(s.handleCreateCollection))
	mux.HandleFunc("DELETE /api/collections/{name}", s.authRequired(s.handleDropCollection))
	mux.HandleFunc("GET /api/collections/{name}/docs", s.authRequired(s.handleBrowseDocs))
	mux.HandleFunc("GET /api/collections/{name}/fields", s.authRequired(s.handleBrowseFields))
	mux.HandleFunc("GET /api/collections/{name}/doc", s.authRequired(s.handleGetDoc))
	mux.HandleFunc("POST /api/collections/{name}/docs", s.authRequired(s.handleInsertDoc))
	mux.HandleFunc("PUT /api/collections/{name}/doc", s.authRequired(s.handleReplaceDoc))
	mux.HandleFunc("DELETE /api/collections/{name}/doc", s.authRequired(s.handleDeleteDoc))

	mux.HandleFunc("GET /api/collections/{name}/indexes", s.authRequired(s.handleListIndexes))
	mux.HandleFunc("POST /api/collections/{name}/indexes", s.authRequired(s.handleCreateIndex))
	mux.HandleFunc("DELETE /api/collections/{name}/indexes", s.authRequired(s.handleDropIndex))
	mux.HandleFunc("GET /api/collections/{name}/export", s.authRequired(s.handleExport))
	mux.HandleFunc("POST /api/collections/{name}/import", s.authRequired(s.handleImport))
	mux.HandleFunc("GET /api/import/{id}", s.authRequired(s.handleImportStatus))

	mux.HandleFunc("GET /api/triggers", s.authRequired(s.handleListTriggers))
	mux.HandleFunc("PUT /api/triggers", s.authRequired(s.handleUpsertTrigger))
	mux.HandleFunc("DELETE /api/triggers/{id}", s.authRequired(s.handleDeleteTrigger))

	mux.HandleFunc("GET /api/users", s.authRequired(s.handleListUsers))
	mux.HandleFunc("POST /api/users", s.authRequired(s.handleCreateUser))
	mux.HandleFunc("PUT /api/users", s.authRequired(s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/users/{username}", s.authRequired(s.handleDeleteUser))
	mux.HandleFunc("GET /api/roles", s.authRequired(s.handleListRoles))
	mux.HandleFunc("POST /api/roles", s.authRequired(s.handleCreateRole))
	mux.HandleFunc("PUT /api/roles", s.authRequired(s.handleUpdateRole))
	mux.HandleFunc("DELETE /api/roles/{name}", s.authRequired(s.handleDeleteRole))
	mux.HandleFunc("POST /api/query", s.authRequired(s.handleQuery))

	mux.HandleFunc("GET /api/graph", s.authRequired(s.handleGraphMeta))
	mux.HandleFunc("GET /api/graph/neighbors", s.authRequired(s.handleGraphNeighbors))
	mux.HandleFunc("POST /api/graph/resolve", s.authRequired(s.handleGraphResolve))
	mux.HandleFunc("POST /api/graph/edge", s.authRequired(s.handleGraphEdgeCreate))
	mux.HandleFunc("DELETE /api/graph/edge", s.authRequired(s.handleGraphEdgeDelete))
	mux.HandleFunc("GET /api/graph/traverse", s.authRequired(s.handleGraphTraverse))
	mux.HandleFunc("GET /api/graph/path", s.authRequired(s.handleGraphPath))

	mux.HandleFunc("GET /app.css", s.serveAssetFunc("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.serveAssetFunc("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /graph.js", s.serveAssetFunc("graph.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /", s.serveIndex)
}

// assetETag deriva un ETag estable del contenido embebido: al recompilar el binario
// cambia y el navegador revalida en lugar de quedarse con un app.js viejo.
func assetETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// writeAsset sirve un asset embebido con Cache-Control no-cache + ETag (304 baratos).
func writeAsset(w http.ResponseWriter, r *http.Request, b []byte, ctype string) {
	et := assetETag(b)
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", et)
	if r.Header.Get("If-None-Match") == et {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

func (s *Server) authRequired(next func(http.ResponseWriter, *http.Request, *webSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws := s.sessionFromRequest(r)
		if !s.store.AuthActive() {
			if ws == nil {
				ws = &webSession{token: "permissive", sess: s.store.SystemSession()}
			}
			next(w, r, ws)
			return
		}
		if ws == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not logged in"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-CSRF-Token") != ws.csrf {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "bad CSRF token"})
				return
			}
		}
		next(w, r, ws)
	}
}

// permAny envuelve un conjunto mínimo de permisos tipo Session para webSession en modo permisivo.

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) panicRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("internal error: %v", rec)})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, 100<<20)
}

// readJSONLimit lee y decodifica un cuerpo JSON de hasta maxBytes.
func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": "payload too large (limit " + humanBytes(maxBytes) + ")",
		})
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case strings.Contains(err.Error(), "is in use"):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, db.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	case errors.Is(err, db.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
	case errors.Is(err, db.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	case errors.Is(err, db.ErrExists):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already exists"})
	case errors.Is(err, db.ErrDuplicate):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "duplicate _id or unique index"})
	case errors.Is(err, db.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "document too large"})
	case errors.Is(err, db.ErrBadFilter), errors.Is(err, db.ErrNoID):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

func fileHumanSize(path string) string {
	if path == "" {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return humanBytes(st.Size())
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func sanitizeCollName(name string) error {
	if name == "" || strings.HasPrefix(name, "$") || strings.ContainsRune(name, 0) ||
		strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') || len(name) > 255 {
		return errors.New("invalid collection name")
	}
	return nil
}
