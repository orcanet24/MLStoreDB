package wire

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"mlstoredb/db"
)

type testServer struct {
	store *db.Store
	srv   *Server
	addr  net.Addr
}

func startTestServer(t *testing.T, opts ServerOptions) *testServer {
	t.Helper()
	store := db.New()
	if opts.DBName == "" {
		opts.DBName = "testdb"
	}
	srv := NewServer(store, opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	ts := &testServer{store: store, srv: srv, addr: ln.Addr()}
	t.Cleanup(func() {
		_ = srv.Close()
		_ = store.Close()
	})
	return ts
}

type testClient struct {
	t     *testing.T
	conn  net.Conn
	r     *bufio.Reader
	reqID int32
}

func dial(t *testing.T, ts *testServer) *testClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", ts.addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tc := &testClient{t: t, conn: conn, r: bufio.NewReader(conn)}
	t.Cleanup(func() { _ = conn.Close() })
	return tc
}

func (tc *testClient) cmd(doc db.Document) db.Document {
	tc.t.Helper()
	tc.reqID++
	body, err := encodeBSON(doc)
	if err != nil {
		tc.t.Fatalf("encode cmd: %v", err)
	}
	var msg []byte
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(tc.reqID))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(opMsg))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = append(msg, 0x00)
	msg = append(msg, body...)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	if _, err := tc.conn.Write(msg); err != nil {
		tc.t.Fatalf("write: %v", err)
	}
	return tc.readReply()
}

func (tc *testClient) readReply() db.Document {
	tc.t.Helper()
	var hb [16]byte
	if err := readFull(tc.r, hb[:]); err != nil {
		tc.t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	op := int32(binary.LittleEndian.Uint32(hb[12:16]))
	if op != opMsg {
		tc.t.Fatalf("expected OP_MSG reply, got opcode %d", op)
	}
	body := make([]byte, length-16)
	if err := readFull(tc.r, body); err != nil {
		tc.t.Fatalf("read body: %v", err)
	}
	if len(body) < 5 {
		tc.t.Fatalf("OP_MSG reply too short")
	}
	flags := binary.LittleEndian.Uint32(body[0:4])
	if flags != 0 {
		tc.t.Fatalf("unexpected reply flags %d", flags)
	}
	if body[4] != 0x00 {
		tc.t.Fatalf("expected kind-0 section, got %d", body[4])
	}
	doc, err := decodeBSON(body[5:])
	if err != nil {
		tc.t.Fatalf("decode reply: %v", err)
	}
	return doc
}

func readFull(r *bufio.Reader, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}

func (tc *testClient) legacyCmd(fullColl string, query db.Document) db.Document {
	tc.t.Helper()
	tc.reqID++
	qb, err := encodeBSON(query)
	if err != nil {
		tc.t.Fatalf("encode query: %v", err)
	}
	var msg []byte
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(tc.reqID))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(opQuery))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = append(append(msg, fullColl...), 0)
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, 0xFFFFFFFF)
	msg = append(msg, qb...)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	if _, err := tc.conn.Write(msg); err != nil {
		tc.t.Fatalf("write legacy: %v", err)
	}
	return tc.readReplyOp()
}

