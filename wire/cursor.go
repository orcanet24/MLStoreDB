package wire

import (
	"sync"
	"time"

	"mlstoredb/db"
)

const cursorIdleTTL = 10 * time.Minute

type cursor struct {
	docs   []db.Document
	pos    int
	ns     string
	expiry time.Time
}

type cursorRegistry struct {
	mu   sync.Mutex
	next int64
	m    map[int64]*cursor
}

func newCursorRegistry() *cursorRegistry {
	return &cursorRegistry{
		next: time.Now().UnixNano(),
		m:    make(map[int64]*cursor),
	}
}

func (cr *cursorRegistry) register(docs []db.Document, ns string) int64 {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	id := cr.next
	cr.next++
	cr.m[id] = &cursor{docs: docs, ns: ns, expiry: time.Now().Add(cursorIdleTTL)}
	return id
}

func (cr *cursorRegistry) batch(id int64, n int) (docs []db.Document, ns string, nextID int64, ok bool) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	c, found := cr.m[id]
	if !found {
		return nil, "", 0, false
	}
	if time.Now().After(c.expiry) {
		delete(cr.m, id)
		return nil, "", 0, false
	}
	if n <= 0 {
		n = 101
	}
	end := c.pos + n
	if end > len(c.docs) {
		end = len(c.docs)
	}
	docs = c.docs[c.pos:end]
	c.pos = end
	c.expiry = time.Now().Add(cursorIdleTTL)
	if c.pos >= len(c.docs) {
		delete(cr.m, id)
		return docs, c.ns, 0, true
	}
	return docs, c.ns, id, true
}

func (cr *cursorRegistry) kill(ids []int64) (killed, notFound []int64) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	for _, id := range ids {
		if _, ok := cr.m[id]; ok {
			delete(cr.m, id)
			killed = append(killed, id)
		} else {
			notFound = append(notFound, id)
		}
	}
	return killed, notFound
}

func (cr *cursorRegistry) sweep() {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	now := time.Now()
	for id, c := range cr.m {
		if now.After(c.expiry) {
			delete(cr.m, id)
		}
	}
}
