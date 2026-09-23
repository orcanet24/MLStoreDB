package wire

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mlstoredb/db"
)

type ServerOptions struct {
	DBName   string
	AuthUser string
	AuthPass string
}

type Server struct {
	store    *db.Store
	opts     ServerOptions
	cursors  *cursorRegistry
	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	closed   atomic.Bool
	wg       sync.WaitGroup
	connSeq  atomic.Int64
	started  time.Time
}

func NewServer(store *db.Store, o ServerOptions) *Server {
	if o.DBName == "" {
		o.DBName = "mlstoredb"
	}
	return &Server{
		store:   store,
		opts:    o,
		cursors: newCursorRegistry(),
		conns:   make(map[net.Conn]struct{}),
		started: time.Now(),
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
	if s.closed.Load() {
		s.mu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed.Load() {
			s.mu.Unlock()
			_ = c.Close()
			return nil
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveConn(c)
	}
}

func (s *Server) Close() error {
	s.closed.Store(true)
	s.mu.Lock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

type connCtx struct {
	srv     *Server
	id      int64
	remote  string
	session *db.Session
	scram   *scramState
	authed  bool
	respSeq int32
}

func (c *connCtx) nextResponseID() int32 {
	c.respSeq++
	return c.respSeq
}

func (c *connCtx) collections() []string {
	if c.session != nil {
		return c.session.Collections()
	}
	return c.srv.store.Collections()
}

func (c *connCtx) get(coll, id string) (db.Document, error) {
	if c.session != nil {
		return c.session.Get(coll, id)
	}
	return c.srv.store.Get(coll, id)
}

func (c *connCtx) find(coll string, filter db.Document, opts *db.FindOptions) ([]db.Document, error) {
	if c.session != nil {
		return c.session.Find(coll, filter, opts)
	}
	return c.srv.store.Find(coll, filter, opts)
}

func (c *connCtx) count(coll string, filter db.Document) (int, error) {
	if c.session != nil {
		return c.session.Count(coll, filter)
	}
	return c.srv.store.Count(coll, filter)
}

func (c *connCtx) insert(coll string, doc db.Document) error {
	if c.session != nil {
		return c.session.Insert(coll, doc)
	}
	return c.srv.store.Insert(coll, doc)
}

func (c *connCtx) upsert(coll, id string, doc db.Document) error {
	if c.session != nil {
		return c.session.Upsert(coll, id, doc)
	}
	return c.srv.store.Upsert(coll, id, doc)
}

func (c *connCtx) updateFields(coll, id string, patch db.Document, remove []string) error {
	if c.session != nil {
		return c.session.UpdateFields(coll, id, patch, remove)
	}
	return c.srv.store.UpdateFields(coll, id, patch, remove)
}

func (c *connCtx) delete(coll, id string) error {
	if c.session != nil {
		return c.session.Delete(coll, id)
	}
	return c.srv.store.Delete(coll, id)
}

func (c *connCtx) ensureIndex(coll string, fields []string, unique bool) error {
	if c.session != nil {
		return c.session.EnsureIndex(coll, fields, unique)
	}
	return c.srv.store.EnsureIndex(coll, fields, unique)
}

func (c *connCtx) dropIndex(coll string, fields []string) error {
	if c.session != nil {
		return c.session.DropIndex(coll, fields)
	}
	return c.srv.store.DropIndex(coll, fields)
}

func (c *connCtx) listIndexes(coll string) ([]db.IndexInfo, error) {
	if c.session != nil {
		return c.session.ListIndexes(coll)
	}
	return c.srv.store.ListIndexes(coll)
}

func (c *connCtx) createCollection(coll string) error {
	if c.session != nil {
		return c.session.CreateCollection(coll)
	}
	return c.srv.store.CreateCollection(coll)
}

func (c *connCtx) dropCollection(coll string) error {
	if c.session != nil {
		return c.session.DropCollection(coll)
	}
	return c.srv.store.DropCollection(coll)
}

func (s *Server) serveConn(c net.Conn) {
	defer s.wg.Done()
	ctx := &connCtx{
		srv:    s,
		id:     s.connSeq.Add(1),
		remote: c.RemoteAddr().String(),
	}
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(cursorIdleTTL * 2))
		m, err := readMessage(r)
		if err != nil {
			return
		}
		s.cursors.sweep()
		if err := s.dispatchSafe(ctx, w, m); err != nil {
			return
		}
	}
}

func (s *Server) dispatchSafe(ctx *connCtx, w *bufio.Writer, m *message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errWire(1, "InternalError", "internal server error")
			_ = err
		}
	}()
	return s.dispatch(ctx, w, m)
}

