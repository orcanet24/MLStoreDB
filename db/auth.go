package db

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// System collections (RBAC, M4). Created lazily on first admin API call —
// never in New()/Open — so Collections() stays clean until users exist.
// Hidden from Collections() and unreachable via generic CRUD (admin APIs
// write them through dedicated paths only).
const (
	usersColl = "_users"
	rolesColl = "_roles"

	// Password KDF (argon2id, PHC-style string). Params follow LightKDF.
	pwSaltLen = 16
	pwKeyLen  = 32

	defaultSessionTTL = 24 * time.Hour
	sessionTokenLen   = 32
)

// Permission is one entry of a role's access list (stored in _roles docs).
// Collection "*" matches every non-system collection.
type Permission struct {
	Collection string   `json:"collection"`
	Read       bool     `json:"read"`
	Write      bool     `json:"write"`
	FieldDeny  []string `json:"fieldDeny,omitempty"`
}

// Session is an authenticated principal with a snapshot of role permissions
// resolved at Authenticate time. Obtain via Store.Authenticate.
// Safe for concurrent use.
type Session struct {
	store   *Store
	token   string
	user    string
	roles   []string
	perms   []Permission
	expires time.Time // zero = never expires
	system  bool      // hookBypass principal (M5a): trusted, not in sessions map
}

// User returns the authenticated username.
func (sess *Session) User() string { return sess.user }

// Roles returns a copy of the role names resolved at Authenticate.
func (sess *Session) Roles() []string { return append([]string{}, sess.roles...) }

// Token returns the session token (for Store.Revoke).
func (sess *Session) Token() string { return sess.token }

// Revoke invalidates this session immediately.
func (sess *Session) Revoke() { sess.store.Revoke(sess.token) }

// --- RBAC checks (caller must hold s.mu) ---

func isSystemColl(name string) bool {
	return name == usersColl || name == rolesColl || name == triggersColl
}

// authActiveLocked reports whether RBAC enforcement is on (≥1 user exists).
func (s *Store) authActiveLocked() bool {
	c, ok := s.collections[usersColl]
	return ok && len(c.entries) > 0
}

// AuthActive reports whether RBAC enforcement is on (≥1 user exists).
// While false, the raw Store API is fully open (permissive default).
func (s *Store) AuthActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authActiveLocked()
}

// checkReadLocked gates a read. nil sess = raw Store API (allowed only while
// auth is inactive). Non-nil = valid session with read permission.
func (s *Store) checkReadLocked(sess *Session, coll string) error {
	if sess == nil {
		if s.authActiveLocked() {
			return ErrUnauthorized
		}
		return nil
	}
	if !sess.validLocked() {
		return ErrUnauthorized
	}
	if isSystemColl(coll) {
		return ErrForbidden
	}
	if !sess.canRead(coll) {
		return ErrForbidden
	}
	return nil
}

// checkWriteLocked gates a write. System collections are never writable
// through generic CRUD (prevents fake-user docs / hash tampering).
func (s *Store) checkWriteLocked(sess *Session, coll string) error {
	if isSystemColl(coll) {
		return ErrForbidden
	}
	if sess == nil {
		if s.authActiveLocked() {
			return ErrUnauthorized
		}
		return nil
	}
	if !sess.validLocked() {
		return ErrUnauthorized
	}
	if !sess.canWrite(coll) {
		return ErrForbidden
	}
	return nil
}

// validLocked: not expired and still registered (Revoke removes the entry).
// system (hookBypass) sessions are always valid. Caller holds s.mu.
func (sess *Session) validLocked() bool {
	if sess.system {
		return true
	}
	if !sess.expires.IsZero() && time.Now().After(sess.expires) {
		return false
	}
	cur, ok := sess.store.sessions[sess.token]
	return ok && cur == sess
}

func (sess *Session) canRead(coll string) bool {
	if sess.system {
		return true
	}
	for _, p := range sess.perms {
		if (p.Collection == coll || p.Collection == "*") && p.Read {
			return true
		}
	}
	return false
}

func (sess *Session) canWrite(coll string) bool {
	if sess.system {
		return true
	}
	for _, p := range sess.perms {
		if (p.Collection == coll || p.Collection == "*") && p.Write {
			return true
		}
	}
	return false
}

// fieldDenySet unions FieldDeny from permissions matching coll (or "*").
func (sess *Session) fieldDenySet(coll string) map[string]bool {
	var out map[string]bool
	for _, p := range sess.perms {
		if p.Collection != coll && p.Collection != "*" {
			continue
		}
		for _, f := range p.FieldDeny {
			if out == nil {
				out = make(map[string]bool, len(p.FieldDeny))
			}
			out[f] = true
		}
	}
	return out
}

// redactDoc removes denied fields from an owned clone.
func redactDoc(doc Document, deny map[string]bool) {
	for f := range deny {
		delete(doc, f)
	}
}

