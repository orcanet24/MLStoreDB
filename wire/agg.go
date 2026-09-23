package wire

import (
	"fmt"
	"sort"
	"strings"

	"mlstoredb/db"
)

func cmdAggregate(ctx *connCtx, arg any, cmd db.Document) db.Document {
	coll, _ := arg.(string)
	if coll == "" {
		return cmdErrorReply(errWire(9, "FailedToParse", "aggregate requires a collection"))
	}
	pipeline, ok := cmd["pipeline"].([]any)
	if !ok {
		return cmdErrorReply(errWire(9, "FailedToParse", "aggregate requires a pipeline"))
	}
	docs, err := ctx.find(coll, nil, nil)
	if err != nil {
		return cmdErrorReply(err)
	}
	for _, st := range pipeline {
		stage, ok := st.(db.Document)
		if !ok || len(stage) != 1 {
			return cmdErrorReply(errWire(40323, "Location40323", "pipeline stages must be single-key documents"))
		}
		var name string
		var spec any
		for k, v := range stage {
			name = k
			spec = v
		}
		switch name {
		case "$match":
			filter, err := translateFilter(spec)
			if err != nil {
				return cmdErrorReply(err)
			}
			docs, err = ctx.find(coll, filter, nil)
			if err != nil {
				return cmdErrorReply(err)
			}
		case "$sort":
			sortSpec, err := parseSort(spec)
			if err != nil {
				return cmdErrorReply(err)
			}
			docs = sortDocsServer(docs, sortSpec)
		case "$skip":
			n, ok := toInt64(spec)
			if !ok || n < 0 {
				return cmdErrorReply(errWire(15956, "Location15956", "$skip requires a non-negative number"))
			}
			if int(n) >= len(docs) {
				docs = nil
			} else {
				docs = docs[n:]
			}
		case "$limit":
			n, ok := toInt64(spec)
			if !ok || n <= 0 {
				return cmdErrorReply(errWire(15958, "Location15958", "$limit requires a positive number"))
			}
			if int(n) < len(docs) {
				docs = docs[:n]
			}
		case "$count":
			field, ok := spec.(string)
			if !ok || field == "" || strings.HasPrefix(field, "$") {
				return cmdErrorReply(errWire(40156, "Location40156", "invalid $count field name"))
			}
			docs = []db.Document{{field: float64(len(docs))}}
		case "$group":
			out, err := groupDocs(docs, spec)
			if err != nil {
				return cmdErrorReply(err)
			}
			docs = out
		case "$unwind":
			out, err := unwindDocs(docs, spec)
			if err != nil {
				return cmdErrorReply(err)
			}
			docs = out
		case "$project":
			proj, err := parseProjection(spec)
			if err != nil {
				return cmdErrorReply(err)
			}
			if proj == nil {
				continue
			}
			docs = proj.apply(docs)
		case "$lookup":
			out, err := lookupDocs(ctx, docs, spec)
			if err != nil {
				return cmdErrorReply(err)
			}
			docs = out
		default:
			return cmdErrorReply(errWire(40324, "Location40324", "Unrecognized pipeline stage name: '"+name+"'"))
		}
	}
	return cursorReply(ctx, coll, docs, defaultBatchSize, false)
}

