package wire

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mlstoredb/db"
)

const (
	bsDouble     byte = 0x01
	bsString     byte = 0x02
	bsDocument   byte = 0x03
	bsArray      byte = 0x04
	bsBinary     byte = 0x05
	bsUndefined  byte = 0x06
	bsObjectID   byte = 0x07
	bsBool       byte = 0x08
	bsDatetime   byte = 0x09
	bsNull       byte = 0x0A
	bsRegex      byte = 0x0B
	bsDBPointer  byte = 0x0C
	bsJS         byte = 0x0D
	bsSymbol     byte = 0x0E
	bsJSScope    byte = 0x0F
	bsInt32      byte = 0x10
	bsTimestamp  byte = 0x11
	bsInt64      byte = 0x12
	bsDecimal128 byte = 0x13
	bsMinKey     byte = 0xFF
	bsMaxKey     byte = 0x7F
)

const maxBSONDepth = 100

var errBSON = errors.New("bson: invalid document")

type bsonReader struct {
	b   []byte
	pos int
}

func (r *bsonReader) readByte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errBSON
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *bsonReader) readInt32() (int32, error) {
	if r.pos+4 > len(r.b) {
		return 0, errBSON
	}
	v := int32(binary.LittleEndian.Uint32(r.b[r.pos:]))
	r.pos += 4
	return v, nil
}

func (r *bsonReader) readInt64() (int64, error) {
	if r.pos+8 > len(r.b) {
		return 0, errBSON
	}
	v := int64(binary.LittleEndian.Uint64(r.b[r.pos:]))
	r.pos += 8
	return v, nil
}

func (r *bsonReader) readUint64() (uint64, error) {
	v, err := r.readInt64()
	return uint64(v), err
}

func (r *bsonReader) readF64() (float64, error) {
	v, err := r.readInt64()
	return math.Float64frombits(uint64(v)), err
}

func (r *bsonReader) readCString() (string, error) {
	for i := r.pos; i < len(r.b); i++ {
		if r.b[i] == 0 {
			s := string(r.b[r.pos:i])
			r.pos = i + 1
			return s, nil
		}
	}
	return "", errBSON
}

func (r *bsonReader) readString() (string, error) {
	n, err := r.readInt32()
	if err != nil {
		return "", err
	}
	if n < 1 || r.pos+int(n) > len(r.b) {
		return "", errBSON
	}
	s := string(r.b[r.pos : r.pos+int(n)-1])
	r.pos += int(n)
	return s, nil
}

