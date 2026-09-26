package db

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// acquireLock toma un bloqueo de proceso exclusivo junto al archivo de la base de datos.
// Devuelve ErrAlreadyOpen si otro proceso lo tiene.
func acquireLock(dbPath string, lockName string) (*flock.Flock, error) {
	lockPath := filepath.Join(filepath.Dir(dbPath), lockName)
	fl := flock.New(lockPath)
	ok, err := fl.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAlreadyOpen
	}
	return fl, nil
}

// Snapshot escribe el estado actual en dest como un .mlstore totalmente cifrado sin
// cambiar s.path (respaldo R10). Se ejecuta fuera de s.mu, igual que Flush.
func (s *Store) Snapshot(dest string) error {
	if dest == "" {
		return errors.New("db: Snapshot dest required")
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.RLock()
	master := len(s.opts.MasterKey) > 0
	s.mu.RUnlock()
	if !master {
		return errNoMaster
	}
	st, err := s.prepareFlush(dest, false)
	if err != nil || st == nil {
		return err
	}
	return writeState(st)
}

// startAutoFlush lanza un ticker en segundo plano que hace Flush cuando hay datos sucios
// (periodo de Options.AutoFlush, 2s por defecto). Se detiene con Close.
func (s *Store) startAutoFlush() {
	s.mu.RLock()
	path := s.path
	iv := s.opts.autoFlushInterval()
	s.mu.RUnlock()
	if path == "" {
		return
	}
	s.flushStop = make(chan struct{})
	s.flushDone = make(chan struct{})
	go func() {
		defer close(s.flushDone)
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-s.flushStop:
				return
			case <-t.C:
				// Flush solo mantiene s.mu brevemente (instantánea); la E/S queda fuera.
				_ = s.Flush()
			}
		}
	}()
}

func (s *Store) stopAutoFlush() {
	if s.flushStop == nil {
		return
	}
	close(s.flushStop)
	<-s.flushDone
	s.flushStop = nil
	s.flushDone = nil
}

// OpenWithLock es Open + bloqueo de archivo exclusivo + ticker de auto-flush.
// Prefiérelo para el binario MLD (propietario único del proceso).
func OpenWithLock(path string, opts Options) (*Store, error) {
	s, err := Open(path, opts)
	if err != nil {
		return nil, err
	}
	if path != "" {
		lk, lerr := acquireLock(path, opts.lockFileName())
		if lerr != nil {
			_ = s.Close()
			return nil, lerr
		}
		s.lock = lk
	}
	s.startAutoFlush()
	return s, nil
}
