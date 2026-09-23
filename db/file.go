package db

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	magicStr = "MLDB"
	// formatVersion 2 = header + append-only record log (M1).
	formatVersion = uint16(2)
	// headerSize: body 120B + HMAC 32B. dek_wrapped = 32B DEK + 16B GCM tag.
	headerSize     = 152
	headerBodySize = 120
	flagCompressed = uint16(1 << 0)

	// Argon2id defaults (stored per-file in header).
	defaultKDFTime = uint32(1)
	defaultKDFMem  = uint32(64 * 1024) // KiB
	defaultKDFPar  = uint32(4)
	// Light params for tests / embedded low-RAM (still recorded in header).
	lightKDFTime = uint32(1)
	lightKDFMem  = uint32(8 * 1024)
	lightKDFPar  = uint32(1)

	// Auto-flush and lock defaults (overridable via Options.AutoFlush/LockFile).
	defaultAutoFlush    = 2 * time.Second
	defaultLockFileName = "mlstoredb.lock"
)

// Options for Open/Flush crypto material.
type Options struct {
	// MasterKey is the provider secret (KEK input). Required for encrypted files.
	MasterKey []byte
	// MachineID mixes into KEK (DESIGN §4.3). Empty = "default".
	// See ResolveMachineID for a per-installation random identity.
	MachineID string
	// LightKDF uses faster Argon2 params (tests). Ignored when reopening existing file.
	LightKDF bool
	// SyncOnWrite makes every successful mutation call FlushSync before
	// returning (durable, slower). Use for tokens/settings; leave false for
	// bulk sync where the auto-flush is enough.
	SyncOnWrite bool
	// AutoFlush is the ticker period for the background flush started by
	// OpenWithLock (dirty data only). Zero or negative = default 2s.
	// Use shorter periods for durability-sensitive caches; longer ones to
	// reduce write amplification on mostly-read workloads.
	AutoFlush time.Duration
	// LockFile is the name of the process lock file created in the database
	// directory by OpenWithLock/Repair. Empty = default "mlstoredb.lock".
	// Give different names to open different databases independently in the
	// same directory (one lock file per database file).
	LockFile string
	// CacheBytes is the resident page-cache budget for cold documents.
	// Zero = default 256 MiB. Negative = unlimited (no eviction).
	CacheBytes int64
	// FindWorkers bounds parallel Find/Count materialization (M3).
	// Zero = GOMAXPROCS. Negative = 1 (fully serial). Positive = n workers.
	FindWorkers int
	// MaxPendingWrites enables write backpressure while a flush is in
	// progress (M3): writers block once (writeSeq-durableSeq) reaches this
	// many mutations, until the flush completes. Zero = unlimited (legacy).
	MaxPendingWrites int
	// SessionTTL bounds sessions from Authenticate (M4). Zero = default 24h.
	// Negative = sessions never expire (until Revoke/ChangePassword/Close).
	SessionTTL time.Duration
}

// findWorkers resolves the parallel Find worker count.
func (o Options) findWorkers() int {
	if o.FindWorkers > 0 {
		return o.FindWorkers
	}
	if o.FindWorkers < 0 {
		return 1
	}
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		return 1
	}
	return n
}

// maxPendingWrites resolves the write-backpressure limit (0 = unlimited).
func (o Options) maxPendingWrites() int {
	if o.MaxPendingWrites > 0 {
		return o.MaxPendingWrites
	}
	return 0
}

// autoFlushInterval resolves the configured auto-flush period.
func (o Options) autoFlushInterval() time.Duration {
	if o.AutoFlush > 0 {
		return o.AutoFlush
	}
	return defaultAutoFlush
}

// lockFileName resolves the process lock file name.
func (o Options) lockFileName() string {
	if o.LockFile != "" {
		return o.LockFile
	}
	return defaultLockFileName
}

// sessionTTL resolves the Authenticate session lifetime (0=default, <0=never).
func (o Options) sessionTTL() time.Duration {
	if o.SessionTTL == 0 {
		return defaultSessionTTL
	}
	return o.SessionTTL
}

var errNoMaster = errors.New("db: MasterKey required")