func (tc *testClient) readReplyOp() db.Document {
	tc.t.Helper()
	var hb [16]byte
	if err := readFull(tc.r, hb[:]); err != nil {
		tc.t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	op := int32(binary.LittleEndian.Uint32(hb[12:16]))
	if op != opReply {
		tc.t.Fatalf("expected OP_REPLY, got opcode %d", op)
	}
	body := make([]byte, length-16)
	if err := readFull(tc.r, body); err != nil {
		tc.t.Fatalf("read body: %v", err)
	}
	n := int32(binary.LittleEndian.Uint32(body[16:20]))
	if n != 1 {
		tc.t.Fatalf("expected 1 doc in OP_REPLY, got %d", n)
	}
	doc, err := decodeBSON(body[20:])
	if err != nil {
		tc.t.Fatalf("decode reply: %v", err)
	}
	return doc
}

func okVal(t *testing.T, reply db.Document) db.Document {
	t.Helper()
	if v, _ := reply["ok"].(float64); v != 1 {
		t.Fatalf("expected ok:1, got %v (reply=%v)", reply["ok"], reply)
	}
	return reply
}

func batchDocs(t *testing.T, reply db.Document, key string) []db.Document {
	t.Helper()
	cur, ok := reply["cursor"].(db.Document)
	if !ok {
		t.Fatalf("no cursor in reply: %v", reply)
	}
	items, ok := cur[key].([]any)
	if !ok {
		t.Fatalf("no %s in cursor: %v", key, cur)
	}
	out := make([]db.Document, 0, len(items))
	for _, it := range items {
		d, ok := it.(db.Document)
		if !ok {
			t.Fatalf("non-doc in batch: %T", it)
		}
		out = append(out, d)
	}
	return out
}

func TestBSONRoundTrip(t *testing.T) {
	doc := db.Document{
		"str":   "héllo",
		"int":   float64(42),
		"neg":   float64(-7),
		"big":   db.Document{"$numberLong": "9007199254740993"},
		"dbl":   3.14,
		"bool":  true,
		"nil":   nil,
		"oid":   db.Document{"$oid": "6512f0a0b1c2d3e4f5a6b7c8"},
		"date":  db.Document{"$date": float64(1695000000000)},
		"bin":   db.Document{"$binary": db.Document{"base64": "AQID", "subType": "00"}},
		"regex": db.Document{"$regularExpression": db.Document{"pattern": "^a", "options": "i"}},
		"arr":   []any{float64(1), "two", nil, true},
		"nest":  db.Document{"deep": db.Document{"x": float64(1)}},
		"min":   db.Document{"$minKey": int32(1)},
		"max":   db.Document{"$maxKey": int32(1)},
		"ts":    db.Document{"$timestamp": db.Document{"t": float64(10), "i": float64(20)}},
	}
	b, err := encodeBSON(doc)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeBSON(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"str", "int", "neg", "dbl", "bool", "nil", "arr"} {
		if !deepEqual(doc[k], back[k]) {
			t.Errorf("field %s: %v != %v", k, doc[k], back[k])
		}
	}
	if oid, _ := back["big"].(db.Document); oid["$numberLong"] != "9007199254740993" {
		t.Errorf("big int64 lost: %v", back["big"])
	}
	if back["oid"].(db.Document)["$oid"] != "6512f0a0b1c2d3e4f5a6b7c8" {
		t.Errorf("oid lost: %v", back["oid"])
	}
	if back["date"].(db.Document)["$date"] != float64(1695000000000) {
		t.Errorf("date lost: %v", back["date"])
	}
	if back["bool"] != true || back["nil"] != nil {
		t.Errorf("bool/nil lost")
	}
}

func TestBSONNumberEncoding(t *testing.T) {
	small, err := encodeBSON(db.Document{"n": float64(5)})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if small[4] != bsInt32 {
		t.Errorf("small int should encode as int32, got 0x%02x", small[4])
	}
	mid, _ := encodeBSON(db.Document{"n": float64(1 << 40)})
	if mid[4] != bsInt64 {
		t.Errorf("mid int should encode as int64, got 0x%02x", mid[4])
	}
	frac, _ := encodeBSON(db.Document{"n": 1.5})
	if frac[4] != bsDouble {
		t.Errorf("fraction should encode as double, got 0x%02x", frac[4])
	}
	for _, b := range [][]byte{small, mid, frac} {
		back, err := decodeBSON(b)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := back["n"].(float64); !ok {
			t.Errorf("number lost: %v", back)
		}
	}
}

func TestHelloHandshake(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	reply := okVal(t, tc.cmd(db.Document{"hello": int32(1), "helloOk": true}))
	if reply["isWritablePrimary"] != true {
		t.Errorf("expected isWritablePrimary, got %v", reply["isWritablePrimary"])
	}
	if reply["helloOk"] != true {
		t.Errorf("expected helloOk")
	}
	if v, _ := reply["maxWireVersion"].(float64); v != 17 {
		t.Errorf("expected maxWireVersion 17, got %v", reply["maxWireVersion"])
	}
	if _, ok := reply["saslSupportedMechs"]; ok {
		t.Errorf("no saslSupportedMechs without auth")
	}
}

func TestLegacyIsMasterHandshake(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	reply := okVal(t, tc.legacyCmd("admin.$cmd", db.Document{"isMaster": int32(1), "helloOk": true}))
	if reply["ismaster"] != true {
		t.Errorf("expected ismaster:true, got %v", reply["ismaster"])
	}
	if reply["helloOk"] != true {
		t.Errorf("expected helloOk")
	}
	ping := okVal(t, tc.legacyCmd("admin.$cmd", db.Document{"ping": int32(1)}))
	if ping["ok"] != float64(1) {
		t.Errorf("ping failed over legacy")
	}
}

func TestInsertFindRoundTrip(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	ins := okVal(t, tc.cmd(db.Document{
		"insert":    "users",
		"documents": []any{db.Document{
			"_id":   db.Document{"$oid": "6512f0a0b1c2d3e4f5a6b7c8"},
			"name":  "Ana",
			"age":   float64(30),
			"tags":  []any{"a", "b"},
			"addr":  db.Document{"city": "Lima", "zip": float64(15001)},
			"when":  db.Document{"$date": float64(1695000000000)},
		}},
	}))
	if ins["n"] != float64(1) {
		t.Fatalf("insert n=%v", ins["n"])
	}
	reply := okVal(t, tc.cmd(db.Document{"find": "users", "filter": db.Document{"name": "Ana"}}))
	docs := batchDocs(t, reply, "firstBatch")
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	d := docs[0]
	if d["_id"] != "6512f0a0b1c2d3e4f5a6b7c8" {
		t.Errorf("_id should normalize to hex string, got %v", d["_id"])
	}
	if d["age"] != float64(30) {
		t.Errorf("age lost: %v", d["age"])
	}
	if d["when"].(db.Document)["$date"] != float64(1695000000000) {
		t.Errorf("date lost: %v", d["when"])
	}
	nest := d["addr"].(db.Document)
	if nest["city"] != "Lima" {
		t.Errorf("nested doc lost: %v", d["addr"])
	}
}

func TestFindFilterSortLimitSkip(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	var docs []any
	for i := 0; i < 10; i++ {
		docs = append(docs, db.Document{"_id": "u" + string(rune('0'+i)), "n": float64(i), "g": "even"})
	}
	okVal(t, tc.cmd(db.Document{"insert": "nums", "documents": docs}))
	reply := okVal(t, tc.cmd(db.Document{
		"find":   "nums",
		"filter": db.Document{"n": db.Document{"$gte": float64(3)}},
		"sort":   db.Document{"n": int32(-1)},
		"skip":   float64(1),
		"limit":  float64(2),
	}))
	got := batchDocs(t, reply, "firstBatch")
	if len(got) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(got))
	}
	if got[0]["n"] != float64(8) || got[1]["n"] != float64(7) {
		t.Errorf("sort/skip/limit wrong: %v %v", got[0]["n"], got[1]["n"])
	}
}

