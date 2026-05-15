// Package storage persists recipes in MySQL. The schema is normalized:
// recipes hold the core post, recipe_ingredients and recipe_steps each get
// their own row per item, and publish_log records every send attempt.
// utf8mb4 throughout so Persian text and emoji round-trip without loss.
package storage

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Status is the lifecycle state of a recipe in the queue.
type Status string

const (
	StatusReady     Status = "ready"     // processed, waiting to be published
	StatusPublished Status = "published" // already sent to the channel
	StatusFailed    Status = "failed"    // publishing failed, won't retry automatically
)

// Ingredient is one normalized row of the recipe's ingredient list.
type Ingredient struct {
	Position  int    // 1-based order in the recipe
	Original  string // spoonacular's english line ("1 cup flour")
	Localized string // persian translation produced by the AI
}

// Step is one normalized row of the recipe's instruction list.
type Step struct {
	Position  int
	Original  string
	Localized string
}

// Recipe is the queue row plus its children. ingredients and steps live in
// their own tables but travel together through this struct.
type Recipe struct {
	ID             int
	OriginalTitle  string
	Title          string
	Intro          string
	Tip            string
	ReadyInMinutes int
	Servings       int
	ImageURL       string
	ImagePath      string
	Content        string // final formatted telegram post
	Status         Status
	ErrorMessage   string
	FetchedAt      time.Time
	PublishedAt    *time.Time
	Ingredients    []Ingredient
	Steps          []Step
}

//go:embed schema.sql
var schemaSQL string

// Storage is a thin handle around *sql.DB plus the small set of queries the
// fetcher and publisher need. The DB driver's own pool gives us concurrency
// safety — no extra mutex required.
type Storage struct {
	db *sql.DB
}

// New opens a connection pool to the recipe_bot database and pings it so a
// bad DSN fails loudly at startup rather than on first query.
func New(dsn string) (*Storage, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &Storage{db: db}, nil
}

// Close releases the underlying pool. Safe to call once at shutdown.
func (s *Storage) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Migrate creates the database (if absent) and applies schema.sql. It opens
// its own connections — serverDSN must point at the server without selecting
// a database, dbDSN at the database we want to populate. Splitting the two
// is the only way to issue CREATE DATABASE before USE.
func Migrate(serverDSN, dbDSN, dbName string) error {
	server, err := sql.Open("mysql", serverDSN)
	if err != nil {
		return fmt.Errorf("open mysql server: %w", err)
	}
	defer server.Close()
	if err := server.Ping(); err != nil {
		return fmt.Errorf("ping mysql server: %w", err)
	}

	createDB := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		dbName,
	)
	if _, err := server.Exec(createDB); err != nil {
		return fmt.Errorf("create database %q: %w", dbName, err)
	}

	db, err := sql.Open("mysql", dbDSN)
	if err != nil {
		return fmt.Errorf("open mysql db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Exists reports whether a recipe id is already in the store, in any status.
// Dedup guard: spoonacular's random endpoint can repeat recipes across calls.
func (s *Storage) Exists(id int) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT 1 FROM recipes WHERE id = ? LIMIT 1", id).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("exists: %w", err)
	}
	return true, nil
}

// IsPublished is the explicit "did we already send this to the channel?"
// check. Equivalent to Exists() + status == published.
func (s *Storage) IsPublished(id int) (bool, error) {
	var status Status
	err := s.db.QueryRow("SELECT status FROM recipes WHERE id = ?", id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is published: %w", err)
	}
	return status == StatusPublished, nil
}

