package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Status is the lifecycle state of a recipe in the queue.
type Status string

const (
	StatusReady     Status = "ready"     // processed, waiting to be published
	StatusPublished Status = "published" // already sent to the channel
	StatusFailed    Status = "failed"    // publishing failed, won't retry automatically
)

// Recipe is one item in the queue: the AI-processed post plus metadata.
type Recipe struct {
	ID          int       `json:"id"` // spoonacular recipe id — used for de-duplication
	Title       string    `json:"title"`
	Content     string    `json:"content"`    // final, ready-to-post Persian text
	ImagePath   string    `json:"image_path"` // local path to the downloaded image ("" if none)
	Status      Status    `json:"status"`
	FetchedAt   time.Time `json:"fetched_at"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

// Storage is a tiny concurrency-safe JSON-file store. It is intentionally
// simple — for this workload (a few posts a day) a file is plenty. The method
// set is small enough that swapping in SQLite/Postgres later is straightforward.
type Storage struct {
	path string
	mu   sync.Mutex
	data map[int]*Recipe
}

// New opens (or creates) the store at path.
func New(path string) (*Storage, error) {
	s := &Storage{path: path, data: make(map[int]*Recipe)}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Storage) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var list []*Recipe
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("parse store file: %w", err)
	}
	for _, r := range list {
		s.data[r.ID] = r
	}
	return nil
}

// persist writes the whole store atomically (write temp, then rename).
// Callers must already hold s.mu.
func (s *Storage) persist() error {
	list := make([]*Recipe, 0, len(s.data))
	for _, r := range s.data {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].FetchedAt.Before(list[j].FetchedAt)
	})
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Exists reports whether a recipe id is already in the store. This is the
// de-duplication guard — spoonacular's random endpoint can repeat recipes.
func (s *Storage) Exists(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[id]
	return ok
}

// SaveReady stores a freshly processed recipe with status "ready".
func (s *Storage) SaveReady(r *Recipe) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Status = StatusReady
	if r.FetchedAt.IsZero() {
		r.FetchedAt = time.Now()
	}
	s.data[r.ID] = r
	return s.persist()
}

// NextReady returns a copy of the oldest "ready" recipe (FIFO).
func (s *Storage) NextReady() (*Recipe, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var candidates []*Recipe
	for _, r := range s.data {
		if r.Status == StatusReady {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].FetchedAt.Before(candidates[j].FetchedAt)
	})
	cp := *candidates[0]
	return &cp, true
}

// MarkPublished flips a recipe to "published" and stamps the time.
func (s *Storage) MarkPublished(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[id]
	if !ok {
		return fmt.Errorf("recipe %d not found", id)
	}
	r.Status = StatusPublished
	r.PublishedAt = time.Now()
	return s.persist()
}

// MarkFailed flips a recipe to "failed" so it is not picked again.
func (s *Storage) MarkFailed(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[id]
	if !ok {
		return fmt.Errorf("recipe %d not found", id)
	}
	r.Status = StatusFailed
	return s.persist()
}

// CountReady returns how many recipes are queued and ready to publish.
func (s *Storage) CountReady() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.data {
		if r.Status == StatusReady {
			n++
		}
	}
	return n
}