func (s *Server) dispatch(ctx *connCtx, w *bufio.Writer, m *message) error {
	switch {
	case m.opCode == opMsg:
		if m.moreToCome {
			_ = s.runCommand(ctx, m.body, m.bodyKeys)
			return nil
		}
		reply := s.runCommand(ctx, m.body, m.bodyKeys)
		return writeOpMsg(w, m.requestID, ctx.nextResponseID(), reply)
	case m.opCode == opQuery && m.isLegacyFind:
		return s.dispatchLegacyFind(ctx, w, m)
	case m.opCode == opQuery:
		reply := s.runCommand(ctx, m.body, m.bodyKeys)
		return writeOpReply(w, m.requestID, ctx.nextResponseID(), 0, 0, []db.Document{reply})
	case m.opCode == opGetMore:
		reply := s.runCommand(ctx, m.body, m.bodyKeys)
		docs, next, ok := cursorFromReply(reply)
		if !ok {
			return writeOpReply(w, m.requestID, ctx.nextResponseID(), 0, 0, []db.Document{reply})
		}
		return writeOpReply(w, m.requestID, ctx.nextResponseID(), next, int32(len(docs)), docs)
	case m.opCode == opKillCursors:
		_ = s.runCommand(ctx, m.body, m.bodyKeys)
		return nil
	}
	return nil
}

func cursorFromReply(reply db.Document) (docs []db.Document, next int64, ok bool) {
	cur, ok := reply["cursor"].(db.Document)
	if !ok {
		return nil, 0, false
	}
	batch, _ := cur["nextBatch"].([]any)
	out := make([]db.Document, 0, len(batch))
	for _, d := range batch {
		if doc, ok := d.(db.Document); ok {
			out = append(out, doc)
		}
	}
	id, _ := toInt64(cur["id"])
	return out, id, true
}

func (s *Server) dispatchLegacyFind(ctx *connCtx, w *bufio.Writer, m *message) error {
	coll := collOf(m.fullColl)
	filter := m.queryDoc
	sortSpec := db.Document{}
	if ord, ok := m.queryDoc["$orderby"].(db.Document); ok {
		sortSpec = ord
		trimmed := db.Document{}
		for k, v := range m.queryDoc {
			if k == "$orderby" || k == "$query" || k == "$hint" || k == "$comment" || k == "$maxTimeMS" {
				continue
			}
			trimmed[k] = v
		}
		filter = trimmed
	} else if q, ok := m.queryDoc["$query"].(db.Document); ok {
		filter = q
		trimmed := db.Document{}
		for k, v := range m.queryDoc {
			if k == "$query" || k == "$orderby" || k == "$hint" || k == "$comment" || k == "$maxTimeMS" {
				continue
			}
			trimmed[k] = v
		}
		filter = trimmed
	}
	cmd := db.Document{"find": coll}
	cmdKeys := []string{"find"}
	if len(filter) > 0 {
		cmd["filter"] = filter
		cmdKeys = append(cmdKeys, "filter")
	}
	if len(sortSpec) > 0 {
		cmd["sort"] = sortSpec
		cmdKeys = append(cmdKeys, "sort")
	}
	if len(m.fields) > 0 {
		cmd["projection"] = m.fields
		cmdKeys = append(cmdKeys, "projection")
	}
	if m.nSkip > 0 {
		cmd["skip"] = float64(m.nSkip)
		cmdKeys = append(cmdKeys, "skip")
	}
	single := false
	if m.nReturn < 0 {
		single = true
		cmd["limit"] = float64(-m.nReturn)
		cmdKeys = append(cmdKeys, "limit")
	} else if m.nReturn > 0 {
		cmd["batchSize"] = float64(m.nReturn)
		cmdKeys = append(cmdKeys, "batchSize")
	}
	if single {
		cmd["singleBatch"] = true
		cmdKeys = append(cmdKeys, "singleBatch")
	}
	reply := s.runCommand(ctx, cmd, cmdKeys)
	cur, ok := reply["cursor"].(db.Document)
	if !ok {
		return writeOpReply(w, m.requestID, ctx.nextResponseID(), 0, 0, []db.Document{reply})
	}
	batch, _ := cur["firstBatch"].([]any)
	docs := make([]db.Document, 0, len(batch))
	for _, d := range batch {
		if doc, ok := d.(db.Document); ok {
			docs = append(docs, doc)
		}
	}
	id, _ := toInt64(cur["id"])
	return writeOpReply(w, m.requestID, ctx.nextResponseID(), id, int32(len(docs)), docs)
}

