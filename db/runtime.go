package db

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// acquireLock takes an exclusive process lock next to the db file.
// Returns ErrAlreadyOpen if another process holds it.
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

// Snapshot writes the current state to dest as a fully encrypted .mlstore
// without changing s.path (backup R10). Runs outside s.mu like Flush.
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

// startAutoFlush spawns a background ticker that Flushes when dirty
// (period from Options.AutoFlush, default 2s). Stop is via Close.
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
				// Flush only briefly holds s.mu (snapshot); I/O is outside.
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

// OpenWithLock is Open + exclusive file lock + auto-flush ticker.
// Prefer this for the MLD binary (single process owner).
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
