package wire

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"mlstoredb/db"
)

var engineOps = map[string]bool{
	"$eq":      true,
	"$ne":      true,
	"$gt":      true,
	"$gte":     true,
	"$lt":      true,
	"$lte":     true,
	"$in":      true,
	"$nin":     true,
	"$regex":   true,
	"$options": true,
	"$exists":  true,
	"$not":     true,
}

var dropOps = map[string]bool{
	"$comment":   true,
	"$hint":      true,
	"$maxTimeMS": true,
}

func compileRegex(pattern, options string) (*regexp.Regexp, error) {
	var prefix string
	for _, o := range options {
		switch o {
		case 'i':
			prefix += "i"
		case 'm':
			prefix += "m"
		case 's':
			prefix += "s"
		case 'x':
			prefix += "x"
		case 'u':
		default:
			return nil, fmt.Errorf("unsupported regex option %q", o)
		}
	}
	if prefix != "" {
		pattern = "(?" + prefix + ")" + pattern
	}
	return regexp.Compile(pattern)
}

func translateFilter(v any) (db.Document, error) {
	if v == nil {
		return nil, nil
	}
	src, ok := v.(db.Document)
	if !ok {
		return nil, errWire(2, "BadValue", "filter must be a document")
	}
	return translateFilterDoc(src)
}

func translateFilterDoc(src db.Document) (db.Document, error) {
	out := db.Document{}
	for k, v := range src {
		switch {
		case k == "$and" || k == "$or" || k == "$nor":
			items, ok := v.([]any)
			if !ok {
				return nil, errWire(2, "BadValue", k+" must be an array")
			}
			var clauses []any
			for _, item := range items {
				d, ok := item.(db.Document)
				if !ok {
					return nil, errWire(2, "BadValue", k+" clauses must be documents")
				}
				t, err := translateFilterDoc(d)
				if err != nil {
					return nil, err
				}
				clauses = append(clauses, t)
			}
			out[k] = clauses
		case k == "$not":
			d, ok := v.(db.Document)
			if !ok {
				return nil, errWire(2, "BadValue", "$not must be a document")
			}
			t, err := translateFilterDoc(d)
			if err != nil {
				return nil, err
			}
			out[k] = t
		case dropOps[k]:
		case strings.HasPrefix(k, "$"):
			return nil, errWire(2, "BadValue", "unsupported query operator: "+k)
		default:
			nv, err := translateFieldValue(k, v)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
	}
	return out, nil
}

func translateFieldValue(key string, v any) (any, error) {
	if re, ok := v.(db.Document); ok && len(re) == 1 {
		if m, ok := re["$regularExpression"].(db.Document); ok {
			p, _ := m["pattern"].(string)
			o, _ := m["options"].(string)
			return db.Document{"$regex": p, "$options": o}, nil
		}
	}
	doc, ok := v.(db.Document)
	if !ok {
		if key == "_id" {
			return normalizeIDValue(v)
		}
		return v, nil
	}
	hasDollar := false
	for fk := range doc {
		if strings.HasPrefix(fk, "$") {
			hasDollar = true
			break
		}
	}
	if !hasDollar {
		if key == "_id" {
			return normalizeIDValue(v)
		}
		return v, nil
	}
	out := db.Document{}
	for fk, fv := range doc {
		if !engineOps[fk] {
			return nil, errWire(2, "BadValue", "unsupported query operator: "+fk)
		}
		switch fk {
		case "$in", "$nin":
			items, ok := fv.([]any)
			if !ok {
				return nil, errWire(2, "BadValue", fk+" must be an array")
			}
			if key == "_id" {
				norm := make([]any, len(items))
				for i, it := range items {
					n, err := normalizeIDValue(it)
					if err != nil {
						return nil, err
					}
					norm[i] = n
				}
				fv = norm
			}
		case "$eq":
			if key == "_id" {
				n, err := normalizeIDValue(fv)
				if err != nil {
					return nil, err
				}
				fv = n
			}
		case "$not":
			nd, ok := fv.(db.Document)
			if !ok {
				return nil, errWire(2, "BadValue", "$not must be a document")
			}
			nt, err := translateFilterDoc(nd)
			if err != nil {
				return nil, err
			}
			fv = nt
		case "$regex":
			p, ok := fv.(string)
			if !ok {
				return nil, errWire(2, "BadValue", "$regex must be a string")
			}
			opts := ""
			if o, ok := doc["$options"].(string); ok {
				opts = o
			}
			if _, err := compileRegex(p, opts); err != nil {
				return nil, errWire(51008, "BadRegex", "invalid $regex: "+err.Error())
			}
		case "$options":
			continue
		}
		out[fk] = fv
	}
	if _, ok := out["$regex"]; ok {
		if o, ok := doc["$options"].(string); ok {
			out["$options"] = o
		}
	}
	return out, nil
}

func normalizeIDValue(v any) (any, error) {
	switch id := v.(type) {
	case string:
		return id, nil
	case db.Document:
		if oid, ok := id["$oid"].(string); ok && len(oid) == 24 {
			return oid, nil
		}
		if nl, ok := id["$numberLong"]; ok {
			if s, ok := nl.(string); ok {
				return s, nil
			}
		}
		return nil, errWire(2, "BadValue", "_id must be a string, ObjectId or number")
	case float64:
		return formatIDNumber(id), nil
	case int32:
		return strconv.FormatInt(int64(id), 10), nil
	case int64:
		return strconv.FormatInt(id, 10), nil
	case bool:
		if id {
			return "true", nil
		}
		return "false", nil
	}
	return nil, errWire(2, "BadValue", "_id must be a string, ObjectId or number")
}

func formatIDNumber(f float64) string {
	if f == trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func trunc(f float64) float64 {
	if f < 0 {
		return -float64(int64(-f))
	}
	return float64(int64(f))
}

func normalizeDocID(doc db.Document) error {
	if v, ok := doc["_id"]; ok {
		n, err := normalizeIDValue(v)
		if err != nil {
			return err
		}
		s, ok := n.(string)
		if !ok {
			return errWire(2, "BadValue", "_id must normalize to a string")
		}
		doc["_id"] = s
	}
	return nil
}

func equalityFields(q db.Document) db.Document {
	out := db.Document{}
	for k, v := range q {
		if strings.HasPrefix(k, "$") {
			continue
		}
		if doc, ok := v.(db.Document); ok {
			hasDollar := false
			for fk := range doc {
				if strings.HasPrefix(fk, "$") {
					hasDollar = true
					break
				}
			}
			if hasDollar {
				if eq, ok := doc["$eq"]; ok && len(doc) == 1 {
					if n, err := normalizeIDValue(eq); err == nil {
						out[k] = n
					}
				}
				continue
			}
		}
		if k == "_id" {
			if n, err := normalizeIDValue(v); err == nil {
				out[k] = n
			}
			continue
		}
		out[k] = v
	}
	return out
}

func getPath(doc db.Document, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = doc
	for _, p := range parts {
		m, ok := cur.(db.Document)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
