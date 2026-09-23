package wire

import (
	"strings"

	"mlstoredb/db"
)

const defaultBatchSize = 101
const maxBatchBytes = 16 * 1024 * 1024

func cmdInsert(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	docs, ok := cmd["documents"].([]any)
	if !ok || len(docs) == 0 {
		return cmdErrorReply(errWire(16, "InvalidLength", "insert requires a non-empty documents array"))
	}
	if len(docs) > maxWriteBatchSize {
		return cmdErrorReply(errWire(16, "InvalidLength", "write batch exceeds maxWriteBatchSize"))
	}
	ordered := true
	if o, ok := cmd["ordered"].(bool); ok {
		ordered = o
	}
	n := 0
	var writeErrors []any
	for i, d := range docs {
		doc, ok := d.(db.Document)
		if !ok {
			writeErrors = append(writeErrors, writeError(i, 14, "TypeMismatch", "document must be an object"))
			if ordered {
				break
			}
			continue
		}
		if err := normalizeDocID(doc); err != nil {
			writeErrors = append(writeErrors, writeError(i, 2, "BadValue", err.Error()))
			if ordered {
				break
			}
			continue
		}
		if err := ctx.insert(coll, doc); err != nil {
			we := writeErrorFromEngine(i, err)
			writeErrors = append(writeErrors, we)
			if ordered {
				break
			}
			continue
		}
		n++
	}
	reply := db.Document{"n": int32(n), "ok": float64(1)}
	if writeErrors != nil {
		reply["writeErrors"] = writeErrors
	}
	return reply
}

func writeError(index int, code int32, name, msg string) db.Document {
	return db.Document{"index": int32(index), "code": code, "errmsg": name + ": " + msg}
}

func writeErrorFromEngine(index int, err error) db.Document {
	reply := cmdErrorReply(err)
	return db.Document{
		"index":  int32(index),
		"code":   reply["code"],
		"errmsg": reply["errmsg"],
	}
}

type projectionMode struct {
	include []string
	exclude []string
	dropID  bool
}

func parseProjection(v any) (*projectionMode, error) {
	if v == nil {
		return nil, nil
	}
	p, ok := v.(db.Document)
	if !ok {
		return nil, errWire(9, "FailedToParse", "projection must be a document")
	}
	if len(p) == 0 {
		return nil, nil
	}
	mode := &projectionMode{}
	hasInc, hasExc := false, false
	for k, val := range p {
		if strings.Contains(k, ".") {
			return nil, errWire(9, "FailedToParse", "dotted projections are not supported: "+k)
		}
		on := false
		switch t := val.(type) {
		case bool:
			on = t
		case float64:
			on = t != 0
		case int32:
			on = t != 0
		default:
			return nil, errWire(9, "FailedToParse", "projection values must be 0/1")
		}
		if k == "_id" {
			if !on {
				mode.dropID = true
			}
			continue
		}
		if on {
			hasInc = true
			mode.include = append(mode.include, k)
		} else {
			hasExc = true
			mode.exclude = append(mode.exclude, k)
		}
	}
	if hasInc && hasExc {
		return nil, errWire(31254, "Location31254", "cannot mix include and exclude projections")
	}
	return mode, nil
}

func (pm *projectionMode) apply(docs []db.Document) []db.Document {
	if pm == nil {
		return docs
	}
	out := make([]db.Document, 0, len(docs))
	for _, d := range docs {
		nd := db.Document{}
		if pm.include != nil {
			for _, f := range pm.include {
				if v, ok := d[f]; ok {
					nd[f] = v
				}
			}
			if !pm.dropID {
				if id, ok := d["_id"]; ok {
					nd["_id"] = id
				}
			}
		} else {
			for k, v := range d {
				if k == "_id" && pm.dropID {
					continue
				}
				dropped := false
				for _, f := range pm.exclude {
					if f == k {
						dropped = true
						break
					}
				}
				if !dropped {
					nd[k] = v
				}
			}
		}
		out = append(out, nd)
	}
	return out
}

func parseSort(v any) (map[string]int, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(db.Document)
	if !ok {
		return nil, errWire(9, "FailedToParse", "sort must be a document")
	}
	if len(s) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(s))
	for k, val := range s {
		dir := 0
		switch t := val.(type) {
		case float64:
			dir = int(t)
		case int32:
			dir = int(t)
		default:
			return nil, errWire(9, "FailedToParse", "sort direction must be 1 or -1")
		}
		if dir != 1 && dir != -1 {
			return nil, errWire(9, "FailedToParse", "sort direction must be 1 or -1")
		}
		out[k] = dir
	}
	return out, nil
}

