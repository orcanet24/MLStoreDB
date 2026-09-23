package wire

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"mlstoredb/db"
)

func cmdHello(ctx *connCtx, cmd db.Document) db.Document {
	reply := db.Document{
		"isWritablePrimary":           true,
		"ismaster":                    true,
		"helloOk":                     true,
		"maxBsonObjectSize":           int32(1 << 20),
		"maxMessageSizeBytes":         int32(48000000),
		"maxWriteBatchSize":           int32(maxWriteBatchSize),
		"localTime":                   db.Document{"$date": float64(time.Now().UnixMilli())},
		"logicalSessionTimeoutMinutes": int32(30),
		"connectionId":                int32(ctx.id),
		"minWireVersion":              int32(0),
		"maxWireVersion":              int32(17),
		"readOnly":                    false,
	}
	if ctx.srv.opts.AuthUser != "" {
		reply["saslSupportedMechs"] = []any{"SCRAM-SHA-256"}
	}
	reply["ok"] = float64(1)
	return reply
}

func cmdBuildInfo() db.Document {
	return db.Document{
		"version":           "7.0.0-mlstoredb",
		"gitVersion":        "mlstoredb",
		"versionArray":      []any{int32(7), int32(0), int32(0), int32(0)},
		"javascriptEngine":  "none",
		"bits":              int32(64),
		"debug":             false,
		"maxBsonObjectSize": int32(1 << 20),
		"ok":                float64(1),
	}
}

func cmdConnectionStatus(ctx *connCtx) db.Document {
	users := []any{}
	roles := []any{}
	if ctx.session != nil {
		users = append(users, db.Document{"user": ctx.session.User(), "db": ctx.srv.opts.DBName})
		for _, r := range ctx.session.Roles() {
			roles = append(roles, db.Document{"role": r, "db": ctx.srv.opts.DBName})
		}
	}
	return okReply("authInfo", db.Document{
		"authenticatedUsers":      users,
		"authenticatedUserRoles":  roles,
	})
}

func cmdServerStatus(ctx *connCtx) db.Document {
	uptime := time.Since(ctx.srv.started).Seconds()
	return okReply(
		"host", "mlstoredb",
		"version", "7.0.0-mlstoredb",
		"process", "mlstoredb",
		"pid", int32(os.Getpid()),
		"uptime", float64(uptime),
		"uptimeMillis", int64(uptime*1000),
		"localTime", db.Document{"$date": float64(time.Now().UnixMilli())},
		"connections", db.Document{"current": int32(1), "available": int32(999), "totalCreated": int32(ctx.srv.connSeq.Load())},
	)
}

func cmdStartSession() db.Document {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return cmdErrorReply(errWire(1, "InternalError", "session id generation failed"))
	}
	raw[6] = (raw[6] & 0x0F) | 0x40
	raw[8] = (raw[8] & 0x3F) | 0x80
	return okReply(
		"id", db.Document{"id": db.Document{
			"$binary": db.Document{"base64": base64.StdEncoding.EncodeToString(raw[:]), "subType": "04"},
		}},
		"timeoutMinutes", int32(30),
	)
}

func cmdGetParameter() db.Document {
	return okReply("featureCompatibilityVersion", db.Document{"version": "7.0"})
}

func cmdListDatabases(ctx *connCtx) db.Document {
	colls := ctx.collections()
	empty := len(colls) == 0
	dbs := []any{db.Document{
		"name":       ctx.srv.opts.DBName,
		"sizeOnDisk": int64(0),
		"empty":      empty,
	}}
	return okReply("databases", dbs, "totalSize", int64(0))
}

func cmdListCollections(ctx *connCtx, cmd db.Document) db.Document {
	colls := ctx.collections()
	var nameFilter db.Document
	if f, ok := cmd["filter"].(db.Document); ok {
		if nf, ok := f["name"].(db.Document); ok {
			nameFilter = nf
		} else if nv, ok := f["name"]; ok && nf == nil {
			nameFilter = db.Document{"$eq": nv}
		}
	}
	batch := []any{}
	for _, c := range colls {
		if nameFilter != nil && !matchNameFilter(c, nameFilter) {
			continue
		}
		batch = append(batch, db.Document{
			"name":    c,
			"type":    "collection",
			"options": db.Document{},
			"info":    db.Document{"readOnly": false},
			"idIndex": db.Document{"v": int32(2), "key": db.Document{"_id": int32(1)}, "name": "_id_"},
		})
	}
	return okReply("cursor", db.Document{
		"firstBatch": batch,
		"id":         int64(0),
		"ns":         ctx.srv.opts.DBName + ".$cmd.listCollections",
	})
}