func TestCursorBatching(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	var docs []any
	for i := 0; i < 250; i++ {
		docs = append(docs, db.Document{"_id": "d" + strconv.Itoa(1000+i), "i": float64(i)})
	}
	okVal(t, tc.cmd(db.Document{"insert": "big", "documents": docs}))
	reply := okVal(t, tc.cmd(db.Document{"find": "big", "batchSize": float64(100)}))
	first := batchDocs(t, reply, "firstBatch")
	if len(first) != 100 {
		t.Fatalf("expected first batch 100, got %d", len(first))
	}
	cur, _ := reply["cursor"].(db.Document)
	id, ok := toInt64(cur["id"])
	if !ok || id == 0 {
		t.Fatalf("expected non-zero cursor id, got %v", cur["id"])
	}
	more := okVal(t, tc.cmd(db.Document{"getMore": id, "collection": "big", "batchSize": float64(100)}))
	second := batchDocs(t, more, "nextBatch")
	if len(second) != 100 {
		t.Fatalf("expected second batch 100, got %d", len(second))
	}
	cur2, _ := more["cursor"].(db.Document)
	id2, _ := toInt64(cur2["id"])
	if id2 == 0 {
		t.Fatalf("cursor exhausted early")
	}
	last := okVal(t, tc.cmd(db.Document{"getMore": id2, "collection": "big"}))
	third := batchDocs(t, last, "nextBatch")
	if len(third) != 50 {
		t.Fatalf("expected final batch 50, got %d", len(third))
	}
	cur3, _ := last["cursor"].(db.Document)
	if v, _ := toInt64(cur3["id"]); v != 0 {
		t.Errorf("cursor should be exhausted")
	}
	killed := okVal(t, tc.cmd(db.Document{"killCursors": "big", "cursors": []any{float64(999)}}))
	if killed["cursorsNotFound"] == nil {
		t.Errorf("expected cursorsNotFound for unknown id")
	}
}

func TestCountDistinct(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	docs := []any{
		db.Document{"_id": "a", "g": "x", "n": float64(1)},
		db.Document{"_id": "b", "g": "x", "n": float64(2)},
		db.Document{"_id": "c", "g": "y", "n": float64(3)},
	}
	okVal(t, tc.cmd(db.Document{"insert": "items", "documents": docs}))
	cnt := okVal(t, tc.cmd(db.Document{"count": "items", "query": db.Document{"g": "x"}}))
	if cnt["n"] != float64(2) {
		t.Errorf("count=%v", cnt["n"])
	}
	cd := okVal(t, tc.cmd(db.Document{"countDocuments": "items", "filter": db.Document{}}))
	if cd["n"] != float64(3) {
		t.Errorf("countDocuments=%v", cd["n"])
	}
	est := okVal(t, tc.cmd(db.Document{"estimatedDocumentCount": "items"}))
	if est["n"] != float64(3) {
		t.Errorf("estimated=%v", est["n"])
	}
	dis := okVal(t, tc.cmd(db.Document{"distinct": "items", "key": "g"}))
	vals, _ := dis["values"].([]any)
	if len(vals) != 2 {
		t.Errorf("distinct values=%v", dis["values"])
	}
}