func intField(cmd db.Document, key string) int {
	if v, ok := cmd[key]; ok {
		if n, ok := toInt64(v); ok {
			return int(n)
		}
	}
	return 0
}

func boolField(cmd db.Document, key string, def bool) bool {
	if v, ok := cmd[key].(bool); ok {
		return v
	}
	return def
}

func cursorReply(ctx *connCtx, coll string, docs []db.Document, batchSize int, singleBatch bool) db.Document {
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	batch := docs
	rest := []db.Document{}
	if len(docs) > batchSize && !singleBatch {
		batch = docs[:batchSize]
		rest = docs[batchSize:]
	}
	sizes := make([]int, len(batch))
	total := 0
	for i, d := range batch {
		b, err := encodeBSON(d)
		if err != nil {
			return cmdErrorReply(err)
		}
		sizes[i] = len(b)
		total += sizes[i]
	}
	for len(batch) > 1 && total > maxBatchBytes {
		total -= sizes[len(batch)-1]
		rest = append([]db.Document{batch[len(batch)-1]}, rest...)
		batch = batch[:len(batch)-1]
		sizes = sizes[:len(sizes)-1]
	}
	var id int64
	if len(rest) > 0 {
		id = ctx.srv.cursors.register(rest, ctx.srv.opts.DBName+"."+coll)
	}
	items := make([]any, len(batch))
	for i, d := range batch {
		items[i] = d
	}
	return okReply("cursor", db.Document{
		"firstBatch": items,
		"id":         id,
		"ns":         ctx.srv.opts.DBName + "." + coll,
	})
}

func cmdFind(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	filter, err := translateFilter(cmd["filter"])
	if err != nil {
		return cmdErrorReply(err)
	}
	sortSpec, err := parseSort(cmd["sort"])
	if err != nil {
		return cmdErrorReply(err)
	}
	proj, err := parseProjection(cmd["projection"])
	if err != nil {
		return cmdErrorReply(err)
	}
	skip := intField(cmd, "skip")
	limit := intField(cmd, "limit")
	singleBatch := boolField(cmd, "singleBatch", false)
	if limit < 0 {
		singleBatch = true
		limit = -limit
	}
	batchSize := intField(cmd, "batchSize")
	if batchSize < 0 {
		batchSize = -batchSize
	}
	opts := &db.FindOptions{Sort: sortSpec, Limit: limit, Skip: skip}
	if proj != nil && proj.include != nil {
		opts.Projection = proj.include
	}
	docs, err := ctx.find(coll, filter, opts)
	if err != nil {
		return cmdErrorReply(err)
	}
	if proj != nil {
		docs = proj.apply(docs)
	}
	return cursorReply(ctx, coll, docs, batchSize, singleBatch)
}

func cmdGetMore(ctx *connCtx, arg any, cmd db.Document) db.Document {
	id, ok := toInt64(arg)
	if !ok {
		return cmdErrorReply(errWire(9, "FailedToParse", "getMore requires a cursor id"))
	}
	batchSize := intField(cmd, "batchSize")
	docs, ns, next, found := ctx.srv.cursors.batch(id, batchSize)
	if !found {
		return cmdErrorReply(errWire(43, "CursorNotFound", "cursor id "+formatIDInt64(id)+" not found"))
	}
	items := make([]any, len(docs))
	for i, d := range docs {
		items[i] = d
	}
	return okReply("cursor", db.Document{
		"nextBatch": items,
		"id":        next,
		"ns":        ns,
	})
}

func formatIDInt64(v int64) string {
	return formatIDNumber(float64(v))
}

func cmdKillCursors(ctx *connCtx, arg any, cmd db.Document) db.Document {
	ids, ok := cmd["cursors"].([]any)
	if !ok {
		return cmdErrorReply(errWire(9, "FailedToParse", "killCursors requires a cursors array"))
	}
	var idList []int64
	for _, v := range ids {
		if n, ok := toInt64(v); ok {
			idList = append(idList, n)
		}
	}
	killed, notFound := ctx.srv.cursors.kill(idList)
	mk := func(ids []int64) []any {
		out := make([]any, len(ids))
		for i, id := range ids {
			out[i] = id
		}
		return out
	}
	return okReply(
		"cursorsKilled", mk(killed),
		"cursorsNotFound", mk(notFound),
		"cursorsAlive", []any{},
		"cursorsUnknown", []any{},
	)
}

func cmdCount(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	filter, err := translateFilter(cmd["query"])
	if err != nil {
		return cmdErrorReply(err)
	}
	n, err := ctx.count(coll, filter)
	if err != nil {
		return cmdErrorReply(err)
	}
	return okReply("n", int32(n))
}

