package db

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// sensitiveFields are never exported to CSV (DESIGN §7.1 / análisis D1).
var sensitiveFields = map[string]bool{
	"access_token":  true,
	"refresh_token": true,
	"client_secret": true,
	"password":      true,
	"password_hash": true,
	"secret":        true,
	"api_key":       true,
	"apikey":        true,
	"token":         true,
	"session_token": true,
	"machine_key":   true,
	"master_key":    true,
	"private_key":   true,
}

// SetSensitiveFields declares extra CSV-hidden fields for one collection
// (union with the global blacklist). Replaces any previous list.
func (s *Store) SetSensitiveFields(coll string, fields []string) {
	s.setSensitiveFields(coll, fields)
	_ = s.afterMutation()
}

func (s *Store) setSensitiveFields(coll string, fields []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.coll(coll)
	c.sensitive = append([]string{}, fields...)
	s.markDirty()
}

// SensitiveFields returns the collection-specific sensitive field list.
func (s *Store) SensitiveFields(coll string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.collRO(coll)
	if !ok {
		return nil
	}
	return append([]string{}, c.sensitive...)
}

// ExportCSV writes one row per doc; columns = union of top-level keys
// (first seen order from sorted docs scan is replaced by sorted keys for stability,
// but _id is always first). UTF-8 BOM for Excel. Sensitive fields omitted (R8).
// When ≥1 user exists (M4), raw Store export returns ErrUnauthorized.
func (s *Store) ExportCSV(coll string, w io.Writer) error {
	return s.exportCSV(nil, coll, w)
}

func (s *Store) exportCSV(sess *Session, coll string, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.checkReadLocked(sess, coll); err != nil {
		return err
	}
	hide := make(map[string]bool, len(sensitiveFields)+4)
	for k := range sensitiveFields {
		hide[k] = true
	}
	if sess != nil {
		for f := range sess.fieldDenySet(coll) {
			hide[f] = true
		}
	}
	c, ok := s.collRO(coll)
	if !ok {
		// empty export with just _id header
		_, err := io.WriteString(w, "\xEF\xBB\xBF_id\r\n")
		return err
	}
	for _, f := range c.sensitive {
		hide[f] = true
	}

	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Materialize docs (may load cold entries) and collect columns.
	docs := make([]Document, len(ids))
	for i, id := range ids {
		doc, err := s.resident(c, id)
		if err != nil {
			continue
		}
		docs[i] = doc
	}

	// column set: _id first, then remaining top-level keys sorted
	colSet := map[string]bool{"_id": true}
	var extra []string
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for k := range doc {
			if k == "_id" || hide[k] || colSet[k] {
				continue
			}
			colSet[k] = true
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	cols := append([]string{"_id"}, extra...)

	// UTF-8 BOM
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(cols); err != nil {
		return err
	}
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		row := make([]string, len(cols))
		for i, col := range cols {
			v, ok := doc[col]
			if !ok {
				continue
			}
			row[i] = csvValue(v)
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func csvValue(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// avoid 42 for 42.0 when integral
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	default:
		// nested / arrays → JSON string (RFC 4180 via csv.Writer quoting)
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}

// Migration is a forward-only schema change (DESIGN §7.2).
type Migration struct {
	Version uint64
	Name    string
	Up      func(s *Store) error
}

// ApplyMigrations runs pending migrations in order and persists schema_version.
// A store with newer schema_version than the last migration is rejected
// ("update the program"). Snapshot before migrate is the binary's job.
func (s *Store) ApplyMigrations(migs []Migration) error {
	if err := s.applyMigrations(migs); err != nil {
		return err
	}
	return s.afterMutation()
}

func (s *Store) applyMigrations(migs []Migration) error {
	if len(migs) == 0 {
		return nil
	}
	// sort by version
	sorted := append([]Migration(nil), migs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.schemaVersion
	var maxTarget uint64
	for _, m := range sorted {
		if m.Version > maxTarget {
			maxTarget = m.Version
		}
	}
	if current > maxTarget {
		return fmt.Errorf("db: schema v%d is newer than supported v%d — update the program", current, maxTarget)
	}

	for _, m := range sorted {
		if m.Version <= current {
			continue
		}
		// Run Up outside holding... we hold lock; Up may call Store methods that Lock → deadlock.
		// Document: Up must not call locked Store methods, OR we release lock per migration.
		// Safer: release lock for Up, re-acquire to bump version.
		s.mu.Unlock()
		err := m.Up(s)
		s.mu.Lock()
		if err != nil {
			return fmt.Errorf("db: migration %d (%s): %w", m.Version, m.Name, err)
		}
		if m.Version > s.schemaVersion {
			s.schemaVersion = m.Version
		}
		s.dirty = true
		s.dirtyGen++
		current = s.schemaVersion
	}
	return nil
}
