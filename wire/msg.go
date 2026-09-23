package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strings"

	"mlstoredb/db"
)

const (
	opReply       int32 = 1
	opUpdate      int32 = 2001
	opInsert      int32 = 2002
	opQuery       int32 = 2004
	opGetMore     int32 = 2005
	opDelete      int32 = 2006
	opKillCursors int32 = 2007
	opCompressed  int32 = 2012
	opMsg         int32 = 2013
)

const (
	flagChecksumPresent uint32 = 1 << 0
	flagMoreToCome      uint32 = 1 << 1
	flagExhaustAllowed  uint32 = 1 << 16
)

const (
	maxMessageSize    = 48 * 1024 * 1024
	maxWriteBatchSize = 100000
)

var crc32c = crc32.MakeTable(crc32.Castagnoli)

type message struct {
	opCode     int32
	requestID  int32
	flags      uint32
	moreToCome bool
	body       db.Document
	bodyKeys   []string
	exhaustOk  bool

	isLegacyFind bool
	fullColl     string
	nSkip        int32
	nReturn      int32
	fields       db.Document
	queryDoc     db.Document
}

type msgHeader struct {
	length     int32
	requestID  int32
	responseTo int32
	opCode     int32
}

func readMessage(r *bufio.Reader) (*message, error) {
	var hb [16]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return nil, err
	}
	h := msgHeader{
		length:     int32(binary.LittleEndian.Uint32(hb[0:4])),
		requestID:  int32(binary.LittleEndian.Uint32(hb[4:8])),
		responseTo: int32(binary.LittleEndian.Uint32(hb[8:12])),
		opCode:     int32(binary.LittleEndian.Uint32(hb[12:16])),
	}
	if h.length < 16 {
		return nil, fmt.Errorf("wire: message length %d < 16", h.length)
	}
	if h.length > maxMessageSize {
		return nil, fmt.Errorf("wire: message length %d exceeds %d", h.length, maxMessageSize)
	}
	body := make([]byte, h.length-16)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	m := &message{opCode: h.opCode, requestID: h.requestID}
	switch h.opCode {
	case opMsg:
		if len(body) < 5 {
			return nil, fmt.Errorf("wire: OP_MSG too short")
		}
		m.flags = binary.LittleEndian.Uint32(body[0:4])
		if m.flags&flagChecksumPresent != 0 {
			if len(body) < 4 {
				return nil, fmt.Errorf("wire: OP_MSG checksum missing")
			}
			want := binary.LittleEndian.Uint32(body[len(body)-4:])
			signed := append(append([]byte{}, hb[:]...), body[:len(body)-4]...)
			got := crc32.Checksum(signed, crc32c)
			if got != want {
				return nil, fmt.Errorf("wire: OP_MSG checksum mismatch")
			}
			body = body[:len(body)-4]
		}
		payload := body[4:]
		m.moreToCome = m.flags&flagMoreToCome != 0
		m.exhaustOk = m.flags&flagExhaustAllowed != 0
		if err := parseSections(m, payload); err != nil {
			return nil, err
		}
		return m, nil
	case opQuery:
		return parseOpQuery(m, body)
	case opGetMore:
		return parseOpGetMore(m, body)
	case opKillCursors:
		return parseOpKillCursors(m, body)
	case opCompressed:
		return nil, fmt.Errorf("wire: compression not supported")
	}
	return nil, fmt.Errorf("wire: unsupported opcode %d", h.opCode)
}

func parseSections(m *message, payload []byte) error {
	r := &bsonReader{b: payload}
	seqs := map[string][]any{}
	for r.pos < len(payload) {
		kind, err := r.readByte()
		if err != nil {
			return err
		}
		switch kind {
		case 0:
			doc, keys, err := readDocumentKeys(r, 0)
			if err != nil {
				return err
			}
			m.body = doc
			m.bodyKeys = keys
		case 1:
			size, err := r.readInt32()
			if err != nil {
				return err
			}
			if size < 5 || r.pos-4+int(size) > len(payload) {
				return fmt.Errorf("wire: bad section size")
			}
			end := r.pos - 4 + int(size)
			ident, err := r.readCString()
			if err != nil {
				return err
			}
			for r.pos < end {
				doc, err := readDocument(r, 0)
				if err != nil {
					return err
				}
				seqs[ident] = append(seqs[ident], doc)
			}
			if r.pos != end {
				return fmt.Errorf("wire: section overrun")
			}
		default:
			return fmt.Errorf("wire: unknown section kind %d", kind)
		}
	}
	if m.body == nil {
		return fmt.Errorf("wire: OP_MSG without body section")
	}
	for ident, docs := range seqs {
		var merged []any
		if existing, ok := m.body[ident].([]any); ok {
			merged = append(merged, existing...)
		}
		merged = append(merged, docs...)
		m.body[ident] = merged
	}
	return nil
}