// filterTouchesFields reports whether filter references any denied field
// (top-level keys + recursion into $and/$or/$nor/$not) — blocks probing
// hidden values via Count/Find predicates.
func filterTouchesFields(deny map[string]bool, filter Document) bool {
	for k, v := range filter {
		if strings.HasPrefix(k, "$") {
			switch k {
			case "$and", "$or", "$nor":
				list, err := toDocList(v)
				if err != nil {
					continue
				}
				for _, sub := range list {
					if filterTouchesFields(deny, sub) {
						return true
					}
				}
			case "$not":
				if sub, err := toDoc(v); err == nil && filterTouchesFields(deny, sub) {
					return true
				}
			}
			continue
		}
		if deny[k] {
			return true
		}
	}
	return false
}

// --- Session data API (permission-checked + fieldDeny redaction) ---

// Get returns a copy of the doc with fieldDeny fields removed.
func (sess *Session) Get(coll string, id string) (Document, error) {
	return sess.store.getDoc(sess, coll, id)
}

// Find is Store.Find under session permissions (fieldDeny filters forbidden,
// denied fields redacted from results).
func (sess *Session) Find(coll string, filter Document, opts *FindOptions) ([]Document, error) {
	return sess.store.findDocs(sess, coll, filter, opts)
}

// Count is Store.Count under session permissions.
func (sess *Session) Count(coll string, filter Document) (int, error) {
	return sess.store.countDocs(sess, coll, filter)
}