func (o Options) machineID() []byte {
	if o.MachineID == "" {
		if g := machineGuid(); g != "" {
			return []byte(g)
		}
		return []byte("default")
	}
	return []byte(o.MachineID)
}

// header is the cleartext prefix of the .mlstore file (DESIGN §4.1).
type header struct {
	FormatVer     uint16
	Flags         uint16
	SchemaVersion uint64
	CreatedAt     uint64
	UpdatedAt     uint64
	KDFSalt       [16]byte
	KDFTime       uint32
	KDFMemKiB     uint32
	KDFPar        uint32
	NonceBase     [12]byte
	DEKWrapped    [48]byte // AES-GCM(32B DEK) = 32 + 16 tag
	HeaderHMAC    [32]byte
}

func deriveKEK(master, machineID, salt []byte, t, m, p uint32) []byte {
	h := sha256.New()
	h.Write(master)
	h.Write([]byte{0})
	h.Write(machineID)
	material := h.Sum(nil)
	return argon2.IDKey(material, salt, t, m, uint8(p), 32)
}

func sealGCM(key, nonce, plaintext, additionalData []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, additionalData), nil
}

func openGCM(key, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, additionalData)
}

func (h *header) marshalBody() []byte {
	// bytes [0..headerBodySize) for HMAC (everything before HeaderHMAC)
	buf := make([]byte, headerBodySize)
	copy(buf[0:4], magicStr)
	binary.LittleEndian.PutUint16(buf[4:6], h.FormatVer)
	binary.LittleEndian.PutUint16(buf[6:8], h.Flags)
	binary.LittleEndian.PutUint64(buf[8:16], h.SchemaVersion)
	binary.LittleEndian.PutUint64(buf[16:24], h.CreatedAt)
	binary.LittleEndian.PutUint64(buf[24:32], h.UpdatedAt)
	copy(buf[32:48], h.KDFSalt[:])
	binary.LittleEndian.PutUint32(buf[48:52], h.KDFTime)
	binary.LittleEndian.PutUint32(buf[52:56], h.KDFMemKiB)
	binary.LittleEndian.PutUint32(buf[56:60], h.KDFPar)
	copy(buf[60:72], h.NonceBase[:])
	copy(buf[72:120], h.DEKWrapped[:])
	return buf
}

func (h *header) setHMAC(kek []byte) {
	body := h.marshalBody()
	mac := hmac.New(sha256.New, kek)
	mac.Write(body)
	copy(h.HeaderHMAC[:], mac.Sum(nil))
}

func (h *header) verifyHMAC(kek []byte) bool {
	body := h.marshalBody()
	mac := hmac.New(sha256.New, kek)
	mac.Write(body)
	return hmac.Equal(h.HeaderHMAC[:], mac.Sum(nil))
}

func parseHeader(data []byte) (*header, error) {
	if len(data) < headerSize {
		return nil, ErrCorrupt
	}
	if string(data[0:4]) != magicStr {
		return nil, ErrCorrupt
	}
	h := &header{}
	h.FormatVer = binary.LittleEndian.Uint16(data[4:6])
	if h.FormatVer > formatVersion {
		return nil, fmt.Errorf("db: file format v%d newer than supported v%d — update the program", h.FormatVer, formatVersion)
	}
	if h.FormatVer == 0 {
		return nil, ErrCorrupt
	}
	h.Flags = binary.LittleEndian.Uint16(data[6:8])
	h.SchemaVersion = binary.LittleEndian.Uint64(data[8:16])
	h.CreatedAt = binary.LittleEndian.Uint64(data[16:24])
	h.UpdatedAt = binary.LittleEndian.Uint64(data[24:32])
	copy(h.KDFSalt[:], data[32:48])
	h.KDFTime = binary.LittleEndian.Uint32(data[48:52])
	h.KDFMemKiB = binary.LittleEndian.Uint32(data[52:56])
	h.KDFPar = binary.LittleEndian.Uint32(data[56:60])
	copy(h.NonceBase[:], data[60:72])
	copy(h.DEKWrapped[:], data[72:120])
	copy(h.HeaderHMAC[:], data[120:152])
	return h, nil
}

