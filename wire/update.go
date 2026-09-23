package wire

import (
	"reflect"
	"strconv"
	"strings"
	"time"

	"mlstoredb/db"
)

var updateOps = map[string]bool{
	"$set":          true,
	"$unset":        true,
	"$inc":          true,
	"$mul":          true,
	"$min":          true,
	"$max":          true,
	"$rename":       true,
	"$push":         true,
	"$addToSet":     true,
	"$pop":          true,
	"$pull":         true,
	"$pullAll":      true,
	"$currentDate":  true,
	"$setOnInsert":  true,
}

func deepClone(v any) any {
	switch t := v.(type) {
	case db.Document:
		out := db.Document{}
		for k, val := range t {
			out[k] = deepClone(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepClone(val)
		}
		return out
	}
	return v
}

func deepEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		return af == bf
	}
	if aok != bok {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case db.Document:
		if s, ok := n["$numberLong"].(string); ok {
			f, err := strconv.ParseFloat(s, 64)
			return f, err == nil
		}
		if f, ok := n["$numberDouble"].(float64); ok {
			return f, true
		}
		if i, ok := n["$numberInt"].(float64); ok {
			return i, true
		}
	}
	return 0, false
}

func setPath(doc db.Document, path string, v any) error {
	parts := strings.Split(path, ".")
	if parts[0] == "_id" {
		return errWire(66, "ImmutableField", "_id is immutable")
	}
	var cur any = doc
	for i := 0; i < len(parts)-1; i++ {
		p := parts[i]
		m, ok := cur.(db.Document)
		if !ok {
			return errWire(2, "BadValue", "cannot create field '"+p+"' in "+typeName(cur))
		}
		next, exists := m[p]
		if !exists || next == nil {
			nm := db.Document{}
			m[p] = nm
			cur = nm
			continue
		}
		cur = next
	}
	last := parts[len(parts)-1]
	m, ok := cur.(db.Document)
	if !ok {
		return errWire(2, "BadValue", "cannot create field '"+last+"' in "+typeName(cur))
	}
	m[last] = v
	return nil
}

func removePath(doc db.Document, path string) {
	parts := strings.Split(path, ".")
	var cur any = doc
	for i := 0; i < len(parts)-1; i++ {
		m, ok := cur.(db.Document)
		if !ok {
			return
		}
		next, exists := m[parts[i]]
		if !exists {
			return
		}
		cur = next
	}
	if m, ok := cur.(db.Document); ok {
		delete(m, parts[len(parts)-1])
	}
}

func typeName(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case db.Document:
		return "document"
	case []any:
		return "array"
	}
	return reflect.TypeOf(v).String()
}

func applyUpdateExpr(old db.Document, expr any, isUpsert bool) (db.Document, error) {
	switch e := expr.(type) {
	case db.Document:
		hasOp := false
		for k := range e {
			if strings.HasPrefix(k, "$") {
				hasOp = true
				break
			}
		}
		if !hasOp {
			repl := deepClone(e).(db.Document)
			if id, ok := repl["_id"]; ok {
				oid, _ := normalizeIDValue(id)
				if s, ok := oid.(string); !ok || s != old["_id"] {
					return nil, errWire(66, "ImmutableField", "_id is immutable")
				}
			} else if old["_id"] != nil {
				repl["_id"] = old["_id"]
			}
			return repl, nil
		}
		out := deepClone(old).(db.Document)
		for k, v := range e {
			if !updateOps[k] {
				return nil, errWire(9, "FailedToParse", "unknown update operator: "+k)
			}
			if k == "$setOnInsert" && !isUpsert {
				continue
			}
			spec, ok := v.(db.Document)
			if !ok {
				return nil, errWire(9, "FailedToParse", k+" must be a document")
			}
			for path, val := range spec {
				if err := applyUpdateOp(out, k, path, val); err != nil {
					return nil, err
				}
			}
		}
		return out, nil
	case []any:
		return nil, errWire(9, "FailedToParse", "pipeline updates are not supported")
	}
	return nil, errWire(9, "FailedToParse", "update must be a document or pipeline")
}