func sortDocsServer(docs []db.Document, spec map[string]int) []db.Document {
	fields := make([]string, 0, len(spec))
	for f := range spec {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	out := append([]db.Document{}, docs...)
	sort.SliceStable(out, func(i, j int) bool {
		for _, f := range fields {
			a, aok := getPath(out[i], f)
			b, bok := getPath(out[j], f)
			if !aok && !bok {
				continue
			}
			if !aok {
				return false
			}
			if !bok {
				return true
			}
			c := compareValues(a, b)
			if c == 0 {
				continue
			}
			return (c < 0) == (spec[f] == 1)
		}
		return false
	})
	return out
}

func resolveExpr(doc db.Document, expr any) (any, bool) {
	switch e := expr.(type) {
	case string:
		if strings.HasPrefix(e, "$") && len(e) > 1 {
			return getPath(doc, e[1:])
		}
		return e, true
	case db.Document:
		if len(e) == 1 {
			if lit, ok := e["$literal"]; ok {
				return lit, true
			}
		}
		return e, true
	}
	return expr, true
}

func groupKey(v any) string {
	switch t := v.(type) {
	case nil:
		return "z"
	case string:
		return "s:" + t
	case float64:
		return "n:" + formatIDNumber(t)
	case bool:
		if t {
			return "b:1"
		}
		return "b:0"
	}
	b, err := encodeBSON(db.Document{"v": v})
	if err != nil {
		return fmt.Sprintf("o:%v", v)
	}
	return "o:" + string(b)
}

func groupDocs(docs []db.Document, spec any) ([]db.Document, error) {
	g, ok := spec.(db.Document)
	if !ok {
		return nil, errWire(15947, "Location15947", "$group requires a document")
	}
	idExpr, hasID := g["_id"]
	if !hasID {
		return nil, errWire(15955, "Location15955", "$group requires _id")
	}
	type accSpec struct {
		field string
		op    string
		expr  any
	}
	accs := map[string]*accSpec{}
	var accOrder []string
	for k, v := range g {
		if k == "_id" {
			continue
		}
		if strings.HasPrefix(k, "$") {
			return nil, errWire(16410, "Location16410", "field names cannot start with $")
		}
		vd, ok := v.(db.Document)
		if !ok || len(vd) != 1 {
			return nil, errWire(40234, "Location40234", "accumulator must be a single-operator object")
		}
		var op string
		var expr any
		for kk, vv := range vd {
			op = kk
			expr = vv
		}
		switch op {
		case "$sum", "$avg", "$min", "$max", "$count", "$first", "$last", "$push", "$addToSet":
		default:
			return nil, errWire(15952, "Location15952", "unknown accumulator: "+op)
		}
		accs[k] = &accSpec{field: k, op: op, expr: expr}
		accOrder = append(accOrder, k)
	}
	sort.Strings(accOrder)
	type group struct {
		key  string
		id   any
		docs []db.Document
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range docs {
		var idVal any
		if idExpr == nil {
			idVal = nil
		} else if s, ok := idExpr.(string); ok && strings.HasPrefix(s, "$") && len(s) > 1 {
			v, found := getPath(d, s[1:])
			if !found {
				idVal = nil
			} else {
				idVal = v
			}
		} else if m, ok := idExpr.(db.Document); ok {
			comp := db.Document{}
			for k, v := range m {
				if s, ok := v.(string); ok && strings.HasPrefix(s, "$") && len(s) > 1 {
					got, found := getPath(d, s[1:])
					if found {
						comp[k] = got
					} else {
						comp[k] = nil
					}
				} else {
					comp[k] = v
				}
			}
			idVal = comp
		} else {
			idVal = idExpr
		}
		key := groupKey(idVal)
		gr, ok := groups[key]
		if !ok {
			gr = &group{key: key, id: idVal}
			groups[key] = gr
			order = append(order, key)
		}
		gr.docs = append(gr.docs, d)
	}
	out := make([]db.Document, 0, len(groups))
	for _, key := range order {
		gr := groups[key]
		doc := db.Document{"_id": gr.id}
		for _, f := range accOrder {
			acc := accs[f]
			count := 0
			sum := 0.0
			var minV, maxV, firstV, lastV any
			var items []any
			for _, d := range gr.docs {
				v, _ := resolveExpr(d, acc.expr)
				switch acc.op {
				case "$count":
					count++
				case "$sum":
					if n, ok := toFloat(v); ok {
						sum += n
					}
				case "$avg":
					if n, ok := toFloat(v); ok {
						sum += n
						count++
					}
				case "$min":
					if minV == nil || (v != nil && compareValues(minV, v) > 0) {
						minV = v
					}
				case "$max":
					if maxV == nil || (v != nil && compareValues(maxV, v) < 0) {
						maxV = v
					}
				case "$first":
					if count == 0 {
						firstV = v
					}
					count++
				case "$last":
					lastV = v
					count++
				case "$push":
					if v != nil {
						items = append(items, v)
					}
				case "$addToSet":
					if v != nil {
						found := false
						for _, e := range items {
							if deepEqual(e, v) {
								found = true
								break
							}
						}
						if !found {
							items = append(items, v)
						}
					}
				}
			}
			switch acc.op {
			case "$count":
				doc[f] = float64(count)
			case "$sum":
				doc[f] = sum
			case "$avg":
				if count > 0 {
					doc[f] = sum / float64(count)
				} else {
					doc[f] = nil
				}
			case "$min":
				doc[f] = minV
			case "$max":
				doc[f] = maxV
			case "$first":
				doc[f] = firstV
			case "$last":
				doc[f] = lastV
			case "$push", "$addToSet":
				if items == nil {
					doc[f] = []any{}
				} else {
					doc[f] = items
				}
			}
		}
		out = append(out, doc)
	}
	return out, nil
}

func unwindDocs(docs []db.Document, spec any) ([]db.Document, error) {
	path := ""
	preserve := false
	switch s := spec.(type) {
	case string:
		path = s
	case db.Document:
		p, ok := s["path"].(string)
		if !ok {
			return nil, errWire(28812, "Location28812", "$unwind requires a path")
		}
		path = p
		if pv, ok := s["preserveNullAndEmptyArrays"].(bool); ok {
			preserve = pv
		}
	default:
		return nil, errWire(28812, "Location28812", "$unwind requires a path")
	}
	if !strings.HasPrefix(path, "$") || len(path) < 2 {
		return nil, errWire(28818, "Location28818", "$unwind path must start with $")
	}
	field := path[1:]
	var out []db.Document
	for _, d := range docs {
		v, ok := getPath(d, field)
		if !ok || v == nil {
			if preserve {
				out = append(out, d)
			}
			continue
		}
		arr, isArr := v.([]any)
		if !isArr {
			out = append(out, d)
			continue
		}
		if len(arr) == 0 {
			if preserve {
				out = append(out, d)
			}
			continue
		}
		for _, item := range arr {
			nd := deepClone(d).(db.Document)
			if err := setPath(nd, field, item); err != nil {
				return nil, err
			}
			out = append(out, nd)
		}
	}
	return out, nil
}

func lookupDocs(ctx *connCtx, docs []db.Document, spec any) ([]db.Document, error) {
	l, ok := spec.(db.Document)
	if !ok {
		return nil, errWire(40320, "Location40320", "$lookup requires a document")
	}
	from, _ := l["from"].(string)
	localField, _ := l["localField"].(string)
	foreignField, _ := l["foreignField"].(string)
	as, _ := l["as"].(string)
	if from == "" || localField == "" || foreignField == "" || as == "" {
		return nil, errWire(40320, "Location40320", "$lookup requires from, localField, foreignField and as")
	}
	out := make([]db.Document, 0, len(docs))
	for _, d := range docs {
		nd := deepClone(d).(db.Document)
		lv, _ := getPath(d, localField)
		var matches []db.Document
		if lv != nil {
			filter := db.Document{foreignField: lv}
			var err error
			matches, err = ctx.find(from, filter, nil)
			if err != nil {
				return nil, err
			}
		}
		items := make([]any, len(matches))
		for i, m := range matches {
			items[i] = m
		}
		nd[as] = items
		out = append(out, nd)
	}
	return out, nil
}