// fileColl / fileData = plaintext_v1 (DESIGN §4.2) — still used for v1 reads
// and for the eager repair/rebuild path over v2 records.
type fileData struct {
	Meta        fileMeta             `json:"meta"`
	Collections map[string]*fileColl `json:"collections"`
}

type fileMeta struct {
	App           string `json:"app"`
	SchemaVersion uint64 `json:"schema_version"`
}

type fileColl struct {
	Indexes   []IndexInfo `json:"indexes"`
	Sensitive []string    `json:"sensitive,omitempty"`
	Docs      []Document  `json:"docs"`
}

// loadFileData rebuilds collections into the entries model (resident docs).
// Validating load: non-string _id ⇒ ErrNoID; unique conflict ⇒ ErrDuplicate.
func (s *Store) loadFileData(fd *fileData) error {
	s.schemaVersion = fd.Meta.SchemaVersion
	s.collections = map[string]*collection{}
	for name, fc := range fd.Collections {
		c := &collection{
			entries:  map[string]*docEntry{},
			sensitive: append([]string{}, fc.Sensitive...),
		}
		for _, info := range fc.Indexes {
			c.indexes = append(c.indexes, newIndex(info.Fields, info.Unique))
		}
		for _, doc := range fc.Docs {
			id, err := docID(clone(doc))
			if err != nil {
				return err
			}
			stored := clone(doc)
			stored["_id"] = id
			for _, idx := range c.indexes {
				if err := idx.addDoc(stored, id); err != nil {
					return err
				}
			}
			e := newDocEntry(stored)
			s.markEntryResident(e)
			c.entries[id] = e
		}
		s.collections[name] = c
	}
	return nil
}

// markEntryResident accounts a resident payload in the cache registry.
func (s *Store) markEntryResident(e *docEntry) {
	if e.docP.Load() == nil {
		return
	}
	// rough size: pointer payload unknown until measured — use 0 and let
	// loadEntry/cacheAdmit set real sizes for cold loads. For eager loads
	// size stays 0 (no eviction pressure from migration path).
	s.cacheAdmit(e)
}

func compressIfNeeded(plain []byte) (out []byte, compressed bool) {
	var buf bytes.Buffer
	zw := newZlibWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return plain, false
	}
	if err := zw.Close(); err != nil {
		return plain, false
	}
	if buf.Len() < len(plain) {
		return buf.Bytes(), true
	}
	return plain, false
}

func decompressIfNeeded(flags uint16, data []byte) ([]byte, error) {
	if flags&flagCompressed == 0 {
		return data, nil
	}
	return zlibDecompress(data)
}

// atomicReplace writes data to path via tmp + rename (DESIGN §5.4).
func atomicReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Windows: rename over existing can fail; remove dest first as fallback.
	if err := os.Rename(tmpName, path); err != nil {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			os.Remove(tmpName)
			return err
		}
		if err2 := os.Rename(tmpName, path); err2 != nil {
			os.Remove(tmpName)
			return err2
		}
	}
	return nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := readRandom(b); err != nil {
		return nil, err
	}
	return b, nil
}

// authHeader verifies the header and unwraps the DEK (v1 and v2).
// Returns kek, dek, effective machine, kdf params — or ErrCorrupt.
func authHeader(data []byte, opts Options, machine []byte) (*header, []byte, []byte, []byte, uint32, uint32, uint32, error) {
	if len(data) < headerSize {
		return nil, nil, nil, nil, 0, 0, 0, ErrCorrupt
	}
	h, err := parseHeader(data)
	if err != nil {
		return nil, nil, nil, nil, 0, 0, 0, err
	}
	t, m, p := h.KDFTime, h.KDFMemKiB, h.KDFPar
	if t == 0 {
		t, m, p = defaultKDFTime, defaultKDFMem, defaultKDFPar
	}
	effMachine := machine
	kek := deriveKEK(opts.MasterKey, machine, h.KDFSalt[:], t, m, p)
	if !h.verifyHMAC(kek) && opts.MachineID == "" && string(machine) != "default" {
		legacy := []byte("default")
		kek = deriveKEK(opts.MasterKey, legacy, h.KDFSalt[:], t, m, p)
		if h.verifyHMAC(kek) {
			effMachine = legacy
		} else {
			return nil, nil, nil, nil, 0, 0, 0, ErrCorrupt
		}
	} else if !h.verifyHMAC(kek) {
		return nil, nil, nil, nil, 0, 0, 0, ErrCorrupt
	}
	wrapNonce := h.KDFSalt[:12]
	dek, err := openGCM(kek, wrapNonce, h.DEKWrapped[:], nil)
	if err != nil || len(dek) != 32 {
		return nil, nil, nil, nil, 0, 0, 0, ErrCorrupt
	}
	return h, kek, dek, effMachine, t, m, p, nil
}