func TestUpdateOperators(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"insert": "t", "documents": []any{db.Document{
		"_id": "1", "a": float64(1), "keep": "yes", "arr": []any{float64(1)},
	}}}))
	up := okVal(t, tc.cmd(db.Document{"update": "t", "updates": []any{db.Document{
		"q": db.Document{"_id": "1"},
		"u": db.Document{
			"$set":    db.Document{"b.nested": "deep", "new": true},
			"$unset":  db.Document{"keep": ""},
			"$inc":    db.Document{"a": float64(4)},
			"$push":   db.Document{"arr": float64(2)},
		},
	}}}))
	if up["n"] != float64(1) || up["nModified"] != float64(1) {
		t.Fatalf("update result: %v", up)
	}
	doc, err := ts.store.Get("t", "1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if doc["a"] != float64(5) {
		t.Errorf("$inc failed: %v", doc["a"])
	}
	if _, exists := doc["keep"]; exists {
		t.Errorf("$unset failed")
	}
	nested := doc["b"].(db.Document)
	if nested["nested"] != "deep" {
		t.Errorf("$set dotted failed: %v", doc["b"])
	}
	arr := doc["arr"].([]any)
	if len(arr) != 2 || arr[1] != float64(2) {
		t.Errorf("$push failed: %v", doc["arr"])
	}
}

func TestUpdateReplaceAndPull(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"insert": "t", "documents": []any{db.Document{
		"_id": "1", "old": float64(1), "tags": []any{"a", "b", "c"},
	}}}))
	okVal(t, tc.cmd(db.Document{"update": "t", "updates": []any{db.Document{
		"q": db.Document{"_id": "1"},
		"u": db.Document{"$pull": db.Document{"tags": "b"}},
	}}}))
	doc, _ := ts.store.Get("t", "1")
	tags := doc["tags"].([]any)
	if len(tags) != 2 || tags[1] != "c" {
		t.Errorf("$pull failed: %v", tags)
	}
	okVal(t, tc.cmd(db.Document{"update": "t", "updates": []any{db.Document{
		"q": db.Document{"_id": "1"},
		"u": db.Document{"fresh": "doc"},
	}}}))
	doc, _ = ts.store.Get("t", "1")
	if _, exists := doc["old"]; exists {
		t.Errorf("replacement should drop old fields")
	}
	if doc["fresh"] != "doc" || doc["_id"] != "1" {
		t.Errorf("replacement wrong: %v", doc)
	}
}

func TestUpdateUpsertMulti(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	up := okVal(t, tc.cmd(db.Document{"update": "t", "updates": []any{db.Document{
		"q":      db.Document{"k": "new"},
		"u":      db.Document{"$set": db.Document{"v": float64(1)}},
		"upsert": true,
	}}}))
	if up["n"] != float64(1) {
		t.Fatalf("upsert n=%v", up)
	}
	ups, _ := up["upserted"].([]any)
	if len(ups) != 1 {
		t.Fatalf("expected upserted entry: %v", up)
	}
	var docs []any
	for i := 0; i < 3; i++ {
		docs = append(docs, db.Document{"_id": "m" + string(rune('0'+i)), "g": "mm"})
	}
	okVal(t, tc.cmd(db.Document{"insert": "t", "documents": docs}))
	multi := okVal(t, tc.cmd(db.Document{"update": "t", "updates": []any{db.Document{
		"q":     db.Document{"g": "mm"},
		"u":     db.Document{"$set": db.Document{"touched": true}},
		"multi": true,
	}}}))
	if multi["n"] != float64(3) || multi["nModified"] != float64(3) {
		t.Fatalf("multi update: %v", multi)
	}
}

func TestDeleteAndFindAndModify(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	docs := []any{
		db.Document{"_id": "1", "v": float64(1)},
		db.Document{"_id": "2", "v": float64(2)},
	}
	okVal(t, tc.cmd(db.Document{"insert": "t", "documents": docs}))
	del := okVal(t, tc.cmd(db.Document{"delete": "t", "deletes": []any{db.Document{
		"q": db.Document{"v": db.Document{"$gte": float64(2)}},
	}}}))
	if del["n"] != float64(1) {
		t.Fatalf("delete n=%v", del)
	}
	fam := okVal(t, tc.cmd(db.Document{
		"findAndModify": "t",
		"query":         db.Document{"_id": "1"},
		"update":        db.Document{"$set": db.Document{"v": float64(10)}},
		"new":           true,
	}))
	val := fam["value"].(db.Document)
	if val["v"] != float64(10) {
		t.Errorf("findAndModify new value wrong: %v", val)
	}
	famRm := okVal(t, tc.cmd(db.Document{
		"findAndModify": "t",
		"query":         db.Document{"_id": "1"},
		"remove":        true,
	}))
	if famRm["value"] == nil {
		t.Errorf("remove should return old doc")
	}
	n, _ := ts.store.Count("t", nil)
	if n != 0 {
		t.Errorf("expected empty collection, got %d", n)
	}
}

