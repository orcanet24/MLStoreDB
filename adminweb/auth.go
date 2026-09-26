package adminweb

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"mlstoredb/db"
)

type webSession struct {
	token  string
	sess   *db.Session
	csrf   string
	remote string
}

type loginAttempt struct {
	count int
	reset time.Time
}

type authRegistry struct {
	mu       sync.Mutex
	sessions map[string]*webSession
	attempts map[string]*loginAttempt
}

func newAuthRegistry() *authRegistry {
	return &authRegistry{
		sessions: make(map[string]*webSession),
		attempts: make(map[string]*loginAttempt),
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString(make([]byte, n))
	}
	return hex.EncodeToString(b)
}

const (
	maxLoginAttempts  = 5
	loginWindow       = time.Minute
	sessionCookieName = "mls_admin"
)

func (ar *authRegistry) register(sess *db.Session, remote string) *webSession {
	ws := &webSession{
		token:  sess.Token(),
		sess:   sess,
		csrf:   randomToken(32),
		remote: remote,
	}
	ar.mu.Lock()
	ar.sessions[ws.token] = ws
	ar.mu.Unlock()
	return ws
}

func (ar *authRegistry) get(token string) *webSession {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return ar.sessions[token]
}

func (ar *authRegistry) drop(token string) {
	ar.mu.Lock()
	delete(ar.sessions, token)
	ar.mu.Unlock()
}

func (ar *authRegistry) count() int {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return len(ar.sessions)
}

func (ar *authRegistry) allowLogin(ip string) bool {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	a := ar.attempts[ip]
	if a == nil || time.Now().After(a.reset) {
		delete(ar.attempts, ip)
		return true
	}
	return a.count < maxLoginAttempts
}

func (ar *authRegistry) loginFailed(ip string) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	a := ar.attempts[ip]
	if a == nil || time.Now().After(a.reset) {
		a = &loginAttempt{reset: time.Now().Add(loginWindow)}
		ar.attempts[ip] = a
	}
	a.count++
}

func (ar *authRegistry) loginOK(ip string) {
	ar.mu.Lock()
	delete(ar.attempts, ip)
	ar.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func (s *Server) sessionFromRequest(r *http.Request) *webSession {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	return s.auth.get(c.Value)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authActive":   s.store.AuthActive(),
		"needsSetup":   !s.store.AuthActive(),
		"sessionCount": s.auth.count(),
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.store.AuthActive() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "already configured"})
		return
	}
	var body struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.User) < 3 || len(body.Pass) < 6 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user min 3, pass min 6"})
		return
	}
	if err := s.store.CreateRole("admin", []db.Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CreateUser(body.User, body.Pass, []string{"admin"}); err != nil {
		writeError(w, err)
		return
	}
	s.logf("console[%s]: first user %q created (RBAC active from now on)", s.dbLabel(), body.User)
	s.loginWith(w, r, body.User, body.Pass)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	s.loginWith(w, r, body.User, body.Pass)
}

// authFailReason describe por qué Store.Authenticate rechazó el login. La respuesta HTTP
// sigue siendo genérica (no se enumeran usuarios al cliente); este texto va al log del
// servidor, que antes descartaba el motivo real del fallo.
func authFailReason(store *db.Store, user string) string {
	for _, u := range store.ListUsers() {
		if u.Username == user {
			return "user exists: wrong password or disabled account"
		}
	}
	return "user does not exist in this database _users"
}

func (s *Server) loginWith(w http.ResponseWriter, r *http.Request, user, pass string) {
	ip := clientIP(r)
	if !s.auth.allowLogin(ip) {
		s.logf("console[%s]: login for %q from %s blocked after repeated failures", s.dbLabel(), user, ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts; wait a minute"})
		return
	}
	if !s.store.AuthActive() {
		// Sin usuarios no hay credenciales que validar: "invalid credentials" sería engañoso.
		s.logf("console[%s]: login refused for %q from %s: database has no users yet "+
			"(create the first one in the setup form)", s.dbLabel(), user, ip)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this database has no users yet: create the first user (setup)",
		})
		return
	}
	sess, err := s.store.Authenticate(user, pass)
	if err != nil {
		s.auth.loginFailed(ip)
		s.logf("console[%s]: login failed for %q from %s: %s", s.dbLabel(), user, ip, authFailReason(s.store, user))
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials"})
		return
	}
	s.auth.loginOK(ip)
	s.logf("console[%s]: login OK for %q from %s (roles=%v)", s.dbLabel(), user, ip, sess.Roles())
	ws := s.auth.register(sess, ip)
	setSessionCookie(w, ws.token)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":  user,
		"roles": sess.Roles(),
		"csrf":  ws.csrf,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if ws := s.sessionFromRequest(r); ws != nil {
		ws.sess.Revoke()
		s.auth.drop(ws.token)
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, ws *webSession) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user":  ws.sess.User(),
		"roles": ws.sess.Roles(),
		"csrf":  ws.csrf,
	})
}