// decryptFileBytes authenticates and decrypts v1 file bytes into fileData.
// Returns header material, DEK, effective machine id (legacy retry), KDF params.
// Any HMAC / unwrap / GCM / JSON failure ⇒ ErrCorrupt (unrecoverable).
func decryptFileBytes(data []byte, opts Options, machine []byte) (*header, []byte, []byte, uint32, uint32, uint32, *fileData, error) {
	h, _, dek, effMachine, t, m, p, err := authHeader(data, opts, machine)
	if err != nil {
		return nil, nil, nil, 0, 0, 0, nil, err
	}
	if h.FormatVer != 1 {
		return nil, nil, nil, 0, 0, 0, nil, ErrCorrupt
	}
	if len(data) < headerSize+16 {
		return nil, nil, nil, 0, 0, 0, nil, ErrCorrupt
	}
	payload := data[headerSize:]
	plain, err := openGCM(dek, h.NonceBase[:], payload, nil)
	if err != nil {
		return nil, nil, nil, 0, 0, 0, nil, ErrCorrupt
	}
	plain, err = decompressIfNeeded(h.Flags, plain)
	if err != nil {
		return nil, nil, nil, 0, 0, 0, nil, ErrCorrupt
	}
	var fd fileData
	if err := json.Unmarshal(plain, &fd); err != nil {
		return nil, nil, nil, 0, 0, 0, nil, ErrCorrupt
	}
	return h, dek, effMachine, t, m, p, &fd, nil
}

// Open loads an encrypted .mlstore file, or creates a new one if path does not exist.
func Open(path string, opts Options) (*Store, error) {
	if len(opts.MasterKey) == 0 {
		return nil, errors.New("db: Options.MasterKey is required")
	}
	s := New()
	s.path = path
	s.opts = opts
	s.machine = opts.machineID()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// fresh file
		s.dek, err = randomBytes(32)
		if err != nil {
			return nil, err
		}
		s.created = uint64(time.Now().Unix())
		s.dirty = true
		s.ensureTriggersLoaded()
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) < headerSize {
		return nil, ErrCorrupt
	}
	h, err := parseHeader(data)
	if err != nil {
		return nil, err
	}

	if h.FormatVer == 1 {
		h2, dek, effMachine, t, m, p, fd, err := decryptFileBytes(data, opts, s.machine)
		if err != nil {
			return nil, err
		}
		s.machine = effMachine
		if err := s.loadFileData(fd); err != nil {
			return nil, err
		}
		s.dek = dek
		s.created = h2.CreatedAt
		if s.schemaVersion == 0 && fd.Meta.SchemaVersion > 0 {
			s.schemaVersion = fd.Meta.SchemaVersion
		}
		copy(s.kdfSalt[:], h2.KDFSalt[:])
		s.kdfTime, s.kdfMem, s.kdfPar = t, m, p
		s.prepareV1Migration()
		s.ensureTriggersLoaded()
		return s, nil
	}

	// v2 record log
	_, _, dek, effMachine, _, _, _, err := authHeader(data, opts, s.machine)
	if err != nil {
		return nil, err
	}
	s.machine = effMachine
	if err := s.openV2(data, h, dek); err != nil {
		return nil, err
	}
	s.ensureTriggersLoaded()
	return s, nil
}

// Flush serializes, encrypts and writes the record log (no-op if clean).
// Snapshot runs under RLock; compression, crypto and disk I/O run outside
// s.mu so concurrent Find/Get/Insert are not blocked for the full write.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	st, err := s.prepareFlush(s.path, true)
	if err != nil || st == nil {
		return err
	}
	if err := writeState(st); err != nil {
		s.mu.Lock()
		s.flushInProgress = false
		s.mu.Unlock()
		if s.bpCond != nil {
			s.bpCond.Broadcast()
		}
		return err
	}
	if st.clearDirty {
		s.applyFlushed(st)
	}
	return nil
}

