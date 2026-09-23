package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// InstallIDFileName is the per-installation identity file. It lives NEXT TO
// the database file (never inside it) and holds a random ULID generated on
// first use. Feeding it as Options.MachineID binds the file's KEK to this
// installation only, so a copied .mlstore cannot be decrypted elsewhere even
// if the master key material is available.
const InstallIDFileName = "mlstoredb.machineid"

const maxMachineIDLen = 256

// validMachineID accepts letters, digits and - _ . (canonical ULIDs qualify).
func validMachineID(s string) bool {
	if s == "" || len(s) > maxMachineIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func installIDPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), InstallIDFileName)
}

// ResolveMachineID returns the machine identity to use as Options.MachineID
// for the database at dbPath:
//
//  1. explicit non-empty → validated and returned as-is (tests / explicit
//     override; the install-id file is neither read nor written).
//  2. otherwise the install-id file next to dbPath: if it exists it is
//     validated and returned (stable across restarts and updates); if not,
//     a fresh random ULID is generated, persisted atomically and returned.
//
// Call it once at installation/setup time, pass the result as
// Options.MachineID on every later Open/OpenWithLock of that database, and
// register the value with the license server (it is both the per-customer
// binding and the recovery backup if the identity file is ever lost).
//
// Existing databases created with the default MachineGuid binding keep
// opening with Options.MachineID empty — do not switch an existing file to
// an install id without a key-rotation/rewrite plan.
//
// Several database files in the same directory intentionally share one
// installation identity (one identity per data directory).
func ResolveMachineID(dbPath, explicit string) (string, error) {
	if explicit != "" {
		if !validMachineID(explicit) {
			return "", errors.New("db: invalid explicit MachineID (allowed: letters, digits, - _ .)")
		}
		return explicit, nil
	}
	p := installIDPath(dbPath)
	data, err := os.ReadFile(p)
	if err == nil {
		id := strings.TrimSpace(string(data))
		switch {
		case id == "":
			// Empty can mean (a) a concurrent creator still between O_EXCL
			// create and WriteString, or (b) a truncated/corrupt file.
			// Retry briefly for (a); re-read after create→write settles.
			for attempt := 0; attempt < 8; attempt++ {
				time.Sleep(2 * time.Millisecond)
				data, err = os.ReadFile(p)
				if err != nil {
					break
				}
				id = strings.TrimSpace(string(data))
				if id != "" {
					break
				}
			}
			if id == "" {
				return "", errors.New("db: install id file is empty: " + p)
			}
			if !validMachineID(id) {
				return "", errors.New("db: install id file has invalid characters: " + p)
			}
			return id, nil
		case !validMachineID(id):
			return "", errors.New("db: install id file has invalid characters: " + p)
		}
		return id, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// First use for this installation: create exclusively so exactly one
	// identity is ever generated, even with concurrent creators. Losers adopt
	// the winner's id (a short read loop covers the create→write window).
	id := ulid.Make().String()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, werr := f.WriteString(id + "\n")
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return "", errors.New("db: cannot write install id file: " + p)
		}
		return id, nil
	}
	if os.IsExist(err) {
		for attempt := 0; attempt < 5; attempt++ {
			data, rerr := os.ReadFile(p)
			if rerr == nil {
				if v := strings.TrimSpace(string(data)); validMachineID(v) {
					return v, nil
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		return "", errors.New("db: install id file created but unreadable: " + p)
	}
	// Filesystem without O_EXCL support: last-resort replace + converge loop.
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := atomicReplace(p, []byte(id+"\n")); err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
			continue
		}
		if data, rerr := os.ReadFile(p); rerr == nil {
			if v := strings.TrimSpace(string(data)); validMachineID(v) {
				return v, nil
			}
		}
		return id, nil
	}
	return "", lastErr
}