type wireError struct {
	code int32
	name string
	msg  string
}

func (e *wireError) Error() string { return e.msg }

func errWire(code int32, name, msg string) *wireError {
	return &wireError{code: code, name: name, msg: msg}
}

func cmdErrorReply(err error) db.Document {
	var we *wireError
	if errors.As(err, &we) {
		return db.Document{
			"ok":       float64(0),
			"errmsg":   we.msg,
			"code":     we.code,
			"codeName": we.name,
		}
	}
	switch {
	case errors.Is(err, db.ErrDuplicate):
		return db.Document{"ok": float64(0), "errmsg": err.Error(), "code": int32(11000), "codeName": "DuplicateKey"}
	case errors.Is(err, db.ErrUnauthorized):
		return db.Document{"ok": float64(0), "errmsg": "not authorized", "code": int32(13), "codeName": "Unauthorized"}
	case errors.Is(err, db.ErrForbidden):
		return db.Document{"ok": float64(0), "errmsg": "not authorized", "code": int32(13), "codeName": "Unauthorized"}
	case errors.Is(err, db.ErrTooLarge):
		return db.Document{"ok": float64(0), "errmsg": err.Error(), "code": int32(10334), "codeName": "BSONObjectTooLarge"}
	case errors.Is(err, db.ErrBadFilter):
		return db.Document{"ok": float64(0), "errmsg": err.Error(), "code": int32(2), "codeName": "BadValue"}
	case errors.Is(err, db.ErrNoID):
		return db.Document{"ok": float64(0), "errmsg": err.Error(), "code": int32(2), "codeName": "BadValue"}
	case errors.Is(err, db.ErrNotFound):
		return db.Document{"ok": float64(0), "errmsg": "no such collection or document", "code": int32(26), "codeName": "NamespaceNotFound"}
	case errors.Is(err, db.ErrExists):
		return db.Document{"ok": float64(0), "errmsg": "collection already exists", "code": int32(48), "codeName": "NamespaceExists"}
	}
	return db.Document{"ok": float64(0), "errmsg": err.Error(), "code": int32(1), "codeName": "InternalError"}
}

func okReply(fields ...any) db.Document {
	doc := db.Document{}
	for i := 0; i+1 < len(fields); i += 2 {
		doc[fields[i].(string)] = fields[i+1]
	}
	doc["ok"] = float64(1)
	return doc
}

var preAuthCommands = map[string]bool{
	"hello":       true,
	"isMaster":    true,
	"ismaster":    true,
	"ping":        true,
	"saslStart":   true,
	"saslContinue": true,
	"buildInfo":   true,
	"buildinfo":   true,
	"getLog":      true,
	"whatsmyuri":  true,
	"logout":      true,
	"isdbgrid":    true,
}

