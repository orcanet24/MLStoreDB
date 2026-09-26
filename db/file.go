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
	// formatVersion 2 = cabecera + log de registros solo-añadir (M1).
	formatVersion = uint16(2)
	// headerSize: cuerpo de 120B + HMAC 32B. dek_wrapped = 32B de DEK + 16B de tag GCM.
	headerSize     = 152
	headerBodySize = 120
	flagCompressed = uint16(1 << 0)

	// Valores por defecto de Argon2id (se guardan por archivo en la cabecera).
	defaultKDFTime = uint32(1)
	defaultKDFMem  = uint32(64 * 1024) // KiB
	defaultKDFPar  = uint32(4)
	// Parámetros ligeros para tests / entornos embebidos con poca RAM (se siguen registrando en la cabecera).
	lightKDFTime = uint32(1)
	lightKDFMem  = uint32(8 * 1024)
	lightKDFPar  = uint32(1)

	// Valores por defecto de auto-flush y bloqueo (sobrescribibles con Options.AutoFlush/LockFile).
	defaultAutoFlush    = 2 * time.Second
	defaultLockFileName = "mlstoredb.lock"
)

// Options contiene el material criptográfico para Open/Flush.
type Options struct {
	// MasterKey es el secreto del proveedor (entrada de la KEK). Obligatoria en archivos cifrados.
	MasterKey []byte
	// MachineID se mezcla en la KEK (DESIGN §4.3). Vacío = "default".
	// Ver ResolveMachineID para obtener una identidad aleatoria por instalación.
	MachineID string
	// LightKDF usa parámetros Argon2 más rápidos (tests). Se ignora al reabrir un archivo existente.
	LightKDF bool
	// SyncOnWrite hace que cada mutación correcta llame a FlushSync antes de
	// retornar (duradero, más lento). Úsalo para tokens/ajustes; déjalo en false
	// para sincronización masiva, donde el auto-flush es suficiente.
	SyncOnWrite bool
	// AutoFlush es el periodo del ticker del flush en segundo plano que inicia
	// OpenWithLock (solo datos sucios). Cero o negativo = 2s por defecto.
	// Usa periodos cortos para cachés sensibles a la durabilidad y largos para
	// reducir la amplificación de escritura en cargas mayoritariamente de lectura.
	AutoFlush time.Duration
	// LockFile es el nombre del archivo de bloqueo de proceso que crean
	// OpenWithLock/Repair en el directorio de la base de datos. Vacío = "mlstoredb.lock".
	// Usa nombres distintos para abrir bases de datos diferentes de forma
	// independiente en el mismo directorio (un archivo de bloqueo por base de datos).
	LockFile string
	// CacheBytes es el presupuesto de caché de páginas residente para documentos fríos.
	// Cero = 256 MiB por defecto. Negativo = sin límite (sin expulsión).
	CacheBytes int64
	// FindWorkers acota la materialización en paralelo de Find/Count (M3).
	// Cero = GOMAXPROCS. Negativo = 1 (totalmente serie). Positivo = n workers.
	FindWorkers int
	// MaxPendingWrites activa la contrapresión de escritura mientras hay un flush
	// en curso (M3): los escritores se bloquean cuando (writeSeq-durableSeq) alcanza
	// ese número de mutaciones, hasta que el flush termina. Cero = sin límite (heredado).
	MaxPendingWrites int
	// SessionTTL acota la vida de las sesiones de Authenticate (M4). Cero = 24h por defecto.
	// Negativo = las sesiones nunca caducan (hasta Revoke/ChangePassword/Close).
	SessionTTL time.Duration
}

// findWorkers resuelve el número de workers paralelos de Find.
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

// maxPendingWrites resuelve el límite de contrapresión de escritura (0 = sin límite).
func (o Options) maxPendingWrites() int {
	if o.MaxPendingWrites > 0 {
		return o.MaxPendingWrites
	}
	return 0
}

// autoFlushInterval resuelve el periodo de auto-flush configurado.
func (o Options) autoFlushInterval() time.Duration {
	if o.AutoFlush > 0 {
		return o.AutoFlush
	}
	return defaultAutoFlush
}

// lockFileName resuelve el nombre del archivo de bloqueo de proceso.
func (o Options) lockFileName() string {
	if o.LockFile != "" {
		return o.LockFile
	}
	return defaultLockFileName
}

// sessionTTL resuelve la vida de las sesiones de Authenticate (0=por defecto, <0=nunca).
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

// header es el prefijo en claro del archivo .mlstore (DESIGN §4.1).
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
	DEKWrapped    [48]byte // AES-GCM(32B de DEK) = 32 + 16 de tag
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
	// bytes [0..headerBodySize) para el HMAC (todo lo anterior a HeaderHMAC)
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

// fileColl / fileData = plaintext_v1 (DESIGN §4.2) — se siguen usando para las
// lecturas v1 y para el camino de reparación/reconstrucción "eager" sobre registros v2.
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