func applyUpdateOp(doc db.Document, op, path string, val any) error {
	switch op {
	case "$set", "$setOnInsert":
		return setPath(doc, path, val)
	case "$unset":
		removePath(doc, path)
		return nil
	case "$inc", "$mul":
		delta, ok := toFloat(val)
		if !ok {
			return errWire(14, "TypeMismatch", op+" requires a number")
		}
		cur, exists := lookupPath(doc, path)
		var base float64
		if exists && cur != nil {
			b, ok := toFloat(cur)
			if !ok {
				return errWire(14, "TypeMismatch", "cannot apply "+op+" to non-numeric value")
			}
			base = b
		} else if op == "$inc" {
			base = 0
		}
		var res float64
		if op == "$inc" {
			res = base + delta
		} else {
			res = base * delta
		}
		return setPath(doc, path, res)
	case "$min", "$max":
		cur, exists := lookupPath(doc, path)
		if !exists || cur == nil {
			return setPath(doc, path, val)
		}
		c := compareValues(cur, val)
		if (op == "$min" && c > 0) || (op == "$max" && c < 0) {
			return setPath(doc, path, val)
		}
		return nil
	case "$rename":
		newPath, ok := val.(string)
		if !ok || newPath == "" {
			return errWire(2, "BadValue", "$rename requires a string destination")
		}
		cur, exists := lookupPath(doc, path)
		if !exists {
			return nil
		}
		removePath(doc, path)
		return setPath(doc, newPath, cur)
	case "$push":
		return applyPush(doc, path, val)
	case "$addToSet":
		return applyAddToSet(doc, path, val)
	case "$pop":
		return applyPop(doc, path, val)
	case "$pull":
		return applyPull(doc, path, val, false)
	case "$pullAll":
		return applyPull(doc, path, val, true)
	case "$currentDate":
		return applyCurrentDate(doc, path, val)
	}
	return errWire(9, "FailedToParse", "unhandled operator: "+op)
}