// applyFlushed updates store bookkeeping after a successful write of st.
func (s *Store) applyFlushed(st *flushState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushInProgress = false
	s.sawFile = true
	// M3: mutations included in this snapshot are now durable.
	if st.writeSeq > s.durableSeq {
		s.durableSeq = st.writeSeq
	}
	// Clear entry dirty flags / install new offsets from the snapshot.
	for _, fr := range st.flushedEntries {
		c, ok := s.collections[fr.coll]
		if !ok {
			continue
		}
		e, ok := c.entries[fr.id]
		if !ok {
			continue
		}
		e.markEntryClean(st.gen)
	}
	// Full rewrite: install fresh record offsets.
	if st.full {
		for i := range st.ops {
			op := &st.ops[i]
			if op.typ != recDOC {
				continue
			}
			c, ok := s.collections[op.coll]
			if !ok {
				continue
			}
			e, ok := c.entries[op.id]
			if !ok {
				continue
			}
			e.recOff.Store(op.outOff)
			e.recLen.Store(op.outLen)
		}
	} else {
		for i := range st.ops {
			op := &st.ops[i]
			if op.typ != recDOC {
				continue
			}
			c, ok := s.collections[op.coll]
			if !ok {
				continue
			}
			if e, ok := c.entries[op.id]; ok {
				e.recOff.Store(op.outOff)
				e.recLen.Store(op.outLen)
			}
		}
	}
	// Drop committed deletes.
	if st.full {
		s.pendingDels = nil
	} else if len(st.dels) > 0 {
		kept := s.pendingDels[:0]
		for _, d := range s.pendingDels {
			if d.gen > st.gen {
				kept = append(kept, d)
			}
		}
		// copy to fresh slice to release old backing array
		s.pendingDels = append([]delItem(nil), kept...)
	}
	if s.dirtyGen == st.gen {
		s.dirty = false
	}
	if s.bpCond != nil {
		s.bpCond.Broadcast()
	}
}

// syncPath fsyncs path (and best-effort its directory) so a durable Flush
// survives power loss after rename. Directory sync is best-effort (Windows
// often rejects Sync on directories).
func syncPath(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	if dir := filepath.Dir(path); dir != "" {
		if d, derr := os.Open(dir); derr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return closeErr
}

// FlushSync is Flush + fsync of the final file (and best-effort dir).
// Use after critical writes (tokens, settings) when durability before return
// matters more than latency. No-op fsync if the store has no path.
func (s *Store) FlushSync() error {
	if err := s.Flush(); err != nil {
		return err
	}
	s.mu.RLock()
	path := s.path
	closed := s.closed
	s.mu.RUnlock()
	if path == "" || closed {
		return nil
	}
	return syncPath(path)
}

// afterMutation optionally FlushSyncs when Options.SyncOnWrite is set.
// Must not be called while holding s.mu (Flush takes flushMu → prepareFlush → mu).
func (s *Store) afterMutation() error {
	if !s.opts.SyncOnWrite || s.path == "" {
		return nil
	}
	return s.FlushSync()
}

// Close stops auto-flush, flushes (if dirty), releases lock, marks closed.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	// stop ticker first (it takes s.mu)
	s.mu.Unlock()
	s.stopAutoFlush()
	s.stopHookWorker()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	err := s.Flush()
	s.mu.Lock()
	if err == nil {
		s.closed = true
	}
	s.mu.Unlock()
	if s.lock != nil {
		_ = s.lock.Unlock()
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}

// markDirty should be called after every successful mutation when path-backed.
// Caller must hold s.mu (write lock).
func (s *Store) markDirty() {
	if s.path != "" {
		s.dirty = true
		s.dirtyGen++
		s.writeSeq++
	}
}

// markEntryMutated bumps dirtyGen and flags the entry for the next flush.
// Caller must hold s.mu.
func (s *Store) markEntryMutated(e *docEntry) {
	s.markDirty()
	if e != nil {
		e.dirty.Store(true)
		e.mutGen = s.dirtyGen
	}
}