// loadFileData reconstruye las colecciones en el modelo de entradas (documentos residentes).
// Carga con validación: _id no string ⇒ ErrNoID; conflicto de único ⇒ ErrDuplicate.
func (s *Store) loadFileData(fd *fileData) error {
	s.schemaVersion = fd.Meta.SchemaVersion
	s.collections = map[string]*collection{}
	for name, fc := range fd.Collections {
		c := &collection{
			entries:   map[string]*docEntry{},
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

// markEntryResident contabiliza una carga residente en el registro de caché.
func (s *Store) markEntryResident(e *docEntry) {
	if e.docP.Load() == nil {
		return
	}
	// tamaño aproximado: la carga por puntero no se conoce hasta medirla — se usa 0
	// y se deja que loadEntry/cacheAdmit fije los tamaños reales en las cargas frías.
	// En cargas "eager" el tamaño se queda en 0 (sin presión de expulsión desde la migración).
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

// atomicReplace escribe los datos en path mediante tmp + rename (DESIGN §5.4).
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
	// Windows: el rename sobre un archivo existente puede fallar; como respaldo se borra el destino antes.
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

// authHeader verifica la cabecera y desenvuelve la DEK (v1 y v2).
// Devuelve kek, dek, máquina efectiva y parámetros KDF — o ErrCorrupt.
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

// decryptFileBytes autentica y descifra los bytes de un archivo v1 en fileData.
// Devuelve el material de cabecera, la DEK, el id de máquina efectivo (reintento heredado) y los parámetros KDF.
// Cualquier fallo de HMAC / desenvolvimiento / GCM / JSON ⇒ ErrCorrupt (irrecuperable).
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

// Open carga un archivo .mlstore cifrado, o crea uno nuevo si la ruta no existe.
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
		// archivo nuevo
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

	// log de registros v2
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

// Flush serializa, cifra y escribe el log de registros (no hace nada si está limpio).
// La instantánea se toma bajo RLock; la compresión, la criptografía y la E/S de disco
// se ejecutan fuera de s.mu para no bloquear durante toda la escritura a Find/Get/Insert concurrentes.
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

// applyFlushed actualiza la contabilidad del almacén tras escribir st correctamente.
func (s *Store) applyFlushed(st *flushState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushInProgress = false
	s.sawFile = true
	// M3: las mutaciones incluidas en esta instantánea ya son duraderas.
	if st.writeSeq > s.durableSeq {
		s.durableSeq = st.writeSeq
	}
	// Limpiar los flags dirty de las entradas / instalar los nuevos offsets de la instantánea.
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
	// Reescritura completa: instalar offsets de registro nuevos.
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
	// Descartar los borrados ya confirmados.
	if st.full {
		s.pendingDels = nil
	} else if len(st.dels) > 0 {
		kept := s.pendingDels[:0]
		for _, d := range s.pendingDels {
			if d.gen > st.gen {
				kept = append(kept, d)
			}
		}
		// copiar a un slice nuevo para liberar el array de respaldo antiguo
		s.pendingDels = append([]delItem(nil), kept...)
	}
	if s.dirtyGen == st.gen {
		s.dirty = false
	}
	if s.bpCond != nil {
		s.bpCond.Broadcast()
	}
}

// syncPath hace fsync de path (y, con mejor esfuerzo, de su directorio) para que
// un Flush duradero sobreviva a un corte de corriente tras el rename. La sincronización
// del directorio es "best-effort" (Windows suele rechazar Sync sobre directorios).
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

// FlushSync es Flush + fsync del archivo final (y del directorio, con mejor esfuerzo).
// Úsalo tras escrituras críticas (tokens, ajustes) cuando importe más la durabilidad
// antes de retornar que la latencia. El fsync no hace nada si el almacén no tiene path.
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

// afterMutation hace FlushSync opcionalmente cuando Options.SyncOnWrite está activo.
// No debe llamarse con s.mu tomado (Flush toma flushMu → prepareFlush → mu).
func (s *Store) afterMutation() error {
	if !s.opts.SyncOnWrite || s.path == "" {
		return nil
	}
	return s.FlushSync()
}

// Close detiene el auto-flush, hace flush (si está sucio), libera el bloqueo y marca cerrado.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	// detener primero el ticker (toma s.mu)
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

// markDirty debe llamarse tras cada mutación correcta cuando hay respaldo en archivo.
// El llamador debe tener s.mu (bloqueo de escritura).
func (s *Store) markDirty() {
	if s.path != "" {
		s.dirty = true
		s.dirtyGen++
		s.writeSeq++
	}
}

// markEntryMutated incrementa dirtyGen y marca la entrada para el próximo flush.
// El llamador debe tener s.mu.
func (s *Store) markEntryMutated(e *docEntry) {
	s.markDirty()
	if e != nil {
		e.dirty.Store(true)
		e.mutGen = s.dirtyGen
	}
}
