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

// Colecciones del sistema (RBAC, M4). Se crean de forma perezosa en la primera llamada
// a la API de administración — nunca en New()/Open — para que Collections() siga limpio
// hasta que existan usuarios. Están ocultas para Collections() y no son accesibles por el
// CRUD genérico (las APIs de administración solo escriben en ellas por caminos dedicados).
const (
	usersColl = "_users"
	rolesColl = "_roles"

	// KDF de contraseñas (argon2id, cadena estilo PHC). Los parámetros siguen LightKDF.
	pwSaltLen = 16
	pwKeyLen  = 32

	defaultSessionTTL = 24 * time.Hour
	sessionTokenLen   = 32
)

// Permission es una entrada de la lista de acceso de un rol (se guarda en documentos de _roles).
// La colección "*" coincide con todas las colecciones que no son del sistema.
type Permission struct {
	Collection string   `json:"collection"`
	Read       bool     `json:"read"`
	Write      bool     `json:"write"`
	FieldDeny  []string `json:"fieldDeny,omitempty"`
}

// RoleInfo es un rol tal y como se expone en la interfaz de administración (admin M9).
type RoleInfo struct {
	Name        string       `json:"name"`
	Permissions []Permission `json:"permissions"`
}

// Session es un principal autenticado con una instantánea de los permisos de rol
// resueltos en el momento de Authenticate. Se obtiene con Store.Authenticate.
// Seguro para uso concurrente.
type Session struct {
	store   *Store
	token   string
	user    string
	roles   []string
	perms   []Permission
	expires time.Time // cero = nunca caduca
	system  bool      // principal hookBypass (M5a): de confianza, no está en el mapa de sesiones
}

// User devuelve el nombre de usuario autenticado.
func (sess *Session) User() string { return sess.user }

// Roles devuelve una copia de los nombres de rol resueltos en Authenticate.
func (sess *Session) Roles() []string { return append([]string{}, sess.roles...) }

// Token devuelve el token de sesión (para Store.Revoke).
func (sess *Session) Token() string { return sess.token }

// Revoke invalida esta sesión de inmediato.
func (sess *Session) Revoke() { sess.store.Revoke(sess.token) }

// --- comprobaciones RBAC (el llamador debe tener s.mu) ---

func isSystemColl(name string) bool {
	return name == usersColl || name == rolesColl || name == triggersColl
}

// authActiveLocked indica si la aplicación de RBAC está activa (existe al menos 1 usuario).
func (s *Store) authActiveLocked() bool {
	c, ok := s.collections[usersColl]
	return ok && len(c.entries) > 0
}

// AuthActive indica si la aplicación de RBAC está activa (existe al menos 1 usuario).
// Mientras es false, la API cruda del Store está totalmente abierta (modo permisivo por defecto).
func (s *Store) AuthActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authActiveLocked()
}

// checkReadLocked controla una lectura. sess nil = API cruda del Store (permitida solo
// mientras la autenticación está inactiva). No nil = sesión válida con permiso de lectura.
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

// checkWriteLocked controla una escritura. Las colecciones del sistema nunca son
// escribibles desde el CRUD genérico (evita documentos de usuario falsos / manipulación de hashes).
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

// validLocked: no caducada y aún registrada (Revoke elimina la entrada).
// Las sesiones system (hookBypass) siempre son válidas. El llamador tiene s.mu.
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

// fieldDenySet une los FieldDeny de los permisos que coinciden con coll (o con "*").
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

// redactDoc elimina los campos denegados de una copia propia.
func redactDoc(doc Document, deny map[string]bool) {
	for f := range deny {
		delete(doc, f)
	}
}

// filterTouchesFields indica si el filtro referencia algún campo denegado
// (claves de primer nivel + recursión en $and/$or/$nor/$not) — bloquea la
// sondeabilidad de valores ocultos mediante predicados de Count/Find.
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

// CanWrite indica si la sesión puede escribir en coll (interfaz de admin M9).
func (sess *Session) CanWrite(coll string) bool { return sess.canWrite(coll) }