// Insert is Store.Insert with write permission.
func (sess *Session) Insert(coll string, doc Document) error {
	if err := sess.store.insertDoc(sess, coll, doc); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// Upsert is Store.Upsert with write permission.
func (sess *Session) Upsert(coll string, id string, doc Document) error {
	if err := sess.store.upsertDoc(sess, coll, id, doc); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// Update is Store.Update with write permission.
func (sess *Session) Update(coll string, id string, patch Document) error {
	if err := sess.store.updateDoc(sess, coll, id, patch); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// Delete is Store.Delete with write permission.
func (sess *Session) Delete(coll string, id string) error {
	if err := sess.store.deleteDoc(sess, coll, id); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// ExportCSV is Store.ExportCSV under read permission (+ fieldDeny hidden).
func (sess *Session) ExportCSV(coll string, w io.Writer) error {
	return sess.store.exportCSV(sess, coll, w)
}

// --- Admin APIs (trusted bootstrap / privileged app code; not session-gated) ---

// CreateUser stores a user in _users (lazily created) with an argon2id
// password hash. Roles must already exist (CreateRole). The first user
// activates RBAC: afterwards the raw Store data API returns ErrUnauthorized.
func (s *Store) CreateUser(username, password string, roles []string) error {
	if username == "" {
		return fmt.Errorf("db: username required")
	}
	if password == "" {
		return fmt.Errorf("db: password required")
	}
	hash, err := s.hashPassword(password)
	if err != nil {
		return err
	}
	if err := s.createUser(username, hash, roles); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) createUser(username, hash string, roles []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(roles) > 0 {
		rc, ok := s.collections[rolesColl]
		if !ok {
			return fmt.Errorf("db: no roles defined")
		}
		for _, r := range roles {
			if _, exists := rc.entries[r]; !exists {
				return fmt.Errorf("db: role %q not found", r)
			}
		}
	}
	c := s.coll(usersColl) // lazy: only now does _users appear
	if _, exists := c.entries[username]; exists {
		return ErrDuplicate
	}
	rolesAny := make([]any, len(roles))
	for i, r := range roles {
		rolesAny[i] = r
	}
	stored := clone(Document{
		"_id":           username,
		"password_hash": hash,
		"roles":         rolesAny,
	})
	e := newDocEntry(stored)
	s.markEntryMutated(e)
	c.entries[username] = e
	s.cacheAdmit(e)
	return nil
}

// CreateRole stores a role (lazily creates _roles) with its permission list.
func (s *Store) CreateRole(name string, perms []Permission) error {
	if name == "" {
		return fmt.Errorf("db: role name required")
	}
	if err := s.createRole(name, perms); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) createRole(name string, perms []Permission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.coll(rolesColl)
	if _, exists := c.entries[name]; exists {
		return ErrDuplicate
	}
	permAny := make([]any, len(perms))
	for i, p := range perms {
		d := Document{
			"collection": p.Collection,
			"read":       p.Read,
			"write":      p.Write,
		}
		if len(p.FieldDeny) > 0 {
			fd := make([]any, len(p.FieldDeny))
			for j, f := range p.FieldDeny {
				fd[j] = f
			}
			d["fieldDeny"] = fd
		}
		permAny[i] = d
	}
	stored := clone(Document{"_id": name, "permissions": permAny})
	e := newDocEntry(stored)
	s.markEntryMutated(e)
	c.entries[name] = e
	s.cacheAdmit(e)
	return nil
}

// ChangePassword re-hashes the password for username and revokes all
// sessions belonging to that user.
func (s *Store) ChangePassword(username, newPassword string) error {
	if username == "" || newPassword == "" {
		return fmt.Errorf("db: username and password required")
	}
	hash, err := s.hashPassword(newPassword)
	if err != nil {
		return err
	}
	s.mu.Lock()
	c, ok := s.collections[usersColl]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	e, exists := c.entries[username]
	if !exists {
		s.mu.Unlock()
		return ErrNotFound
	}
	doc, err := s.resident(c, username)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	next := clone(doc)
	next["password_hash"] = hash
	e.docP.Store(&next)
	s.markEntryMutated(e)
	for tok, sess := range s.sessions {
		if sess.user == username {
			delete(s.sessions, tok)
		}
	}
	s.mu.Unlock()
	return s.afterMutation()
}

// Revoke removes a session by token (no-op if unknown/empty).
func (s *Store) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// Authenticate verifies username/password and returns a Session.
// Unknown user and bad password both yield ErrUnauthorized (no user enum).
// Sessions are purged lazily here (expired entries GC'd on each login).
func (s *Store) Authenticate(username, password string) (*Session, error) {
	if username == "" || password == "" {
		return nil, ErrUnauthorized
	}
	s.mu.RLock()
	var doc Document
	var err error
	if c, ok := s.collections[usersColl]; ok {
		doc, err = s.resident(c, username)
	} else {
		err = ErrNotFound
	}
	ttl := s.opts.sessionTTL()
	s.mu.RUnlock()
	if err != nil || doc == nil {
		return nil, ErrUnauthorized
	}
	if dis, ok := doc["disabled"].(bool); ok && dis {
		return nil, ErrUnauthorized
	}
	hash, _ := doc["password_hash"].(string)
	if hash == "" || !verifyPassword(password, hash) {
		return nil, ErrUnauthorized
	}
	roles := parseRoles(doc["roles"])
	perms := s.resolveRolePerms(roles)
	raw := make([]byte, sessionTokenLen)
	if _, err := readRandom(raw); err != nil {
		return nil, err
	}
	sess := &Session{
		store: s,
		token: base64.RawURLEncoding.EncodeToString(raw),
		user:  username,
		roles: roles,
		perms: perms,
	}
	if ttl > 0 {
		sess.expires = time.Now().Add(ttl)
	} // ttl < 0 → never expires
	s.mu.Lock()
	s.purgeExpiredLocked()
	s.sessions[sess.token] = sess
	s.mu.Unlock()
	return sess, nil
}

// purgeExpiredLocked drops expired sessions. Caller holds s.mu (write).
func (s *Store) purgeExpiredLocked() {
	now := time.Now()
	for tok, sess := range s.sessions {
		if !sess.expires.IsZero() && now.After(sess.expires) {
			delete(s.sessions, tok)
		}
	}
}

// resolveRolePerms loads and flattens permissions for role names.
func (s *Store) resolveRolePerms(names []string) []Permission {
	if len(names) == 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rc, ok := s.collections[rolesColl]
	if !ok {
		return nil
	}
	var out []Permission
	for _, name := range names {
		if _, exists := rc.entries[name]; !exists {
			continue
		}
		doc, err := s.resident(rc, name)
		if err != nil || doc == nil {
			continue
		}
		out = append(out, parsePermissions(doc["permissions"])...)
	}
	return out
}

// --- password hashing (argon2id PHC string) ---

func (s *Store) hashPassword(pw string) (string, error) {
	t, m, p := defaultKDFTime, defaultKDFMem, defaultKDFPar
	if s.opts.LightKDF {
		t, m, p = lightKDFTime, lightKDFMem, lightKDFPar
	}
	return hashArgon2id(pw, t, m, uint8(p))
}

func hashArgon2id(pw string, t, m uint32, p uint8) (string, error) {
	salt := make([]byte, pwSaltLen)
	if _, err := readRandom(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(pw), salt, t, m, p, pwKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		m, t, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// verifyPassword checks pw against a PHC-style argon2id string.
func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- doc field parsers (RAM shape or JSON-roundtripped shape) ---

func parsePermissions(v any) []Permission {
	switch t := v.(type) {
	case []Permission:
		return append([]Permission{}, t...)
	case []any:
		var out []Permission
		for _, e := range t {
			if p, ok := permFromAny(e); ok {
				out = append(out, p)
			}
		}
		return out
	case nil:
		return nil
	}
	return nil
}

func permFromAny(e any) (Permission, bool) {
	switch m := e.(type) {
	case Permission:
		return m, true
	case Document:
		return permFromMap(m), true
	case map[string]any:
		return permFromMap(m), true
	}
	return Permission{}, false
}

func permFromMap(m map[string]any) Permission {
	var p Permission
	if s, ok := m["collection"].(string); ok {
		p.Collection = s
	}
	if b, ok := m["read"].(bool); ok {
		p.Read = b
	}
	if b, ok := m["write"].(bool); ok {
		p.Write = b
	}
	switch fd := m["fieldDeny"].(type) {
	case []string:
		p.FieldDeny = append([]string{}, fd...)
	case []any:
		for _, x := range fd {
			if s, ok := x.(string); ok {
				p.FieldDeny = append(p.FieldDeny, s)
			}
		}
	}
	return p
}

func parseRoles(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string{}, t...)
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case nil:
		return nil
	}
	return nil
}