func cmdCountDocuments(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	filter, err := translateFilter(cmd["filter"])
	if err != nil {
		return cmdErrorReply(err)
	}
	n, err := ctx.count(coll, filter)
	if err != nil {
		return cmdErrorReply(err)
	}
	return okReply("n", int32(n))
}

func cmdEstimatedDocumentCount(ctx *connCtx, arg any) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	n, err := ctx.count(coll, nil)
	if err != nil {
		return cmdErrorReply(err)
	}
	return okReply("n", int32(n))
}

func cmdDistinct(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	key, ok := cmd["key"].(string)
	if !ok || key == "" {
		return cmdErrorReply(errWire(9, "FailedToParse", "distinct requires a key"))
	}
	filter, err := translateFilter(cmd["query"])
	if err != nil {
		return cmdErrorReply(err)
	}
	docs, err := ctx.find(coll, filter, nil)
	if err != nil {
		return cmdErrorReply(err)
	}
	seen := map[string]bool{}
	var values []any
	for _, d := range docs {
		v, ok := getPath(d, key)
		if !ok {
			continue
		}
		candidates := []any{v}
		if arr, ok := v.([]any); ok {
			candidates = arr
		}
		for _, c := range candidates {
			k := distinctKey(c)
			if !seen[k] {
				seen[k] = true
				values = append(values, c)
			}
		}
	}
	if values == nil {
		values = []any{}
	}
	return okReply("values", values)
}

func distinctKey(v any) string {
	switch t := v.(type) {
	case string:
		return "s:" + t
	case float64:
		return "n:" + formatIDNumber(t)
	case bool:
		if t {
			return "b:1"
		}
		return "b:0"
	case nil:
		return "z"
	}
	return "o:" + formatIDNumber(float64(len(formatSlice(v))))
}

func formatSlice(v any) string {
	if arr, ok := v.([]any); ok {
		return strings.Join(make([]string, len(arr)), ",")
	}
	return ""
}

func cmdUpdate(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	updates, ok := cmd["updates"].([]any)
	if !ok || len(updates) == 0 {
		return cmdErrorReply(errWire(9, "FailedToParse", "update requires a non-empty updates array"))
	}
	ordered := boolField(cmd, "ordered", true)
	n := 0
	nModified := 0
	var upserted []any
	var writeErrors []any
	for i, u := range updates {
		spec, ok := u.(db.Document)
		if !ok {
			writeErrors = append(writeErrors, writeError(i, 9, "FailedToParse", "update must be a document"))
			if ordered {
				break
			}
			continue
		}
		filter, err := translateFilter(spec["q"])
		if err != nil {
			writeErrors = append(writeErrors, writeError(i, 2, "BadValue", err.Error()))
			if ordered {
				break
			}
			continue
		}
		upsert := boolField(spec, "upsert", false)
		multi := boolField(spec, "multi", false)
		matches, err := ctx.find(coll, filter, nil)
		if err != nil {
			writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
			if ordered {
				break
			}
			continue
		}
		if len(matches) == 0 && upsert {
			base := equalityFields(filter)
			if err := normalizeDocID(base); err != nil {
				writeErrors = append(writeErrors, writeError(i, 2, "BadValue", err.Error()))
				if ordered {
					break
				}
				continue
			}
			fresh, err := applyUpdateExpr(base, spec["u"], true)
			if err != nil {
				writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
				if ordered {
					break
				}
				continue
			}
			if err := normalizeDocID(fresh); err != nil {
				writeErrors = append(writeErrors, writeError(i, 2, "BadValue", err.Error()))
				if ordered {
					break
				}
				continue
			}
			if err := ctx.insert(coll, fresh); err != nil {
				writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
				if ordered {
					break
				}
				continue
			}
			n++
			upserted = append(upserted, db.Document{"index": int32(i), "_id": fresh["_id"]})
			continue
		}
		if !multi && len(matches) > 1 {
			matches = matches[:1]
		}
		for _, doc := range matches {
			id, _ := doc["_id"].(string)
			fresh, err := applyUpdateExpr(doc, spec["u"], false)
			if err != nil {
				writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
				break
			}
			patch, remove := diffTopLevel(doc, fresh)
			if len(patch) == 0 && len(remove) == 0 {
				n++
				continue
			}
			if err := ctx.updateFields(coll, id, patch, remove); err != nil {
				writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
				break
			}
			n++
			nModified++
		}
		if len(writeErrors) > 0 && ordered {
			break
		}
	}
	reply := db.Document{"n": int32(n), "nModified": int32(nModified), "ok": float64(1)}
	if upserted != nil {
		reply["upserted"] = upserted
	}
	if writeErrors != nil {
		reply["writeErrors"] = writeErrors
	}
	return reply
}