func TestAggregatePipeline(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	var docs []any
	for i := 0; i < 6; i++ {
		g := "a"
		if i%2 == 0 {
			g = "b"
		}
		docs = append(docs, db.Document{"_id": "i" + string(rune('0'+i)), "g": g, "n": float64(i)})
	}
	okVal(t, tc.cmd(db.Document{"insert": "agg", "documents": docs}))
	reply := okVal(t, tc.cmd(db.Document{
		"aggregate": "agg",
		"pipeline": []any{
			db.Document{"$match": db.Document{"n": db.Document{"$gte": float64(1)}}},
			db.Document{"$group": db.Document{
				"_id":   "$g",
				"total": db.Document{"$sum": "$n"},
				"cnt":   db.Document{"$count": db.Document{}},
			}},
			db.Document{"$sort": db.Document{"_id": int32(1)}},
		},
		"cursor": db.Document{},
	}))
	grouped := batchDocs(t, reply, "firstBatch")
	if len(grouped) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(grouped))
	}
	if grouped[0]["_id"] != "a" || grouped[0]["total"] != float64(1+3+5) {
		t.Errorf("group a wrong: %v", grouped[0])
	}
	if grouped[1]["_id"] != "b" || grouped[1]["total"] != float64(2+4) {
		t.Errorf("group b wrong: %v", grouped[1])
	}
}

func TestAggregateUnwindLookup(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"insert": "ord", "documents": []any{db.Document{
		"_id": "o1", "items": []any{db.Document{"sku": "A"}, db.Document{"sku": "B"}},
	}}}))
	okVal(t, tc.cmd(db.Document{"insert": "prod", "documents": []any{
		db.Document{"_id": "p1", "sku": "A", "name": "Alpha"},
		db.Document{"_id": "p2", "sku": "B", "name": "Beta"},
	}}))
	reply := okVal(t, tc.cmd(db.Document{
		"aggregate": "ord",
		"pipeline": []any{
			db.Document{"$unwind": "$items"},
			db.Document{"$lookup": db.Document{
				"from":         "prod",
				"localField":   "items.sku",
				"foreignField": "sku",
				"as":           "info",
			}},
		},
		"cursor": db.Document{},
	}))
	docs := batchDocs(t, reply, "firstBatch")
	if len(docs) != 2 {
		t.Fatalf("expected 2 unwound docs, got %d", len(docs))
	}
	info := docs[0]["info"].([]any)
	if len(info) != 1 {
		t.Fatalf("lookup failed: %v", docs[0])
	}
	if info[0].(db.Document)["name"] != "Alpha" {
		t.Errorf("lookup content wrong: %v", info[0])
	}
}

func TestIndexesAdmin(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"insert": "ix", "documents": []any{db.Document{"_id": "1", "email": "a@b.c"}}}))
	created := okVal(t, tc.cmd(db.Document{
		"createIndexes": "ix",
		"indexes":       []any{db.Document{"key": db.Document{"email": int32(1)}, "name": "email_1", "unique": true}},
	}))
	if created["numIndexesAfter"] != float64(1) {
		t.Fatalf("createIndexes: %v", created)
	}
	listed := okVal(t, tc.cmd(db.Document{"listIndexes": "ix"}))
	idxDocs := batchDocs(t, listed, "firstBatch")
	if len(idxDocs) != 1 {
		t.Fatalf("listIndexes: %v", listed)
	}
	if idxDocs[0]["unique"] != true {
		t.Errorf("unique flag lost: %v", idxDocs[0])
	}
	dup := tc.cmd(db.Document{"insert": "ix", "documents": []any{db.Document{"_id": "2", "email": "a@b.c"}}})
	if dup["n"] != float64(0) {
		t.Fatalf("expected n:0 on unique violation, got %v", dup)
	}
	we, _ := dup["writeErrors"].([]any)
	if len(we) != 1 {
		t.Fatalf("expected writeErrors, got %v", dup)
	}
	dropped := okVal(t, tc.cmd(db.Document{"dropIndexes": "ix", "index": "email_1"}))
	if dropped["nIndexes"] != float64(0) {
		t.Fatalf("dropIndexes: %v", dropped)
	}
}

func TestCollectionAdmin(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"create": "newcoll"}))
	listed := okVal(t, tc.cmd(db.Document{"listCollections": db.Document{}}))
	colls := batchDocs(t, listed, "firstBatch")
	found := false
	for _, c := range colls {
		if c["name"] == "newcoll" {
			found = true
		}
	}
	if !found {
		t.Fatalf("created collection not listed: %v", colls)
	}
	okVal(t, tc.cmd(db.Document{"insert": "newcoll", "documents": []any{db.Document{"_id": "1"}}}))
	dropped := okVal(t, tc.cmd(db.Document{"drop": "newcoll"}))
	if dropped["ok"] != float64(1) {
		t.Fatalf("drop failed: %v", dropped)
	}
	n, _ := ts.store.Count("newcoll", nil)
	if n != 0 {
		t.Errorf("docs survived drop")
	}
	if _, err := ts.store.Get("newcoll", "1"); err == nil {
		t.Errorf("doc survived drop")
	}
	missing := tc.cmd(db.Document{"drop": "newcoll"})
	if missing["ok"] != float64(0) {
		t.Errorf("dropping missing coll should fail")
	}
}