var commandNames = map[string]bool{
	"hello": true, "isMaster": true, "ismaster": true, "ping": true,
	"buildInfo": true, "buildinfo": true, "getLog": true, "connectionStatus": true,
	"whatsmyuri": true, "serverStatus": true, "startSession": true,
	"endSessions": true, "logout": true, "killAllSessions": true, "killSessions": true,
	"getParameter": true, "listDatabases": true, "listCollections": true,
	"create": true, "drop": true, "createIndexes": true, "listIndexes": true,
	"dropIndexes": true, "insert": true, "find": true, "getMore": true,
	"killCursors": true, "count": true, "countDocuments": true,
	"estimatedDocumentCount": true, "distinct": true, "update": true,
	"delete": true, "findAndModify": true, "findandmodify": true,
	"aggregate": true, "dbStats": true, "collStats": true,
	"saslStart": true, "saslContinue": true,
}

func resolveCommand(keys []string, cmd db.Document) (string, any) {
	for _, k := range keys {
		if strings.HasPrefix(k, "$") {
			continue
		}
		if commandNames[k] {
			return k, cmd[k]
		}
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "$") {
			return k, cmd[k]
		}
	}
	return "", nil
}

func (s *Server) runCommand(ctx *connCtx, cmd db.Document, keys []string) db.Document {
	name, arg := resolveCommand(keys, cmd)
	if name == "" {
		return cmdErrorReply(errWire(9, "FailedToParse", "no command name"))
	}
	if s.opts.AuthUser != "" && !ctx.authed && !preAuthCommands[name] {
		return db.Document{
			"ok":       float64(0),
			"errmsg":   "command " + name + " requires authentication",
			"code":     int32(13),
			"codeName": "Unauthorized",
		}
	}
	switch name {
	case "hello", "isMaster", "ismaster":
		return cmdHello(ctx, cmd)
	case "ping":
		return okReply()
	case "buildInfo", "buildinfo":
		return cmdBuildInfo()
	case "getLog":
		return okReply("totalLinesWritten", int32(0), "log", []any{})
	case "connectionStatus":
		return cmdConnectionStatus(ctx)
	case "whatsmyuri":
		return okReply("you", ctx.remote)
	case "serverStatus":
		return cmdServerStatus(ctx)
	case "startSession":
		return cmdStartSession()
	case "endSessions", "logout", "killAllSessions", "killSessions":
		return okReply()
	case "getParameter":
		return cmdGetParameter()
	case "listDatabases":
		return cmdListDatabases(ctx)
	case "listCollections":
		return cmdListCollections(ctx, cmd)
	case "create":
		return cmdCreate(ctx, arg)
	case "drop":
		return cmdDrop(ctx, arg)
	case "createIndexes":
		return cmdCreateIndexes(ctx, arg, cmd)
	case "listIndexes":
		return cmdListIndexes(ctx, arg)
	case "dropIndexes":
		return cmdDropIndexes(ctx, arg, cmd)
	case "insert":
		return cmdInsert(ctx, arg, cmd)
	case "find":
		return cmdFind(ctx, arg, cmd)
	case "getMore":
		return cmdGetMore(ctx, arg, cmd)
	case "killCursors":
		return cmdKillCursors(ctx, arg, cmd)
	case "count":
		return cmdCount(ctx, arg, cmd)
	case "countDocuments":
		return cmdCountDocuments(ctx, arg, cmd)
	case "estimatedDocumentCount":
		return cmdEstimatedDocumentCount(ctx, arg)
	case "distinct":
		return cmdDistinct(ctx, arg, cmd)
	case "update":
		return cmdUpdate(ctx, arg, cmd)
	case "delete":
		return cmdDelete(ctx, arg, cmd)
	case "findAndModify", "findandmodify":
		return cmdFindAndModify(ctx, arg, cmd)
	case "aggregate":
		return cmdAggregate(ctx, arg, cmd)
	case "dbStats":
		return cmdDbStats(ctx)
	case "collStats":
		return cmdCollStats(ctx, arg)
	case "saslStart":
		return s.cmdSaslStart(ctx, arg, cmd)
	case "saslContinue":
		return s.cmdSaslContinue(ctx, arg, cmd)
	}
	return cmdErrorReply(errWire(59, "CommandNotFound", "no such command: '"+name+"'"))
}