func lookupPath(doc db.Document, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = doc
	for _, p := range parts {
		switch c := cur.(type) {
		case db.Document:
			v, ok := c[p]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(p)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, false
			}
			cur = c[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func arrayAtPath(doc db.Document, path string, create bool) ([]any, db.Document, string, error) {
	parts := strings.Split(path, ".")
	var cur any = doc
	for i := 0; i < len(parts)-1; i++ {
		m, ok := cur.(db.Document)
		if !ok {
			return nil, nil, "", errWire(2, "BadValue", "path traverses a non-document")
		}
		next, exists := m[parts[i]]
		if !exists || next == nil {
			if !create {
				return nil, nil, "", errWire(2, "BadValue", "path does not exist")
			}
			nm := db.Document{}
			m[parts[i]] = nm
			cur = nm
			continue
		}
		cur = next
	}
	parent, ok := cur.(db.Document)
	if !ok {
		return nil, nil, "", errWire(2, "BadValue", "path traverses a non-document")
	}
	last := parts[len(parts)-1]
	existing, exists := parent[last]
	if !exists || existing == nil {
		if !create {
			return nil, nil, "", errWire(2, "BadValue", "path does not exist")
		}
		arr := []any{}
		parent[last] = arr
		return arr, parent, last, nil
	}
	arr, ok := existing.([]any)
	if !ok {
		return nil, nil, "", errWire(14, "TypeMismatch", "field is not an array")
	}
	return arr, parent, last, nil
}

func applyPush(doc db.Document, path string, val any) error {
	arr, parent, last, err := arrayAtPath(doc, path, true)
	if err != nil {
		return err
	}
	items := []any{val}
	if spec, ok := val.(db.Document); ok && len(spec) > 0 {
		hasEach := false
		for k := range spec {
			if strings.HasPrefix(k, "$") {
				hasEach = true
				break
			}
		}
		if hasEach {
			each, _ := spec["$each"].([]any)
			items = each
			pos := -1
			if p, ok := spec["$position"].(float64); ok {
				pos = int(p)
			}
			if pos >= 0 {
				rest := append([]any{}, items...)
				items = append([]any{}, arr[:min(pos, len(arr))]...)
				items = append(items, rest...)
				items = append(items, arr[min(pos, len(arr)):]...)
			} else {
				items = append(append([]any{}, arr...), items...)
			}
			if sl, ok := spec["$slice"].(float64); ok {
				n := int(sl)
				if n > 0 && len(items) > n {
					items = items[:n]
				} else if n < 0 && len(items) > -n {
					items = items[len(items)+n:]
				}
			}
			parent[last] = items
			return nil
		}
	}
	parent[last] = append(arr, items...)
	return nil
}

func applyAddToSet(doc db.Document, path string, val any) error {
	arr, parent, last, err := arrayAtPath(doc, path, true)
	if err != nil {
		return err
	}
	var items []any
	if spec, ok := val.(db.Document); ok {
		if each, ok := spec["$each"].([]any); ok {
			items = each
		}
	}
	if items == nil {
		items = []any{val}
	}
	out := append([]any{}, arr...)
	for _, it := range items {
		found := false
		for _, e := range out {
			if deepEqual(e, it) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, it)
		}
	}
	parent[last] = out
	return nil
}

func applyPop(doc db.Document, path string, val any) error {
	arr, parent, last, err := arrayAtPath(doc, path, false)
	if err != nil {
		return nil
	}
	n, ok := toFloat(val)
	if !ok || (n != 1 && n != -1) {
		return errWire(9, "FailedToParse", "$pop requires 1 or -1")
	}
	if len(arr) == 0 {
		return nil
	}
	if n == 1 {
		parent[last] = arr[:len(arr)-1]
	} else {
		parent[last] = arr[1:]
	}
	return nil
}

func applyPull(doc db.Document, path string, val any, pullAll bool) error {
	arr, parent, last, err := arrayAtPath(doc, path, true)
	if err != nil {
		return err
	}
	var cond db.Document
	isCond := false
	if !pullAll {
		if d, ok := val.(db.Document); ok {
			for k := range d {
				if strings.HasPrefix(k, "$") {
					isCond = true
					break
				}
			}
		}
		if isCond {
			cond = val.(db.Document)
		}
	}
	var removals []any
	if pullAll {
		removals, _ = val.([]any)
	}
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		if pullAll {
			drop := false
			for _, r := range removals {
				if deepEqual(e, r) {
					drop = true
					break
				}
			}
			if !drop {
				out = append(out, e)
			}
			continue
		}
		if isCond {
			if !matchElement(e, cond) {
				out = append(out, e)
			}
			continue
		}
		if !deepEqual(e, val) {
			out = append(out, e)
		}
	}
	parent[last] = out
	return nil
}

func matchElement(e any, cond db.Document) bool {
	for k, v := range cond {
		switch k {
		case "$eq":
			if !deepEqual(e, v) {
				return false
			}
		case "$ne":
			if deepEqual(e, v) {
				return false
			}
		case "$gt", "$gte", "$lt", "$lte":
			ev, ok1 := toFloat(e)
			cv, ok2 := toFloat(v)
			if !ok1 || !ok2 {
				return false
			}
			c := 0
			if ev < cv {
				c = -1
			} else if ev > cv {
				c = 1
			}
			switch k {
			case "$gt":
				if c <= 0 {
					return false
				}
			case "$gte":
				if c < 0 {
					return false
				}
			case "$lt":
				if c >= 0 {
					return false
				}
			case "$lte":
				if c > 0 {
					return false
				}
			}
		case "$in":
			items, ok := v.([]any)
			if !ok {
				return false
			}
			found := false
			for _, it := range items {
				if deepEqual(e, it) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func applyCurrentDate(doc db.Document, path string, val any) error {
	now := time.Now()
	var v any
	if d, ok := val.(db.Document); ok {
		if t, ok := d["$type"].(string); ok && t == "timestamp" {
			v = db.Document{"$timestamp": db.Document{"t": float64(now.Unix()), "i": float64(1)}}
		} else {
			v = db.Document{"$date": float64(now.UnixMilli())}
		}
	} else if b, ok := val.(bool); ok && b {
		v = db.Document{"$date": float64(now.UnixMilli())}
	} else {
		return errWire(2, "BadValue", "$currentDate requires true or {$type: ...}")
	}
	return setPath(doc, path, v)
}

func compareValues(a, b any) int {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		}
		return 0
	}
	as, aok2 := a.(string)
	bs, bok2 := b.(string)
	if aok2 && bok2 {
		return strings.Compare(as, bs)
	}
	return 0
}

func diffTopLevel(old, fresh db.Document) (db.Document, []string) {
	patch := db.Document{}
	var remove []string
	for k, v := range fresh {
		if k == "_id" {
			continue
		}
		ov, exists := old[k]
		if !exists || !deepEqual(ov, v) {
			patch[k] = v
		}
	}
	for k := range old {
		if k == "_id" {
			continue
		}
		if _, exists := fresh[k]; !exists {
			remove = append(remove, k)
		}
	}
	return patch, remove
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