func TestListDatabasesAndStats(t *testing.T) {
	ts := startTestServer(t, ServerOptions{DBName: "mydb"})
	tc := dial(t, ts)
	dbs := okVal(t, tc.cmd(db.Document{"listDatabases": int32(1)}))
	list, _ := dbs["databases"].([]any)
	if len(list) != 1 || list[0].(db.Document)["name"] != "mydb" {
		t.Fatalf("listDatabases: %v", dbs)
	}
	okVal(t, tc.cmd(db.Document{"insert": "s", "documents": []any{db.Document{"_id": "1"}}}))
	okVal(t, tc.cmd(db.Document{"dbStats": float64(1)}))
	okVal(t, tc.cmd(db.Document{"collStats": "s"}))
	okVal(t, tc.cmd(db.Document{"serverStatus": db.Document{}}))
	okVal(t, tc.cmd(db.Document{"buildInfo": int32(1)}))
	okVal(t, tc.cmd(db.Document{"connectionStatus": int32(1)}))
	okVal(t, tc.cmd(db.Document{"whatsmyuri": int32(1)}))
	okVal(t, tc.cmd(db.Document{"getLog": "startupWarnings"}))
	okVal(t, tc.cmd(db.Document{"getParameter": int32(1)}))
}

func TestErrors(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	unknown := tc.cmd(db.Document{"totallyBogus": int32(1)})
	if unknown["code"] != float64(59) {
		t.Errorf("expected CommandNotFound, got %v", unknown)
	}
	badOp := tc.cmd(db.Document{"find": "x", "filter": db.Document{"a": db.Document{"$elemMatch": db.Document{"b": float64(1)}}}})
	if badOp["ok"] != float64(0) {
		t.Errorf("unsupported operator should fail: %v", badOp)
	}
	okVal(t, tc.cmd(db.Document{"insert": "d", "documents": []any{db.Document{"_id": "1"}}}))
	dup := tc.cmd(db.Document{"insert": "d", "documents": []any{db.Document{"_id": "1"}}})
	we, _ := dup["writeErrors"].([]any)
	if len(we) != 1 || we[0].(db.Document)["code"] != float64(11000) {
		t.Errorf("expected duplicate writeError 11000, got %v", dup)
	}
	unord := tc.cmd(db.Document{
		"insert":  "d",
		"ordered": false,
		"documents": []any{
			db.Document{"_id": "2"},
			db.Document{"_id": "1"},
			db.Document{"_id": "3"},
		},
	})
	if unord["n"] != float64(2) {
		t.Errorf("unordered insert should continue: %v", unord)
	}
}

func TestProjection(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"insert": "p", "documents": []any{db.Document{
		"_id": "1", "a": float64(1), "b": float64(2), "c": float64(3),
	}}}))
	reply := okVal(t, tc.cmd(db.Document{"find": "p", "projection": db.Document{"a": int32(1)}}))
	doc := batchDocs(t, reply, "firstBatch")[0]
	if len(doc) != 2 || doc["a"] != float64(1) {
		t.Errorf("include projection wrong: %v", doc)
	}
	reply = okVal(t, tc.cmd(db.Document{"find": "p", "projection": db.Document{"a": int32(0)}}))
	doc = batchDocs(t, reply, "firstBatch")[0]
	if _, ok := doc["a"]; ok {
		t.Errorf("exclude projection wrong: %v", doc)
	}
	if doc["b"] != float64(2) {
		t.Errorf("exclude projection kept wrong fields: %v", doc)
	}
	reply = okVal(t, tc.cmd(db.Document{"find": "p", "projection": db.Document{"a": int32(1), "_id": int32(0)}}))
	doc = batchDocs(t, reply, "firstBatch")[0]
	if len(doc) != 1 || doc["a"] != float64(1) {
		t.Errorf("_id:0 projection wrong: %v", doc)
	}
}