// CanRead indica si la sesión puede leer de coll (interfaz de admin M9).
func (sess *Session) CanRead(coll string) bool { return sess.canRead(coll) }

// --- API de datos de la sesión (con comprobación de permisos + redacción fieldDeny) ---

// Get devuelve una copia del documento con los campos de fieldDeny eliminados.
func (sess *Session) Get(coll string, id string) (Document, error) {
	return sess.store.getDoc(sess, coll, id)
}

// Find es Store.Find con los permisos de la sesión (se prohíben los filtros que
// tocan fieldDeny y se redactan los campos denegados en los resultados).
func (sess *Session) Find(coll string, filter Document, opts *FindOptions) ([]Document, error) {
	return sess.store.findDocs(sess, coll, filter, opts)
}

// Count es Store.Count con los permisos de la sesión.
func (sess *Session) Count(coll string, filter Document) (int, error) {
	return sess.store.countDocs(sess, coll, filter)
}

// Insert es Store.Insert con permiso de escritura.
func (sess *Session) Insert(coll string, doc Document) error {
	if err := sess.store.insertDoc(sess, coll, doc); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// Upsert es Store.Upsert con permiso de escritura.
func (sess *Session) Upsert(coll string, id string, doc Document) error {
	if err := sess.store.upsertDoc(sess, coll, id, doc); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// Update es Store.Update con permiso de escritura.
func (sess *Session) Update(coll string, id string, patch Document) error {
	if err := sess.store.updateDoc(sess, coll, id, patch); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// UpdateFields es Store.UpdateFields con permiso de escritura (wire M8).
func (sess *Session) UpdateFields(coll string, id string, patch Document, remove []string) error {
	if err := sess.store.updateDocFrame(nil, sess, coll, id, patch, remove); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// EnsureIndex es Store.EnsureIndex con permiso de escritura sobre coll (wire M8).
func (sess *Session) EnsureIndex(coll string, fields []string, unique bool) error {
	if err := sess.store.previewWrite(sess, coll); err != nil {
		return err
	}
	return sess.store.EnsureIndex(coll, fields, unique)
}

// DropIndex es Store.DropIndex con permiso de escritura sobre coll (wire M8).
func (sess *Session) DropIndex(coll string, fields []string) error {
	if err := sess.store.previewWrite(sess, coll); err != nil {
		return err
	}
	return sess.store.DropIndex(coll, fields)
}

// ListIndexes es Store.ListIndexes con permiso de lectura sobre coll (wire M8).
func (sess *Session) ListIndexes(coll string) ([]IndexInfo, error) {
	sess.store.mu.RLock()
	err := sess.store.checkReadLocked(sess, coll)
	sess.store.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return sess.store.ListIndexes(coll)
}

// Collections es Store.Collections filtrado por permiso de lectura (wire M8).
func (sess *Session) Collections() []string {
	var out []string
	for _, c := range sess.store.Collections() {
		if isSystemColl(c) {
			continue
		}
		if sess.canRead(c) {
			out = append(out, c)
		}
	}
	return out
}

// CreateCollection es Store.CreateCollection con permiso de escritura (wire M8).
func (sess *Session) CreateCollection(coll string) error {
	return sess.store.createCollection(sess, coll)
}

// DropCollection es Store.DropCollection con permiso de escritura (wire M8).
func (sess *Session) DropCollection(coll string) error {
	return sess.store.dropCollection(sess, coll)
}

// Delete es Store.Delete con permiso de escritura.
func (sess *Session) Delete(coll string, id string) error {
	if err := sess.store.deleteDoc(sess, coll, id); err != nil {
		return err
	}
	return sess.store.afterMutation()
}

// ExportCSV es Store.ExportCSV con permiso de lectura (+ campos de fieldDeny ocultos).
func (sess *Session) ExportCSV(coll string, w io.Writer) error {
	return sess.store.exportCSV(sess, coll, w)
}

// --- APIs de administración (bootstrap de confianza / código privilegiado; sin control de sesión) ---

// CreateUser guarda un usuario en _users (creada de forma perezosa) con un hash de
// contraseña argon2id. Los roles deben existir ya (CreateRole). El primer usuario
// activa el RBAC: a partir de ahí la API de datos cruda del Store devuelve ErrUnauthorized.
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
	c := s.coll(usersColl) // perezoso: _users solo aparece ahora
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

// CreateRole guarda un rol (crea _roles de forma perezosa) con su lista de permisos.
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

// UserInfo es un usuario tal y como se expone en la interfaz de administración (sin hash de contraseña).
type UserInfo struct {
	Username string   `json:"username"`
	Roles    []string `json:"roles"`
}

// ListUsers devuelve todos los usuarios (API de administración de confianza). Admin M9.
func (s *Store) ListUsers() []UserInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collections[usersColl]
	if !ok {
		return []UserInfo{}
	}
	out := make([]UserInfo, 0, len(c.entries))
	for id, e := range c.entries {
		doc := Document{}
		if p := e.docP.Load(); p != nil {
			doc = *p
		}
		rolesAny, _ := doc["roles"].([]any)
		roles := make([]string, 0, len(rolesAny))
		for _, r := range rolesAny {
			if rs, ok := r.(string); ok {
				roles = append(roles, rs)
			}
		}
		out = append(out, UserInfo{Username: id, Roles: roles})
	}
	return out
}

// ListRoles devuelve todos los roles con sus permisos (administración de confianza). Admin M9.
func (s *Store) ListRoles() []RoleInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collections[rolesColl]
	if !ok {
		return []RoleInfo{}
	}
	out := make([]RoleInfo, 0, len(c.entries))
	for id, e := range c.entries {
		doc := Document{}
		if p := e.docP.Load(); p != nil {
			doc = *p
		}
		permAny, _ := doc["permissions"].([]any)
		perms := make([]Permission, 0, len(permAny))
		for _, p := range permAny {
			switch pm := p.(type) {
			case map[string]any:
				perms = append(perms, permFromMap(pm))
			case Document:
				perms = append(perms, permFromMap(map[string]any(pm)))
			}
		}
		out = append(out, RoleInfo{Name: id, Permissions: perms})
	}
	return out
}

// SystemSession devuelve una Session de confianza con permisos comodín (equivalente
// al hookBypass de M5a). La usa la consola de administración cuando el RBAC está
// inactivo (modo permisivo) para que la API cruda del Store funcione sin login.
func (s *Store) SystemSession() *Session {
	return &Session{store: s, token: "system", perms: []Permission{{Collection: "*", Read: true, Write: true}}, system: true}
}

// ResetAuth borra todas las credenciales (usuarios, roles, sesiones activas) sin tocar
// ninguna colección de datos. Después, AuthActive() devuelve false y la API cruda del
// Store vuelve a funcionar sin autenticación (modo permisivo). Las credenciales se borran
// como registros DEL reales para que no resuciten al reabrir. Admin M9.
func (s *Store) ResetAuth() error {
	s.mu.Lock()
	if !s.authActiveLocked() {
		s.mu.Unlock()
		return nil
	}
	s.purgeAllSessionsLocked()
	if uc, ok := s.collections[usersColl]; ok {
		for id := range uc.entries {
			_ = s.deleteRecordLocked(usersColl, id)
		}
		delete(s.collections, usersColl)
	}
	if rc, ok := s.collections[rolesColl]; ok {
		for id := range rc.entries {
			_ = s.deleteRecordLocked(rolesColl, id)
		}
		delete(s.collections, rolesColl)
	}
	s.mu.Unlock()
	return nil
}

// deleteRecordLocked escribe un registro DEL para id en coll sin comprobar permisos.
// El llamador debe tener s.mu (escritura). El DEL solo se encola si hay respaldo en archivo.
func (s *Store) deleteRecordLocked(coll, id string) error {
	c, ok := s.collRO(coll)
	if !ok {
		return ErrNotFound
	}
	e, ok := c.entries[id]
	if !ok {
		return ErrNotFound
	}
	if s.path != "" && (s.sawFile || s.flushInProgress) {
		s.pendingDels = append(s.pendingDels, delItem{coll: coll, id: id})
		s.markEntryMutated(e)
	}
	delete(c.entries, id)
	return nil
}

func (s *Store) purgeAllSessionsLocked() {
	for token := range s.sessions {
		delete(s.sessions, token)
	}
}

// DeleteUser elimina un usuario y revoca sus sesiones (administración de confianza). Admin M9.
func (s *Store) DeleteUser(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collections[usersColl]
	if !ok {
		return ErrNotFound
	}
	e, exists := c.entries[username]
	if !exists {
		return ErrNotFound
	}
	delete(c.entries, username)
	if e.docP.Load() != nil {
		if n := e.size.Load(); n > 0 {
			s.residentBytes -= n
			if s.residentBytes < 0 {
				s.residentBytes = 0
			}
		}
		delete(s.cache, e)
	}
	s.collections[usersColl] = c
	s.markDirty()
	return nil
}

// SetUserRoles reemplaza in situ la lista de roles de un usuario existente: se conservan
// el hash de contraseña y los demás campos, y solo las sesiones de los roles afectados
// necesitan reautenticarse en el próximo login (administración de confianza).
func (s *Store) SetUserRoles(username string, roles []string) error {
	s.mu.Lock()
	if len(roles) > 0 {
		rc, ok := s.collections[rolesColl]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("db: no roles defined")
		}
		for _, r := range roles {
			if _, exists := rc.entries[r]; !exists {
				s.mu.Unlock()
				return fmt.Errorf("db: role %q not found", r)
			}
		}
	}
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
	rolesAny := make([]any, len(roles))
	for i, r := range roles {
		rolesAny[i] = r
	}
	next["roles"] = rolesAny
	e.docP.Store(&next)
	s.markEntryMutated(e)
	s.mu.Unlock()
	return s.afterMutation()
}

// DeleteRole elimina un rol si ningún usuario lo referencia (administración de confianza). Admin M9.
func (s *Store) DeleteRole(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	uc, ok := s.collections[usersColl]
	if ok {
		for _, e := range uc.entries {
			doc := Document{}
			if p := e.docP.Load(); p != nil {
				doc = *p
			}
			rolesAny, _ := doc["roles"].([]any)
			for _, r := range rolesAny {
				if rs, ok := r.(string); ok && rs == name {
					return fmt.Errorf("db: role %q is in use", name)
				}
			}
		}
	}
	c, ok := s.collections[rolesColl]
	if !ok {
		return ErrNotFound
	}
	if _, exists := c.entries[name]; !exists {
		return ErrNotFound
	}
	delete(c.entries, name)
	s.collections[rolesColl] = c
	s.markDirty()
	return nil
}

// ChangePassword vuelve a calcular el hash de la contraseña de username y revoca
// todas las sesiones de ese usuario.
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

// Revoke elimina una sesión por token (no hace nada si es desconocido o vacío).
func (s *Store) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// Authenticate verifica usuario/contraseña y devuelve una Session.
// Tanto un usuario desconocido como una contraseña incorrecta devuelven ErrUnauthorized (sin enumerar usuarios).
// Las sesiones se purgan de forma perezosa aquí (las entradas caducadas se recolectan en cada login).
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
	} // ttl < 0 → nunca caduca
	s.mu.Lock()
	s.purgeExpiredLocked()
	s.sessions[sess.token] = sess
	s.mu.Unlock()
	return sess, nil
}

// purgeExpiredLocked descarta las sesiones caducadas. El llamador tiene s.mu (escritura).
func (s *Store) purgeExpiredLocked() {
	now := time.Now()
	for tok, sess := range s.sessions {
		if !sess.expires.IsZero() && now.After(sess.expires) {
			delete(s.sessions, tok)
		}
	}
}

// resolveRolePerms carga y aplana los permisos de los nombres de rol indicados.
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

// --- hash de contraseñas (cadena PHC de argon2id) ---

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

// verifyPassword comprueba pw contra una cadena argon2id estilo PHC.
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

// --- analizadores de campos de documento (forma en RAM o forma tras pasar por JSON) ---

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
