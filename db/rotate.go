package db

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// RotateKeys re-keys the encrypted file at s.path to a new master key and/or
// machine id WITHOUT re-encrypting the payload (D16-style header-only op).
//
// Why it works: the payload is sealed with the per-file DEK, which never
// changes; only the DEK *wrap* depends on the KEK (master ⊕ machineID ⊕ salt).
// So rotation = re-derive KEK with the new material, re-wrap the SAME DEK,
// and rewrite header+wrap bytes while copying the payload ciphertext bytes
// untouched. Cost is O(1) regardless of database size (~µs, one file rewrite
// of the 152B header plus the payload copy by the filesystem).
//
// Safety:
//   - The store must be opened on s.path, clean (Flush first), not closed.
//   - Rotating while dirty would leave the on-disk file stale vs RAM; refuse.
//   - Serialized against Flush/Snapshot via flushMu; readers/writers keep
//     working with the in-RAM DEK, which does not change.
//   - Fresh GCM nonce for the new wrap; new salt avoids nonce reuse of the
//     wrap AEAD (wrapNonce = salt[:12]).
//   - Verified against the OLD KEK first; a wrong current key fails with
//     ErrCorrupt and the file is left untouched.
//   - The rotated file is durably fsynced before returning.
//
// Pass the empty string in a field to keep the current value. The in-RAM
// store adopts the new credentials, so subsequent Flushes keep using them.
//
// Typical uses: provider master-key rotation (plan §3), moving the database
// to another machine (paired with ResolveMachineID), post-compromise rekey.
func (s *Store) RotateKeys(newMaster []byte, newMachineID string) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.RLock()
	path, closed, dirty := s.path, s.closed, s.dirty
	s.mu.RUnlock()
	if path == "" {
		return errors.New("db: RotateKeys requires a file-backed store")
	}
	if closed {
		return errors.New("db: RotateKeys on closed store")
	}
	if dirty {
		return errors.New("db: RotateKeys requires a clean store (Flush first)")
	}

	// Snapshot current crypto material + effective machine id.
	s.mu.RLock()
	curMaster := append([]byte(nil), s.opts.MasterKey...)
	curMachine := append([]byte(nil), s.machine...)
	t, m, p := s.kdfTime, s.kdfMem, s.kdfPar
	dek := append([]byte(nil), s.dek...)
	created := s.created
	schemaVersion := s.schemaVersion
	s.mu.RUnlock()
	if len(curMaster) == 0 || len(dek) == 0 || t == 0 {
		return errors.New("db: RotateKeys on uninitialized crypto material")
	}
	if len(newMaster) == 0 {
		newMaster = curMaster
	}
	if newMachineID == "" {
		newMachineID = string(curMachine)
	}

	// Read, parse and authenticate the current file (old KEK must verify).
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) < headerSize+16 {
		return ErrCorrupt
	}
	h, err := parseHeader(data)
	if err != nil {
		return err
	}
	oldKEK := deriveKEK(curMaster, curMachine, h.KDFSalt[:], t, m, p)
	if !h.verifyHMAC(oldKEK) {
		return ErrCorrupt
	}
	oldDEK, err := openGCM(oldKEK, h.KDFSalt[:12], h.DEKWrapped[:], nil)
	if err != nil || len(oldDEK) != 32 {
		return ErrCorrupt
	}

	// New salt (fresh wrap nonce = salt[:12]; payload nonce stays in header).
	newSalt, err := randomBytes(16)
	if err != nil {
		return err
	}
	newKEK := deriveKEK(newMaster, []byte(newMachineID), newSalt, t, m, p)
	wrapped, err := sealGCM(newKEK, newSalt[:12], oldDEK, nil)
	if err != nil {
		return err
	}
	if len(wrapped) != 48 {
		return fmt.Errorf("db: unexpected wrapped DEK size %d", len(wrapped))
	}

	nh := &header{
		FormatVer:     formatVersion,
		Flags:         h.Flags, // payload bytes are copied verbatim
		SchemaVersion: schemaVersion,
		CreatedAt:     created,
		UpdatedAt:     uint64(time.Now().Unix()),
		KDFTime:       t,
		KDFMemKiB:     m,
		KDFPar:        p,
	}
	copy(nh.KDFSalt[:], newSalt)
	copy(nh.NonceBase[:], h.NonceBase[:]) // payload GCM nonce unchanged
	copy(nh.DEKWrapped[:], wrapped)
	nh.setHMAC(newKEK)

	out := make([]byte, 0, len(data))
	out = append(out, nh.marshalBody()...)
	out = append(out, nh.HeaderHMAC[:]...)
	out = append(out, data[headerSize:]...) // ciphertext copied as-is: O(1) rotate
	if err := atomicReplace(path, out); err != nil {
		return err
	}
	if err := syncPath(path); err != nil {
		return err
	}

	// Adopt new credentials in RAM (opts.MasterKey + machine) so the next
	// Flush re-wraps with the new KEK and Open() from a fresh process agrees.
	// Header rewrite is O(1); body records are DEK-sealed and stay valid.
	s.mu.Lock()
	s.opts.MasterKey = append([]byte(nil), newMaster...)
	s.opts.MachineID = newMachineID
	s.machine = append([]byte(nil), newMachineID...)
	copy(s.kdfSalt[:], newSalt)
	s.kek = append([]byte(nil), newKEK...)
	s.mu.Unlock()
	return nil
}
