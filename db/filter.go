package db

import (
	"fmt"
	"strings"
)

// match filters: nil/empty = all docs.
// Supported: $eq $ne $gt $gte $lt $lte $in $nin $regex $exists $and $or $not
// plus implicit top-level equality.
func match(doc, filter Document) (bool, error) {
	if len(filter) == 0 {
		return true, nil
	}
	for field, cond := range filter {
		ok, err := matchField(doc, field, cond)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func matchField(doc Document, field string, cond any) (bool, error) {
	// Logical operators at top level or nested under a field.
	if isLogicalOp(field) {
		return matchLogical(doc, field, cond)
	}

	vals, exists := lookupMulti(doc, field)

	// Nested query operators: {"field": {"$gt": 5}}
	if obj, ok := cond.(Document); ok && hasOpKeys(obj) {
		return matchOpsMulti(vals, exists, obj)
	}
	if obj, ok := cond.(map[string]any); ok && hasOpKeys(obj) {
		d, err := toDoc(obj)
		if err != nil {
			return false, err
		}
		return matchOpsMulti(vals, exists, d)
	}

	if !exists {
		// Implicit equality on missing field matches only nil/missing semantics:
		// match if cond is nil (Mongo: missing matches null in $eq with null).
		return cond == nil, nil
	}
	// Array path: match if ANY element equals (DESIGN §6.1).
	for _, v := range vals {
		if equalJSON(v, cond) {
			return true, nil
		}
	}
	return false, nil
}

// matchOpsMulti applies ops if ANY of vals satisfies all ops (array semantics).
// Empty vals + exists=false handled inside matchOneOp via exists flag.
func matchOpsMulti(vals []any, exists bool, ops Document) (bool, error) {
	if !exists || len(vals) == 0 {
		return matchOps(nil, false, ops)
	}
	// Try each value; success if one value satisfies every op.
	for _, v := range vals {
		ok, err := matchOps(v, true, ops)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func matchLogical(doc Document, op string, cond any) (bool, error) {
	switch op {
	case "$and":
		list, err := toDocList(cond)
		if err != nil {
			return false, err
		}
		for _, sub := range list {
			ok, err := match(doc, sub)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	case "$or":
		list, err := toDocList(cond)
		if err != nil {
			return false, err
		}
		for _, sub := range list {
			ok, err := match(doc, sub)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case "$not":
		sub, err := toDoc(cond)
		if err != nil {
			return false, err
		}
		ok, err := match(doc, sub)
		if err != nil {
			return false, err
		}
		return !ok, nil
	case "$nor":
		list, err := toDocList(cond)
		if err != nil {
			return false, err
		}
		for _, sub := range list {
			ok, err := match(doc, sub)
			if err != nil {
				return false, err
			}
			if ok {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("%w: unknown operator %s", ErrBadFilter, op)
	}
}

func matchOps(val any, exists bool, ops Document) (bool, error) {
	// $regex + $options handled together first (map iteration order is random).
	if _, hasRegex := ops["$regex"]; hasRegex {
		ok, err := applyRegex(val, exists, ops)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	for op, arg := range ops {
		if op == "$regex" || op == "$options" {
			continue
		}
		ok, err := matchOneOp(op, val, exists, arg)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func matchOneOp(op string, val any, exists bool, arg any) (bool, error) {
	switch op {
	case "$eq":
		if !exists {
			return arg == nil, nil
		}
		return equalJSON(val, arg), nil
	case "$ne":
		if !exists {
			return arg != nil, nil
		}
		return !equalJSON(val, arg), nil
	case "$gt", "$gte", "$lt", "$lte":
		if !exists {
			return false, nil
		}
		return compareOp(op, val, arg)
	case "$in":
		list, err := toAnyList(arg)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
		for _, item := range list {
			if equalJSON(val, item) {
				return true, nil
			}
		}
		return false, nil
	case "$nin":
		list, err := toAnyList(arg)
		if err != nil {
			return false, err
		}
		if !exists {
			return true, nil
		}
		for _, item := range list {
			if equalJSON(val, item) {
				return false, nil
			}
		}
		return true, nil
	case "$exists":
		want, ok := arg.(bool)
		if !ok {
			return false, fmt.Errorf("%w: $exists needs bool", ErrBadFilter)
		}
		return exists == want, nil
	case "$regex", "$options":
		return true, nil // applied in matchOps before iteration
	case "$not":
		// $not with operator expression
		ops, err := toDoc(arg)
		if err != nil {
			// $not with regex-like value not supported at this level
			return false, err
		}
		ok, err := matchOps(val, exists, ops)
		if err != nil {
			return false, err
		}
		return !ok, nil
	default:
		return false, fmt.Errorf("%w: unknown operator %s", ErrBadFilter, op)
	}
}

// $regex is special: may appear alongside $options.
// When iterating map of ops, $options alone is skipped; $regex applies options
// if present in the same ops map. To avoid map-order issues, matchOps handles
// $regex by looking up sibling $options — we pass the full ops map via matchOpsRegex.

func compareOp(op string, val, arg any) (bool, error) {
	// Allow string/string and number/number comparisons; cross-type → false (not error).
	if !comparableTypes(val, arg) {
		return false, nil
	}
	cmp := compareValues(val, arg)
	switch op {
	case "$gt":
		return cmp > 0, nil
	case "$gte":
		return cmp >= 0, nil
	case "$lt":
		return cmp < 0, nil
	case "$lte":
		return cmp <= 0, nil
	}
	return false, fmt.Errorf("%w: %s", ErrBadFilter, op)
}

func comparableTypes(a, b any) bool {
	_, aNum := toFloat(a)
	_, bNum := toFloat(b)
	if aNum && bNum {
		return true
	}
	_, aStr := a.(string)
	_, bStr := b.(string)
	if aStr && bStr {
		return true
	}
	// bool / nil comparisons
	return false
}

var logicalOps = map[string]bool{
	"$and": true, "$or": true, "$not": true, "$nor": true,
}

func isLogicalOp(field string) bool {
	return logicalOps[field]
}

// hasOpKeys reports if d looks like an operator expression ($-prefixed keys).
// Unknown $-ops still route to matchOps so they return ErrBadFilter.
func hasOpKeys(d Document) bool {
	for k := range d {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

// lookup walks dotted path "a.b.c" and returns the first match (legacy helper).
func lookup(doc Document, path string) (any, bool) {
	vals, ok := lookupMulti(doc, path)
	if !ok || len(vals) == 0 {
		return nil, false
	}
	return vals[0], true
}

// lookupMulti walks dotted path; arrays expand — returns ALL leaf values
// reachable via the path (Mongo: "items.item_id" can hit many elements).
func lookupMulti(doc Document, path string) ([]any, bool) {
	if path == "" {
		return nil, false
	}
	if !strings.Contains(path, ".") {
		v, ok := doc[path]
		if !ok {
			return nil, false
		}
		return []any{v}, true
	}
	parts := strings.Split(path, ".")
	out := lookupPartsMulti(doc, parts)
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func lookupPartsMulti(node any, parts []string) []any {
	if len(parts) == 0 {
		return []any{node}
	}
	switch n := node.(type) {
	case Document:
		v, ok := n[parts[0]]
		if !ok {
			return nil
		}
		return lookupPartsMulti(v, parts[1:])
	case map[string]any:
		v, ok := n[parts[0]]
		if !ok {
			return nil
		}
		return lookupPartsMulti(v, parts[1:])
	case []any:
		var out []any
		for _, el := range n {
			out = append(out, lookupPartsMulti(el, parts)...)
		}
		return out
	default:
		return nil
	}
}

func toDoc(v any) (Document, error) {
	switch d := v.(type) {
	case Document:
		return d, nil
	case map[string]any:
		return Document(d), nil
	default:
		return nil, fmt.Errorf("%w: expected object", ErrBadFilter)
	}
}

func toDocList(v any) ([]Document, error) {
	items, err := toAnyList(v)
	if err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(items))
	for _, it := range items {
		d, err := toDoc(it)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func toAnyList(v any) ([]any, error) {
	switch list := v.(type) {
	case []any:
		return list, nil
	case []Document:
		out := make([]any, len(list))
		for i, d := range list {
			out[i] = d
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: expected array", ErrBadFilter)
	}
}