func TestLegacyFindAndGetMore(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	var docs []any
	for i := 0; i < 5; i++ {
		docs = append(docs, db.Document{"_id": "l" + string(rune('0'+i)), "v": float64(i)})
	}
	okVal(t, tc.cmd(db.Document{"insert": "leg", "documents": docs}))
	tc.reqID++
	qb, _ := encodeBSON(db.Document{"v": db.Document{"$gte": float64(0)}})
	var msg []byte
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(tc.reqID))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(opQuery))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = append(append(msg, "testdb.leg"...), 0)
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, 2)
	msg = append(msg, qb...)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	if _, err := tc.conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	var hb [16]byte
	if err := readFull(tc.r, hb[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	body := make([]byte, length-16)
	if err := readFull(tc.r, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	nRet := int32(binary.LittleEndian.Uint32(body[16:20]))
	cursorID := int64(binary.LittleEndian.Uint64(body[4:12]))
	if nRet != 2 {
		t.Fatalf("legacy find should return 2 docs, got %d", nRet)
	}
	if cursorID == 0 {
		t.Fatalf("legacy find should leave open cursor")
	}
	tc.reqID++
	gm := []byte{}
	gm = binary.LittleEndian.AppendUint32(gm, 0)
	gm = binary.LittleEndian.AppendUint32(gm, uint32(tc.reqID))
	gm = binary.LittleEndian.AppendUint32(gm, 0)
	gm = binary.LittleEndian.AppendUint32(gm, uint32(opGetMore))
	gm = binary.LittleEndian.AppendUint32(gm, 0)
	gm = append(append(gm, "testdb.leg"...), 0)
	gm = binary.LittleEndian.AppendUint32(gm, 0)
	var cid [8]byte
	binary.LittleEndian.PutUint64(cid[:], uint64(cursorID))
	gm = append(gm, cid[:]...)
	binary.LittleEndian.PutUint32(gm[0:4], uint32(len(gm)))
	if _, err := tc.conn.Write(gm); err != nil {
		t.Fatalf("write getMore: %v", err)
	}
	if err := readFull(tc.r, hb[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length = int32(binary.LittleEndian.Uint32(hb[0:4]))
	body = make([]byte, length-16)
	if err := readFull(tc.r, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	nRet = int32(binary.LittleEndian.Uint32(body[16:20]))
	if nRet != 3 {
		t.Fatalf("legacy getMore should return 3 docs, got %d", nRet)
	}
}

func TestKind1DocumentSequence(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	tc.reqID++
	bodyDoc, _ := encodeBSON(db.Document{"insert": "seq", "ordered": true})
	d1, _ := encodeBSON(db.Document{"_id": "s1"})
	d2, _ := encodeBSON(db.Document{"_id": "s2"})
	secSize := 4 + len("documents") + 1 + len(d1) + len(d2)
	var msg []byte
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(tc.reqID))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(opMsg))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = append(msg, 0x00)
	msg = append(msg, bodyDoc...)
	msg = append(msg, 0x01)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(secSize))
	msg = append(append(msg, "documents"...), 0)
	msg = append(msg, d1...)
	msg = append(msg, d2...)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	if _, err := tc.conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := tc.readReply()
	if reply["n"] != float64(2) {
		t.Fatalf("kind-1 sequence insert: %v", reply)
	}
	n, _ := ts.store.Count("seq", nil)
	if n != 2 {
		t.Errorf("expected 2 docs, got %d", n)
	}
}

func TestChecksumMessage(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	tc.reqID++
	bodyDoc, _ := encodeBSON(db.Document{"ping": int32(1)})
	var msg []byte
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(tc.reqID))
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = binary.LittleEndian.AppendUint32(msg, uint32(opMsg))
	msg = binary.LittleEndian.AppendUint32(msg, 1)
	msg = append(msg, 0x00)
	msg = append(msg, bodyDoc...)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)+4))
	sum := crc32.Checksum(msg, crc32c)
	msg = binary.LittleEndian.AppendUint32(msg, sum)
	if _, err := tc.conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := tc.readReply()
	if reply["ok"] != float64(1) {
		t.Fatalf("checksummed ping failed: %v", reply)
	}
}

func TestSCRAMAuth(t *testing.T) {
	ts := startTestServer(t, ServerOptions{AuthUser: "admin", AuthPass: "secret"})
	if err := ts.store.CreateRole("root", []db.Permission{{Collection: "*", Read: true, Write: true}}); err != nil {
		t.Fatalf("createRole: %v", err)
	}
	if err := ts.store.CreateUser("admin", "secret", []string{"root"}); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	tc := dial(t, ts)
	denied := tc.cmd(db.Document{"find": "x", "filter": db.Document{}})
	if denied["code"] != float64(13) {
		t.Fatalf("unauthenticated find should be Unauthorized, got %v", denied)
	}
	hello := okVal(t, tc.cmd(db.Document{"hello": int32(1)}))
	if hello["saslSupportedMechs"] == nil {
		t.Errorf("hello should list saslSupportedMechs")
	}
	clientFirst := "n,,n=admin,r=cnonce123"
	start := okVal(t, tc.cmd(db.Document{
		"saslStart":  int32(1),
		"mechanism":  "SCRAM-SHA-256",
		"payload":    binaryDoc([]byte(clientFirst)),
		"options":    db.Document{"skipEmptyExchange": true},
	}))
	pl, _ := start["payload"].(db.Document)
	raw, _, err := parseBinaryExtended(pl["$binary"])
	if err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	serverFirst := string(raw)
	if !strings.HasPrefix(serverFirst, "r=cnonce123") {
		t.Fatalf("server nonce wrong: %s", serverFirst)
	}
	authed := scramClientFinish(t, tc, "admin", "secret", clientFirst, serverFirst)
	if authed["done"] != true {
		t.Fatalf("saslContinue: %v", authed)
	}
	find := okVal(t, tc.cmd(db.Document{"find": "x", "filter": db.Document{}}))
	if find["ok"] != float64(1) {
		t.Fatalf("post-auth find failed: %v", find)
	}
}