func cmdDelete(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	deletes, ok := cmd["deletes"].([]any)
	if !ok || len(deletes) == 0 {
		return cmdErrorReply(errWire(9, "FailedToParse", "delete requires a non-empty deletes array"))
	}
	ordered := boolField(cmd, "ordered", true)
	n := 0
	var writeErrors []any
	for i, d := range deletes {
		spec, ok := d.(db.Document)
		if !ok {
			writeErrors = append(writeErrors, writeError(i, 9, "FailedToParse", "delete must be a document"))
			if ordered {
				break
			}
			continue
		}
		filter, err := translateFilter(spec["q"])
		if err != nil {
			writeErrors = append(writeErrors, writeError(i, 2, "BadValue", err.Error()))
			if ordered {
				break
			}
			continue
		}
		limit := intField(spec, "limit")
		matches, err := ctx.find(coll, filter, nil)
		if err != nil {
			writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
			if ordered {
				break
			}
			continue
		}
		if limit == 1 && len(matches) > 1 {
			matches = matches[:1]
		}
		for _, doc := range matches {
			id, _ := doc["_id"].(string)
			if err := ctx.delete(coll, id); err != nil {
				writeErrors = append(writeErrors, writeErrorFromEngine(i, err))
				break
			}
			n++
		}
		if len(writeErrors) > 0 && ordered {
			break
		}
	}
	reply := db.Document{"n": int32(n), "ok": float64(1)}
	if writeErrors != nil {
		reply["writeErrors"] = writeErrors
	}
	return reply
}

func cmdFindAndModify(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, ok := arg.(string)
	if !ok || coll == "" {
		return cmdErrorReply(errWire(73, "InvalidNamespace", "invalid collection name"))
	}
	filter, err := translateFilter(cmd["query"])
	if err != nil {
		return cmdErrorReply(err)
	}
	sortSpec, err := parseSort(cmd["sort"])
	if err != nil {
		return cmdErrorReply(err)
	}
	proj, err := parseProjection(cmd["fields"])
	if err != nil {
		return cmdErrorReply(err)
	}
	remove := boolField(cmd, "remove", false)
	returnNew := boolField(cmd, "new", false)
	upsert := boolField(cmd, "upsert", false)
	opts := &db.FindOptions{Sort: sortSpec, Limit: 1}
	matches, err := ctx.find(coll, filter, opts)
	if err != nil {
		return cmdErrorReply(err)
	}
	if len(matches) == 0 {
		if !upsert || remove {
			return okReply(
				"value", nil,
				"lastErrorObject", db.Document{"n": int32(0), "updatedExisting": false},
			)
		}
		base := equalityFields(filter)
		if err := normalizeDocID(base); err != nil {
			return cmdErrorReply(err)
		}
		fresh, err := applyUpdateExpr(base, cmd["update"], true)
		if err != nil {
			return cmdErrorReply(err)
		}
		if err := normalizeDocID(fresh); err != nil {
			return cmdErrorReply(err)
		}
		if err := ctx.insert(coll, fresh); err != nil {
			return cmdErrorReply(err)
		}
		var value any
		if returnNew {
			value = fresh
		}
		return okReply(
			"value", value,
			"lastErrorObject", db.Document{"n": int32(1), "updatedExisting": false, "upserted": fresh["_id"]},
		)
	}
	doc := matches[0]
	id, _ := doc["_id"].(string)
	if remove {
		if err := ctx.delete(coll, id); err != nil {
			return cmdErrorReply(err)
		}
		var value any = doc
		if proj != nil {
			value = proj.apply([]db.Document{doc})[0]
		}
		return okReply(
			"value", value,
			"lastErrorObject", db.Document{"n": int32(1), "updatedExisting": false},
		)
	}
	fresh, err := applyUpdateExpr(doc, cmd["update"], false)
	if err != nil {
		return cmdErrorReply(err)
	}
	patch, removeFields := diffTopLevel(doc, fresh)
	if len(patch) > 0 || len(removeFields) > 0 {
		if err := ctx.updateFields(coll, id, patch, removeFields); err != nil {
			return cmdErrorReply(err)
		}
	} else {
		fresh = doc
	}
	var value any = doc
	if returnNew {
		value = fresh
	}
	if proj != nil {
		value = proj.apply([]db.Document{value.(db.Document)})[0]
	}
	return okReply(
		"value", value,
		"lastErrorObject", db.Document{"n": int32(1), "updatedExisting": true},
	)
}