func (r *bsonReader) readRaw(n int) ([]byte, error) {
	if n < 0 || r.pos+n > len(r.b) {
		return nil, errBSON
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

func decodeBSON(b []byte) (db.Document, error) {
	doc, _, err := decodeBSONOrdered(b)
	return doc, err
}

func decodeBSONOrdered(b []byte) (db.Document, []string, error) {
	r := &bsonReader{b: b}
	doc, keys, err := readDocumentKeys(r, 0)
	if err != nil {
		return nil, nil, err
	}
	if r.pos != len(b) {
		return nil, nil, fmt.Errorf("%w: %d trailing bytes", errBSON, len(b)-r.pos)
	}
	return doc, keys, nil
}

func readDocument(r *bsonReader, depth int) (db.Document, error) {
	doc, _, err := readDocumentKeys(r, depth)
	return doc, err
}

func readDocumentKeys(r *bsonReader, depth int) (db.Document, []string, error) {
	if depth > maxBSONDepth {
		return nil, nil, fmt.Errorf("%w: depth limit", errBSON)
	}
	start := r.pos
	n, err := r.readInt32()
	if err != nil {
		return nil, nil, err
	}
	if n < 5 {
		return nil, nil, fmt.Errorf("%w: bad length %d", errBSON, n)
	}
	end := start + int(n)
	if end > len(r.b) {
		return nil, nil, fmt.Errorf("%w: length overflow", errBSON)
	}
	doc := db.Document{}
	var keys []string
	for {
		t, err := r.readByte()
		if err != nil {
			return nil, nil, err
		}
		if t == 0x00 {
			break
		}
		key, err := r.readCString()
		if err != nil {
			return nil, nil, err
		}
		v, err := readValue(r, t, depth)
		if err != nil {
			return nil, nil, err
		}
		doc[key] = v
		keys = append(keys, key)
	}
	if r.pos != end {
		return nil, nil, fmt.Errorf("%w: length mismatch", errBSON)
	}
	return doc, keys, nil
}

func readArray(r *bsonReader, depth int) ([]any, error) {
	if depth > maxBSONDepth {
		return nil, fmt.Errorf("%w: depth limit", errBSON)
	}
	start := r.pos
	n, err := r.readInt32()
	if err != nil {
		return nil, err
	}
	if n < 5 {
		return nil, fmt.Errorf("%w: bad length %d", errBSON, n)
	}
	end := start + int(n)
	if end > len(r.b) {
		return nil, fmt.Errorf("%w: length overflow", errBSON)
	}
	var out []any
	for {
		t, err := r.readByte()
		if err != nil {
			return nil, err
		}
		if t == 0x00 {
			break
		}
		_, err = r.readCString()
		if err != nil {
			return nil, err
		}
		v, err := readValue(r, t, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if r.pos != end {
		return nil, fmt.Errorf("%w: length mismatch", errBSON)
	}
	return out, nil
}

func readValue(r *bsonReader, t byte, depth int) (any, error) {
	switch t {
	case bsDouble:
		return r.readF64()
	case bsString, bsSymbol:
		return r.readString()
	case bsDocument:
		return readDocument(r, depth+1)
	case bsArray:
		return readArray(r, depth+1)
	case bsBinary:
		n, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		sub, err := r.readByte()
		if err != nil {
			return nil, err
		}
		raw, err := r.readRaw(int(n))
		if err != nil {
			return nil, err
		}
		if sub == 0x02 {
			if len(raw) < 4 {
				return nil, errBSON
			}
			inner := int(binary.LittleEndian.Uint32(raw))
			if inner < 0 || 4+inner > len(raw) {
				return nil, errBSON
			}
			raw = raw[4 : 4+inner]
		}
		return db.Document{"$binary": db.Document{
			"base64":  base64.StdEncoding.EncodeToString(raw),
			"subType": fmt.Sprintf("%02x", sub),
		}}, nil
	case bsUndefined, bsNull:
		return nil, nil
	case bsObjectID:
		raw, err := r.readRaw(12)
		if err != nil {
			return nil, err
		}
		return db.Document{"$oid": hex.EncodeToString(raw)}, nil
	case bsBool:
		v, err := r.readByte()
		if err != nil {
			return nil, err
		}
		return v != 0, nil
	case bsDatetime:
		ms, err := r.readInt64()
		if err != nil {
			return nil, err
		}
		return db.Document{"$date": float64(ms)}, nil
	case bsRegex:
		p, err := r.readCString()
		if err != nil {
			return nil, err
		}
		o, err := r.readCString()
		if err != nil {
			return nil, err
		}
		return db.Document{"$regularExpression": db.Document{"pattern": p, "options": o}}, nil
	case bsDBPointer:
		ns, err := r.readString()
		if err != nil {
			return nil, err
		}
		raw, err := r.readRaw(12)
		if err != nil {
			return nil, err
		}
		return db.Document{"$dbPointer": db.Document{"$ref": ns, "$id": hex.EncodeToString(raw)}}, nil
	case bsJS:
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		return db.Document{"$code": s}, nil
	case bsJSScope:
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		scope, err := readDocument(r, depth+1)
		if err != nil {
			return nil, err
		}
		return db.Document{"$code": s, "$scope": scope}, nil
	case bsInt32:
		v, err := r.readInt32()
		if err != nil {
			return nil, err
		}
		return float64(v), nil
	case bsTimestamp:
		v, err := r.readUint64()
		if err != nil {
			return nil, err
		}
		return db.Document{"$timestamp": db.Document{"t": float64(v >> 32), "i": float64(v & 0xFFFFFFFF)}}, nil
	case bsInt64:
		v, err := r.readInt64()
		if err != nil {
			return nil, err
		}
		if v >= -(1<<53) && v <= 1<<53 {
			return float64(v), nil
		}
		return db.Document{"$numberLong": strconv.FormatInt(v, 10)}, nil
	case bsDecimal128:
		raw, err := r.readRaw(16)
		if err != nil {
			return nil, err
		}
		return db.Document{"$numberDecimal": hex.EncodeToString(raw)}, nil
	case bsMinKey:
		return db.Document{"$minKey": int32(1)}, nil
	case bsMaxKey:
		return db.Document{"$maxKey": int32(1)}, nil
	}
	return nil, fmt.Errorf("%w: unknown type 0x%02x", errBSON, t)
}

func encodeBSON(doc db.Document) ([]byte, error) {
	return appendDocument(nil, doc, 0)
}

func appendDocument(dst []byte, doc db.Document, depth int) ([]byte, error) {
	if depth > maxBSONDepth {
		return nil, fmt.Errorf("%w: depth limit", errBSON)
	}
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var err error
		dst, err = appendElement(dst, k, doc[k], depth)
		if err != nil {
			return nil, err
		}
	}
	dst = append(dst, 0x00)
	if len(dst)-start > math.MaxInt32 {
		return nil, fmt.Errorf("%w: document too large", errBSON)
	}
	binary.LittleEndian.PutUint32(dst[start:start+4], uint32(len(dst)-start))
	return dst, nil
}

func appendCString(dst []byte, s string) []byte {
	return append(append(dst, s...), 0)
}

func appendString(dst []byte, s string) []byte {
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(s)+1))
	dst = append(dst, l[:]...)
	dst = append(dst, s...)
	return append(dst, 0)
}

func appendInt32(dst []byte, v int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return append(dst, b[:]...)
}

func appendInt64(dst []byte, v int64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	return append(dst, b[:]...)
}

func appendDouble(dst []byte, v float64) []byte {
	return appendInt64(dst, int64(math.Float64bits(v)))
}

func numberType(f float64) byte {
	if f == math.Trunc(f) && !math.IsInf(f, 0) {
		if f >= math.MinInt32 && f <= math.MaxInt32 {
			return bsInt32
		}
		if math.Abs(f) <= 1<<53 {
			return bsInt64
		}
	}
	return bsDouble
}

func appendNumber(dst []byte, f float64) []byte {
	switch numberType(f) {
	case bsInt32:
		return appendInt32(dst, int32(f))
	case bsInt64:
		return appendInt64(dst, int64(f))
	}
	return appendDouble(dst, f)
}

func appendElement(dst []byte, key string, v any, depth int) ([]byte, error) {
	if strings.ContainsRune(key, 0) {
		return nil, fmt.Errorf("%w: NUL in key", errBSON)
	}
	switch val := v.(type) {
	case nil:
		dst = append(dst, bsNull)
		return appendCString(dst, key), nil
	case bool:
		dst = append(dst, bsBool)
		dst = appendCString(dst, key)
		if val {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	case float64:
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return nil, fmt.Errorf("%w: NaN/Inf not representable", errBSON)
		}
		dst = append(dst, numberType(val))
		dst = appendCString(dst, key)
		return appendNumber(dst, val), nil
	case int:
		if val >= math.MinInt32 && val <= math.MaxInt32 {
			dst = append(dst, bsInt32)
			dst = appendCString(dst, key)
			return appendInt32(dst, int32(val)), nil
		}
		dst = append(dst, bsInt64)
		dst = appendCString(dst, key)
		return appendInt64(dst, int64(val)), nil
	case int32:
		dst = append(dst, bsInt32)
		dst = appendCString(dst, key)
		return appendInt32(dst, val), nil
	case int64:
		dst = append(dst, bsInt64)
		dst = appendCString(dst, key)
		return appendInt64(dst, val), nil
	case uint64:
		if val <= math.MaxInt64 {
			return appendElement(dst, key, int64(val), depth)
		}
		return nil, fmt.Errorf("%w: uint64 overflow", errBSON)
	case string:
		dst = append(dst, bsString)
		dst = appendCString(dst, key)
		return appendString(dst, val), nil
	case []byte:
		dst = append(dst, bsBinary)
		dst = appendCString(dst, key)
		dst = appendInt32(dst, int32(len(val)))
		dst = append(dst, 0x00)
		return append(dst, val...), nil
	case time.Time:
		dst = append(dst, bsDatetime)
		dst = appendCString(dst, key)
		return appendInt64(dst, val.UnixMilli()), nil
	case db.Document:
		if ext, ok, err := appendExtended(dst, key, val); ok || err != nil {
			return ext, err
		}
		dst = append(dst, bsDocument)
		dst = appendCString(dst, key)
		return appendDocument(dst, val, depth+1)
	case []any:
		dst = append(dst, bsArray)
		dst = appendCString(dst, key)
		return appendArray(dst, val, depth)
	case []db.Document:
		items := make([]any, len(val))
		for i, d := range val {
			items[i] = d
		}
		return appendElement(dst, key, items, depth)
	case []string:
		items := make([]any, len(val))
		for i, s := range val {
			items[i] = s
		}
		return appendElement(dst, key, items, depth)
	}
	return nil, fmt.Errorf("%w: unsupported value type %T", errBSON, v)
}

func appendArray(dst []byte, items []any, depth int) ([]byte, error) {
	if depth > maxBSONDepth {
		return nil, fmt.Errorf("%w: depth limit", errBSON)
	}
	start := len(dst)
	dst = append(dst, 0, 0, 0, 0)
	for i, v := range items {
		var err error
		dst, err = appendElement(dst, strconv.Itoa(i), v, depth+1)
		if err != nil {
			return nil, err
		}
	}
	dst = append(dst, 0x00)
	binary.LittleEndian.PutUint32(dst[start:start+4], uint32(len(dst)-start))
	return dst, nil
}

func appendExtended(dst []byte, key string, doc db.Document) ([]byte, bool, error) {
	if len(doc) == 1 {
		for k, v := range doc {
			switch k {
			case "$oid":
				s, ok := v.(string)
				if !ok || len(s) != 24 {
					return nil, false, fmt.Errorf("%w: bad $oid", errBSON)
				}
				raw, err := hex.DecodeString(s)
				if err != nil {
					return nil, false, fmt.Errorf("%w: bad $oid", errBSON)
				}
				dst = append(dst, bsObjectID)
				dst = appendCString(dst, key)
				return append(dst, raw...), true, nil
			case "$minKey":
				dst = append(dst, bsMinKey)
				return appendCString(dst, key), true, nil
			case "$maxKey":
				dst = append(dst, bsMaxKey)
				return appendCString(dst, key), true, nil
			case "$numberInt":
				n, ok := toInt64(v)
				if !ok || n < math.MinInt32 || n > math.MaxInt32 {
					return nil, false, fmt.Errorf("%w: bad $numberInt", errBSON)
				}
				dst = append(dst, bsInt32)
				dst = appendCString(dst, key)
				return appendInt32(dst, int32(n)), true, nil
			case "$numberLong":
				n, ok := toInt64(v)
				if !ok {
					return nil, false, fmt.Errorf("%w: bad $numberLong", errBSON)
				}
				dst = append(dst, bsInt64)
				dst = appendCString(dst, key)
				return appendInt64(dst, n), true, nil
			case "$numberDouble":
				f, ok := v.(float64)
				if !ok {
					return nil, false, fmt.Errorf("%w: bad $numberDouble", errBSON)
				}
				dst = append(dst, bsDouble)
				dst = appendCString(dst, key)
				return appendDouble(dst, f), true, nil
			case "$numberDecimal":
				s, ok := v.(string)
				if !ok || len(s) != 32 {
					return nil, false, fmt.Errorf("%w: bad $numberDecimal", errBSON)
				}
				raw, err := hex.DecodeString(s)
				if err != nil {
					return nil, false, fmt.Errorf("%w: bad $numberDecimal", errBSON)
				}
				dst = append(dst, bsDecimal128)
				dst = appendCString(dst, key)
				return append(dst, raw...), true, nil
			case "$date":
				var ms int64
				switch d := v.(type) {
				case float64:
					ms = int64(d)
				case int64:
					ms = d
				case int:
					ms = int64(d)
				case string:
					t, err := time.Parse(time.RFC3339Nano, d)
					if err != nil {
						return nil, false, fmt.Errorf("%w: bad $date", errBSON)
					}
					ms = t.UnixMilli()
				default:
					return nil, false, fmt.Errorf("%w: bad $date", errBSON)
				}
				dst = append(dst, bsDatetime)
				dst = appendCString(dst, key)
				return appendInt64(dst, ms), true, nil
			case "$code":
				s, ok := v.(string)
				if !ok {
					return nil, false, fmt.Errorf("%w: bad $code", errBSON)
				}
				dst = append(dst, bsJS)
				dst = appendCString(dst, key)
				return appendString(dst, s), true, nil
			case "$binary":
				raw, sub, err := parseBinaryExtended(v)
				if err != nil {
					return nil, false, err
				}
				dst = append(dst, bsBinary)
				dst = appendCString(dst, key)
				dst = appendInt32(dst, int32(len(raw)))
				dst = append(dst, sub)
				return append(dst, raw...), true, nil
			case "$regularExpression":
				m, ok := v.(db.Document)
				if !ok {
					return nil, false, fmt.Errorf("%w: bad $regularExpression", errBSON)
				}
				p, _ := m["pattern"].(string)
				o, _ := m["options"].(string)
				dst = append(dst, bsRegex)
				dst = appendCString(dst, key)
				dst = appendCString(dst, p)
				return appendCString(dst, o), true, nil
			case "$timestamp":
				m, ok := v.(db.Document)
				if !ok {
					return nil, false, fmt.Errorf("%w: bad $timestamp", errBSON)
				}
				t, _ := toInt64(m["t"])
				i, _ := toInt64(m["i"])
				dst = append(dst, bsTimestamp)
				dst = appendCString(dst, key)
				return appendInt64(dst, int64(uint64(t)<<32|uint64(i)&0xFFFFFFFF)), true, nil
			case "$dbPointer":
				m, ok := v.(db.Document)
				if !ok {
					return nil, false, fmt.Errorf("%w: bad $dbPointer", errBSON)
				}
				ns, _ := m["$ref"].(string)
				id, _ := m["$id"].(string)
				raw, err := hex.DecodeString(id)
				if err != nil || len(raw) != 12 {
					return nil, false, fmt.Errorf("%w: bad $dbPointer", errBSON)
				}
				dst = append(dst, bsDBPointer)
				dst = appendCString(dst, key)
				dst = appendString(dst, ns)
				return append(dst, raw...), true, nil
			}
		}
		return nil, false, nil
	}
	if len(doc) == 2 {
		code, hasCode := doc["$code"]
		scope, hasScope := doc["$scope"]
		if hasCode && hasScope {
			s, ok := code.(string)
			if !ok {
				return nil, false, fmt.Errorf("%w: bad $code", errBSON)
			}
			sc, ok := scope.(db.Document)
			if !ok {
				return nil, false, fmt.Errorf("%w: bad $scope", errBSON)
			}
			dst = append(dst, bsJSScope)
			dst = appendCString(dst, key)
			dst = appendString(dst, s)
			out, err := appendDocument(dst, sc, 1)
			if err != nil {
				return nil, false, err
			}
			return out, true, nil
		}
	}
	return nil, false, nil
}

func parseBinaryExtended(v any) ([]byte, byte, error) {
	switch b := v.(type) {
	case db.Document:
		b64, _ := b["base64"].(string)
		subHex, _ := b["subType"].(string)
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: bad $binary base64", errBSON)
		}
		sub, err := parseHexByte(subHex)
		if err != nil {
			return nil, 0, err
		}
		return raw, sub, nil
	case string:
		raw, err := base64.StdEncoding.DecodeString(b)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: bad $binary base64", errBSON)
		}
		return raw, 0x00, nil
	}
	return nil, 0, fmt.Errorf("%w: bad $binary", errBSON)
}

func parseHexByte(s string) (byte, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("%w: bad subType", errBSON)
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 1 {
		return 0, fmt.Errorf("%w: bad subType", errBSON)
	}
	return raw[0], nil
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int64(n), true
		}
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		if err == nil {
			return i, true
		}
	case db.Document:
		if s, ok := n["$numberLong"].(string); ok {
			i, err := strconv.ParseInt(s, 10, 64)
			if err == nil {
				return i, true
			}
			return 0, false
		}
		if s, ok := n["$numberInt"].(string); ok {
			i, err := strconv.ParseInt(s, 10, 64)
			if err == nil {
				return i, true
			}
			return 0, false
		}
		if f, ok := n["$numberInt"].(float64); ok && f == math.Trunc(f) {
			return int64(f), true
		}
		if f, ok := n["$numberDouble"].(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
			return int64(f), true
		}
	}
	return 0, false
}