func TestSCRAMWrongPassword(t *testing.T) {
	ts := startTestServer(t, ServerOptions{AuthUser: "admin", AuthPass: "secret"})
	tc := dial(t, ts)
	clientFirst := "n,,n=admin,r=nonceX"
	start := okVal(t, tc.cmd(db.Document{
		"saslStart": int32(1),
		"mechanism": "SCRAM-SHA-256",
		"payload":   binaryDoc([]byte(clientFirst)),
	}))
	pl, _ := start["payload"].(db.Document)
	raw, _, _ := parseBinaryExtended(pl["$binary"])
	fail := scramClientFinish(t, tc, "admin", "wrong", clientFirst, string(raw))
	if fail["ok"] != float64(0) || fail["code"] != float64(18) {
		t.Fatalf("wrong password should fail 18, got %v", fail)
	}
}

func scramClientFinish(t *testing.T, tc *testClient, user, pass, clientFirst, serverFirst string) db.Document {
	t.Helper()
	var r, s, i string
	for _, part := range strings.Split(serverFirst, ",") {
		if strings.HasPrefix(part, "r=") {
			r = part[2:]
		} else if strings.HasPrefix(part, "s=") {
			s = part[2:]
		} else if strings.HasPrefix(part, "i=") {
			i = part[2:]
		}
	}
	salt, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("salt: %v", err)
	}
	iters, _ := strconv.Atoi(i)
	salted := pbkdf2.Key([]byte(pass), salt, iters, 32, sha256.New)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedHash := sha256.Sum256(clientKey)
	clientFinalNoProof := "c=biws,r=" + r
	authMessage := clientFirst[3:] + "," + serverFirst + "," + clientFinalNoProof
	clientSig := hmacSHA256(storedHash[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for j := range clientKey {
		proof[j] = clientKey[j] ^ clientSig[j]
	}
	final := clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	return tc.cmd(db.Document{
		"saslContinue":   int32(1),
		"conversationId": int32(1),
		"payload":        binaryDoc([]byte(final)),
	})
}

func TestEngineAuthActiveWithoutWireAuth(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	if err := ts.store.CreateUser("u", "p", nil); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	tc := dial(t, ts)
	reply := tc.cmd(db.Document{"find": "x", "filter": db.Document{}})
	if reply["code"] != float64(13) {
		t.Fatalf("engine RBAC should deny raw access, got %v", reply)
	}
}

func TestGraphOverWire(t *testing.T) {
	ts := startTestServer(t, ServerOptions{})
	tc := dial(t, ts)
	okVal(t, tc.cmd(db.Document{"insert": "people", "documents": []any{
		db.Document{"_id": "ana"},
		db.Document{"_id": "bob"},
	}}))
	okVal(t, tc.cmd(db.Document{"insert": "edges.knows", "documents": []any{
		db.Document{"_from": "ana", "_to": "bob"},
	}}))
	reply := okVal(t, tc.cmd(db.Document{"find": "edges.knows", "filter": db.Document{"_from": "ana"}}))
	docs := batchDocs(t, reply, "firstBatch")
	if len(docs) != 1 || docs[0]["_to"] != "bob" {
		t.Fatalf("edge find failed: %v", reply)
	}
}

func TestNaNRejected(t *testing.T) {
	doc := db.Document{"x": math.NaN()}
	if _, err := encodeBSON(doc); err == nil {
		t.Errorf("NaN should fail to encode")
	}
}

func TestHexIDNormalization(t *testing.T) {
	v, err := normalizeIDValue(db.Document{"$oid": "6512f0a0b1c2d3e4f5a6b7c8"})
	if err != nil || v != "6512f0a0b1c2d3e4f5a6b7c8" {
		t.Errorf("oid normalize: %v %v", v, err)
	}
	v, err = normalizeIDValue(float64(42))
	if err != nil || v != "42" {
		t.Errorf("num normalize: %v %v", v, err)
	}
	if _, err := normalizeIDValue([]any{}); err == nil {
		t.Errorf("array _id should fail")
	}
}
