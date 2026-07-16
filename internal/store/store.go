// Package store remembers which postings have already been processed so a
// job is evaluated — and emailed — only once. State is a plain JSON file:
// safe to inspect, and deleting it simply re-processes everything.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Record is what we keep about one posting we've seen. A record with
// Matched=true and Notified=false is a match whose delivery hasn't been
// confirmed yet — the runner retries it on the next cycle.
type Record struct {
	FirstSeen time.Time `json:"first_seen"`
	Title     string    `json:"title"`
	Matched   bool      `json:"matched"`
	Notified  bool      `json:"notified"`
}

// Store is a JSON-file-backed set of seen postings, keyed by model.Job.ID.
// Opening takes an exclusive lock so concurrent jobwatch processes can't
// double-notify or clobber each other. It is not safe for concurrent use
// within a process; the runner accesses it from one goroutine only.
type Store struct {
	path    string
	recs    map[string]Record
	release func()
}

// Open loads the store at path, creating parent directories as needed.
// A missing file is an empty store, not an error.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	release, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}

	s := &Store{path: path, recs: map[string]Record{}, release: release}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		release()
		return nil, err
	}
	if err := json.Unmarshal(data, &s.recs); err != nil {
		release()
		return nil, fmt.Errorf("state file %s is corrupt (delete it to start fresh): %w", path, err)
	}
	return s, nil
}

// Close releases the inter-process lock. (Exiting the process releases it
// too; Close exists for tidiness and tests.)
func (s *Store) Close() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

// Get returns the record for a posting and whether one exists.
func (s *Store) Get(id string) (Record, bool) {
	r, ok := s.recs[id]
	return r, ok
}

// Seen reports whether the posting was processed in an earlier run.
func (s *Store) Seen(id string) bool {
	_, ok := s.recs[id]
	return ok
}

// Add records a posting. It overwrites any previous record.
func (s *Store) Add(id string, r Record) { s.recs[id] = r }

// Len returns the number of recorded postings.
func (s *Store) Len() int { return len(s.recs) }

// Save writes the store atomically: unique temp file, fsync, rename, then
// a best-effort directory fsync — so neither a crash mid-write nor power
// loss right after can leave a truncated state file behind.
func (s *Store) Save() error {
	data, err := json.MarshalIndent(s.recs, "", " ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best-effort: makes the rename itself durable
		d.Close()
	}
	return nil
}