func matchNameFilter(name string, filter db.Document) bool {
	for k, v := range filter {
		switch k {
		case "$eq":
			if s, ok := v.(string); !ok || s != name {
				return false
			}
		case "$regex":
			p, _ := v.(string)
			opts := ""
			if re, ok := filter["$options"].(db.Document); ok {
				opts, _ = re["options"].(string)
			}
			_ = opts
			if !regexMatch(p, name) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func regexMatch(pattern, s string) bool {
	re, err := compileRegex(pattern, "")
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func cmdCreate(ctx *connCtx, arg any) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	if err := ctx.createCollection(coll); err != nil {
		return cmdErrorReply(err)
	}
	return okReply()
}

func cmdDrop(ctx *connCtx, arg any) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	if err := ctx.dropCollection(coll); err != nil {
		return cmdErrorReply(err)
	}
	return okReply("ns", ctx.srv.opts.DBName+"."+coll)
}

func cmdCreateIndexes(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	specs, ok := cmd["indexes"].([]any)
	if !ok || len(specs) == 0 {
		return cmdErrorReply(errWire(9, "FailedToParse", "indexes must be a non-empty array"))
	}
	before := 0
	if existing, err := ctx.listIndexes(coll); err == nil {
		before = len(existing)
	}
	created := 0
	for _, sp := range specs {
		spec, ok := sp.(db.Document)
		if !ok {
			return cmdErrorReply(errWire(9, "FailedToParse", "invalid index spec"))
		}
		keyDoc, ok := spec["key"].(db.Document)
		if !ok || len(keyDoc) == 0 {
			return cmdErrorReply(errWire(9, "FailedToParse", "index spec requires key"))
		}
		fields := sortedFields(keyDoc)
		unique := false
		if u, ok := spec["unique"].(bool); ok {
			unique = u
		}
		if err := ctx.ensureIndex(coll, fields, unique); err != nil {
			return cmdErrorReply(err)
		}
		created++
	}
	after, err := ctx.listIndexes(coll)
	if err != nil {
		return cmdErrorReply(err)
	}
	return okReply(
		"numIndexesBefore", int32(before),
		"numIndexesAfter", int32(len(after)),
		"createdCollectionAutomatically", before == 0,
		"created", int32(created),
	)
}

func sortedFields(keyDoc db.Document) []string {
	fields := make([]string, 0, len(keyDoc))
	for f := range keyDoc {
		fields = append(fields, f)
	}
	sortStrings(fields)
	return fields
}

func cmdListIndexes(ctx *connCtx, arg any) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	idxs, err := ctx.listIndexes(coll)
	if err != nil {
		return cmdErrorReply(err)
	}
	batch := []any{}
	for _, idx := range idxs {
		key := db.Document{}
		for _, f := range idx.Fields {
			key[f] = int32(1)
		}
		entry := db.Document{"v": int32(2), "key": key, "name": indexName(idx)}
		if idx.Unique {
			entry["unique"] = true
		}
		batch = append(batch, entry)
	}
	return okReply("cursor", db.Document{
		"firstBatch": batch,
		"id":         int64(0),
		"ns":         ctx.srv.opts.DBName + "." + coll,
	})
}

func indexName(idx db.IndexInfo) string {
	return strings.Join(idx.Fields, "_") + "_1"
}

func cmdDropIndexes(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	idxs, err := ctx.listIndexes(coll)
	if err != nil {
		return cmdErrorReply(err)
	}
	target, ok := cmd["index"]
	if !ok {
		return cmdErrorReply(errWire(9, "FailedToParse", "index required"))
	}
	dropped := 0
	switch t := target.(type) {
	case string:
		if t == "*" {
			for _, idx := range idxs {
				if err := ctx.dropIndex(coll, idx.Fields); err != nil {
					return cmdErrorReply(err)
				}
				dropped++
			}
			break
		}
		found := false
		for _, idx := range idxs {
			if indexName(idx) == t {
				if err := ctx.dropIndex(coll, idx.Fields); err != nil {
					return cmdErrorReply(err)
				}
				dropped++
				found = true
				break
			}
		}
		if !found {
			return cmdErrorReply(errWire(27, "IndexNotFound", "index not found: "+t))
		}
	case db.Document:
		fields := sortedFields(t)
		found := false
		for _, idx := range idxs {
			if sameStrings(idx.Fields, fields) {
				if err := ctx.dropIndex(coll, idx.Fields); err != nil {
					return cmdErrorReply(err)
				}
				dropped++
				found = true
				break
			}
		}
		if !found {
			return cmdErrorReply(errWire(27, "IndexNotFound", "index not found"))
		}
	default:
		return cmdErrorReply(errWire(9, "FailedToParse", "invalid index specifier"))
	}
	return okReply("nIndexesWas", int32(len(idxs)), "nIndexes", int32(len(idxs)-dropped))
}

func cmdDbStats(ctx *connCtx) db.Document {
	colls := ctx.collections()
	objects := 0
	indexes := 0
	for _, c := range colls {
		if n, err := ctx.count(c, nil); err == nil {
			objects += n
		}
		if idxs, err := ctx.listIndexes(c); err == nil {
			indexes += len(idxs)
		}
	}
	var avg float64
	if objects > 0 {
		avg = 0
	}
	return okReply(
		"db", ctx.srv.opts.DBName,
		"collections", int32(len(colls)),
		"views", int32(0),
		"objects", int32(objects),
		"avgObjSize", avg,
		"dataSize", int64(0),
		"storageSize", int64(0),
		"indexes", int32(indexes),
		"indexSize", int64(0),
		"totalSize", int64(0),
	)
}

func cmdCollStats(ctx *connCtx, arg any) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	n, err := ctx.count(coll, nil)
	if err != nil {
		return cmdErrorReply(err)
	}
	idxs, _ := ctx.listIndexes(coll)
	return okReply(
		"ns", ctx.srv.opts.DBName+"."+coll,
		"count", int32(n),
		"size", int64(0),
		"avgObjSize", int64(0),
		"storageSize", int64(0),
		"nindexes", int32(len(idxs)),
		"totalIndexSize", int64(0),
	)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newObjectIDHex() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return hex.EncodeToString(make([]byte, 12))
	}
	return hex.EncodeToString(raw[:])
}