func parseOpQuery(m *message, body []byte) (*message, error) {
	r := &bsonReader{b: body}
	if _, err := r.readInt32(); err != nil {
		return nil, err
	}
	full, err := r.readCString()
	if err != nil {
		return nil, err
	}
	skip, err := r.readInt32()
	if err != nil {
		return nil, err
	}
	ret, err := r.readInt32()
	if err != nil {
		return nil, err
	}
	query, queryKeys, err := readDocumentKeys(r, 0)
	if err != nil {
		return nil, err
	}
	var fields db.Document
	if r.pos < len(body) {
		f, err := readDocument(r, 0)
		if err != nil {
			return nil, err
		}
		fields = f
	}
	m.fullColl = full
	m.nSkip = skip
	m.nReturn = ret
	m.fields = fields
	if strings.HasSuffix(full, ".$cmd") {
		m.body = query
		m.bodyKeys = queryKeys
		if wrapped, ok := query["$query"].(db.Document); ok {
			m.body = wrapped
			m.bodyKeys = queryKeys[:0]
			for k := range wrapped {
				m.bodyKeys = append(m.bodyKeys, k)
			}
			if ord, ok := query["$orderby"].(db.Document); ok {
				merged := db.Document{}
				for k, v := range m.body {
					merged[k] = v
				}
				merged["sort"] = ord
				m.body = merged
				m.bodyKeys = append(m.bodyKeys, "sort")
			}
		}
		return m, nil
	}
	m.isLegacyFind = true
	m.queryDoc = query
	return m, nil
}

func parseOpGetMore(m *message, body []byte) (*message, error) {
	r := &bsonReader{b: body}
	if _, err := r.readInt32(); err != nil {
		return nil, err
	}
	full, err := r.readCString()
	if err != nil {
		return nil, err
	}
	ret, err := r.readInt32()
	if err != nil {
		return nil, err
	}
	cid, err := r.readInt64()
	if err != nil {
		return nil, err
	}
	coll := collOf(full)
	m.body = db.Document{"getMore": cid, "collection": coll}
	m.bodyKeys = []string{"getMore", "collection"}
	if ret > 0 {
		m.body["batchSize"] = float64(ret)
		m.bodyKeys = append(m.bodyKeys, "batchSize")
	}
	return m, nil
}

func parseOpKillCursors(m *message, body []byte) (*message, error) {
	r := &bsonReader{b: body}
	if _, err := r.readInt32(); err != nil {
		return nil, err
	}
	n, err := r.readInt32()
	if err != nil {
		return nil, err
	}
	if n < 0 || r.pos+int(n)*8 > len(body) {
		return nil, fmt.Errorf("wire: bad killCursors count")
	}
	ids := make([]any, 0, n)
	for i := int32(0); i < n; i++ {
		cid, err := r.readInt64()
		if err != nil {
			return nil, err
		}
		ids = append(ids, cid)
	}
	m.body = db.Document{"killCursors": "", "cursors": ids}
	m.bodyKeys = []string{"killCursors", "cursors"}
	return m, nil
}

func fmtInt64(v int64) string {
	return fmt.Sprintf("%d", v)
}

func collOf(fullColl string) string {
	if i := strings.IndexByte(fullColl, '.'); i >= 0 {
		return fullColl[i+1:]
	}
	return fullColl
}

func writeOpMsg(w *bufio.Writer, responseTo int32, responseID int32, doc db.Document) error {
	body, err := encodeBSON(doc)
	if err != nil {
		return err
	}
	total := 16 + 4 + 1 + len(body)
	var hb [16]byte
	binary.LittleEndian.PutUint32(hb[0:4], uint32(total))
	binary.LittleEndian.PutUint32(hb[4:8], uint32(responseID))
	binary.LittleEndian.PutUint32(hb[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(hb[12:16], uint32(opMsg))
	if _, err := w.Write(hb[:]); err != nil {
		return err
	}
	var fb [4]byte
	binary.LittleEndian.PutUint32(fb[:], 0)
	if _, err := w.Write(fb[:]); err != nil {
		return err
	}
	if err := w.WriteByte(0x00); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Flush()
}

func writeOpReply(w *bufio.Writer, responseTo int32, responseID int32, cursorID int64, startingFrom int32, docs []db.Document) error {
	var payload []byte
	for _, d := range docs {
		b, err := encodeBSON(d)
		if err != nil {
			return err
		}
		payload = append(payload, b...)
	}
	total := 16 + 4 + 8 + 4 + 4 + len(payload)
	var hb [16]byte
	binary.LittleEndian.PutUint32(hb[0:4], uint32(total))
	binary.LittleEndian.PutUint32(hb[4:8], uint32(responseID))
	binary.LittleEndian.PutUint32(hb[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(hb[12:16], uint32(opReply))
	if _, err := w.Write(hb[:]); err != nil {
		return err
	}
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], 8)
	if _, err := w.Write(b4[:]); err != nil {
		return err
	}
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], uint64(cursorID))
	if _, err := w.Write(b8[:]); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b4[:], uint32(startingFrom))
	if _, err := w.Write(b4[:]); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b4[:], uint32(len(docs)))
	if _, err := w.Write(b4[:]); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return w.Flush()
}
