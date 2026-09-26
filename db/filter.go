package db

import (
	"fmt"
	"strings"
)

// match aplica filtros: nil/vacío = todos los documentos.
// Soportados: $eq $ne $gt $gte $lt $lte $in $nin $regex $exists $and $or $not
// más igualdad implícita en el primer nivel.
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
	// Operadores lógicos en el primer nivel o anidados bajo un campo.
	if isLogicalOp(field) {
		return matchLogical(doc, field, cond)
	}

	vals, exists := lookupMulti(doc, field)

	// Operadores de consulta anidados: {"field": {"$gt": 5}}
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
		// La igualdad implícita sobre un campo ausente solo encaja con la semántica de nil/ausente:
		// coincide si cond es nil (Mongo: ausente coincide con null en $eq con null).
		return cond == nil, nil
	}
	// Camino de array: coincide si CUALQUIER elemento es igual (DESIGN §6.1).
	for _, v := range vals {
		if equalJSON(v, cond) {
			return true, nil
		}
	}
	return false, nil
}

// matchOpsMulti aplica los operadores si CUALQUIERA de los vals cumple todos los operadores (semántica de array).
// El caso vals vacío + exists=false se trata dentro de matchOneOp mediante el flag exists.
func matchOpsMulti(vals []any, exists bool, ops Document) (bool, error) {
	if !exists || len(vals) == 0 {
		return matchOps(nil, false, ops)
	}
	// Se prueba cada valor; hay éxito si un valor cumple todos los operadores.
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
	// $regex + $options se tratan juntos primero (el orden de iteración del mapa es aleatorio).
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
		return true, nil // ya se aplicó en matchOps antes de iterar
	case "$not":
		// $not con una expresión de operadores
		ops, err := toDoc(arg)
		if err != nil {
			// $not con un valor tipo regex no está soportado en este nivel
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

// $regex es especial: puede aparecer junto a $options.
// Al iterar el mapa de operadores, $options por sí solo se ignora; $regex aplica las
// opciones si están presentes en el mismo mapa. Para evitar problemas de orden del
// mapa, matchOps trata $regex buscando el $options hermano — se le pasa el mapa
// completo de operadores mediante matchOpsRegex.

func compareOp(op string, val, arg any) (bool, error) {
	// Se permiten comparaciones string/string y número/número; entre tipos distintos → false (no es error).
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
	// comparaciones bool / nil
	return false
}

var logicalOps = map[string]bool{
	"$and": true, "$or": true, "$not": true, "$nor": true,
}

func isLogicalOp(field string) bool {
	return logicalOps[field]
}

// hasOpKeys indica si d parece una expresión de operadores (claves que empiezan por $).
// Los $-ops desconocidos siguen yendo a matchOps para que devuelvan ErrBadFilter.
func hasOpKeys(d Document) bool {
	for k := range d {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

// lookup recorre la ruta con puntos "a.b.c" y devuelve la primera coincidencia (ayudante heredado).
func lookup(doc Document, path string) (any, bool) {
	vals, ok := lookupMulti(doc, path)
	if !ok || len(vals) == 0 {
		return nil, false
	}
	return vals[0], true
}

// lookupMulti recorre la ruta con puntos; los arrays se expanden y devuelve TODOS los
// valores hoja alcanzables por la ruta (Mongo: "items.item_id" puede tocar varios elementos).
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