// SaveReady inserts a recipe plus all its ingredients and steps inside one
// transaction — either the whole post lands intact or none of it does, so we
// never end up with a recipe row whose children are half-written.
func (s *Storage) SaveReady(r *Recipe) error {
	if r.FetchedAt.IsZero() {
		r.FetchedAt = time.Now()
	}
	r.Status = StatusReady

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const insertRecipe = `
		INSERT INTO recipes
		    (id, original_title, title, intro, tip, ready_in_minutes, servings,
		     image_url, image_path, formatted_content, status, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insertRecipe,
		r.ID, r.OriginalTitle, r.Title, r.Intro, r.Tip,
		r.ReadyInMinutes, r.Servings, r.ImageURL, r.ImagePath,
		r.Content, r.Status, r.FetchedAt,
	); err != nil {
		return fmt.Errorf("insert recipe %d: %w", r.ID, err)
	}

	if len(r.Ingredients) > 0 {
		const q = `INSERT INTO recipe_ingredients (recipe_id, position, original, localized) VALUES (?, ?, ?, ?)`
		stmt, err := tx.Prepare(q)
		if err != nil {
			return fmt.Errorf("prepare ingredients: %w", err)
		}
		for _, ing := range r.Ingredients {
			if _, err := stmt.Exec(r.ID, ing.Position, ing.Original, ing.Localized); err != nil {
				_ = stmt.Close()
				return fmt.Errorf("insert ingredient: %w", err)
			}
		}
		_ = stmt.Close()
	}

	if len(r.Steps) > 0 {
		const q = `INSERT INTO recipe_steps (recipe_id, position, original, localized) VALUES (?, ?, ?, ?)`
		stmt, err := tx.Prepare(q)
		if err != nil {
			return fmt.Errorf("prepare steps: %w", err)
		}
		for _, st := range r.Steps {
			if _, err := stmt.Exec(r.ID, st.Position, st.Original, st.Localized); err != nil {
				_ = stmt.Close()
				return fmt.Errorf("insert step: %w", err)
			}
		}
		_ = stmt.Close()
	}

	return tx.Commit()
}

// NextReady returns the oldest "ready" recipe (FIFO) with its ingredients and
// steps loaded. Returns sql.ErrNoRows wrapped if the queue is empty.
func (s *Storage) NextReady() (*Recipe, error) {
	const q = `
		SELECT id, original_title, title, intro, tip, ready_in_minutes, servings,
		       image_url, image_path, formatted_content, status, fetched_at, published_at
		FROM recipes
		WHERE status = ?
		ORDER BY fetched_at ASC
		LIMIT 1`

	r := &Recipe{}
	var intro, tip, imageURL, imagePath sql.NullString
	var publishedAt sql.NullTime
	err := s.db.QueryRow(q, StatusReady).Scan(
		&r.ID, &r.OriginalTitle, &r.Title, &intro, &tip,
		&r.ReadyInMinutes, &r.Servings, &imageURL, &imagePath,
		&r.Content, &r.Status, &r.FetchedAt, &publishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("next ready: %w", err)
	}
	r.Intro = intro.String
	r.Tip = tip.String
	r.ImageURL = imageURL.String
	r.ImagePath = imagePath.String
	if publishedAt.Valid {
		t := publishedAt.Time
		r.PublishedAt = &t
	}

	if r.Ingredients, err = s.loadIngredients(r.ID); err != nil {
		return nil, err
	}
	if r.Steps, err = s.loadSteps(r.ID); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Storage) loadIngredients(recipeID int) ([]Ingredient, error) {
	const q = `SELECT position, original, localized FROM recipe_ingredients WHERE recipe_id = ? ORDER BY position`
	rows, err := s.db.Query(q, recipeID)
	if err != nil {
		return nil, fmt.Errorf("load ingredients: %w", err)
	}
	defer rows.Close()
	var out []Ingredient
	for rows.Next() {
		var ing Ingredient
		if err := rows.Scan(&ing.Position, &ing.Original, &ing.Localized); err != nil {
			return nil, fmt.Errorf("scan ingredient: %w", err)
		}
		out = append(out, ing)
	}
	return out, rows.Err()
}

func (s *Storage) loadSteps(recipeID int) ([]Step, error) {
	const q = `SELECT position, original, localized FROM recipe_steps WHERE recipe_id = ? ORDER BY position`
	rows, err := s.db.Query(q, recipeID)
	if err != nil {
		return nil, fmt.Errorf("load steps: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.Position, &st.Original, &st.Localized); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// MarkPublished flips a recipe to "published", stamps the time, and writes a
// success row to publish_log. Both happen in one transaction so the audit
// trail stays consistent with the recipe row.
func (s *Storage) MarkPublished(id int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	if _, err := tx.Exec(
		"UPDATE recipes SET status = ?, published_at = ?, error_message = NULL WHERE id = ?",
		StatusPublished, now, id,
	); err != nil {
		return fmt.Errorf("update published: %w", err)
	}
	if _, err := tx.Exec(
		"INSERT INTO publish_log (recipe_id, attempted_at, success) VALUES (?, ?, 1)",
		id, now,
	); err != nil {
		return fmt.Errorf("log published: %w", err)
	}
	return tx.Commit()
}

// MarkFailed flips a recipe to "failed", records the error, and appends a
// failure row to publish_log. errMsg may be empty.
func (s *Storage) MarkFailed(id int, errMsg string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"UPDATE recipes SET status = ?, error_message = ? WHERE id = ?",
		StatusFailed, nullString(errMsg), id,
	); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	if _, err := tx.Exec(
		"INSERT INTO publish_log (recipe_id, attempted_at, success, error_message) VALUES (?, ?, 0, ?)",
		id, time.Now(), nullString(errMsg),
	); err != nil {
		return fmt.Errorf("log failed: %w", err)
	}
	return tx.Commit()
}

// CountReady returns how many recipes are queued and ready to publish.
func (s *Storage) CountReady() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM recipes WHERE status = ?", StatusReady).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count ready: %w", err)
	}
	return n, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
