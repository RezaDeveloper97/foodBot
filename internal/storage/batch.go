package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BatchEntryStatus is the lifecycle state of a single recipe inside a batch.
type BatchEntryStatus string

const (
	EntryQueued    BatchEntryStatus = "queued"
	EntryProcessed BatchEntryStatus = "processed"
	EntryFailed    BatchEntryStatus = "failed"
)

// Batch states are mirrored straight from the Anthropic API ("in_progress",
// "ended", "canceling", "canceled", "expired") plus our own "failed" when the
// submit call itself didn't succeed. Compared as plain strings.
const (
	BatchStatusInProgress = "in_progress"
	BatchStatusEnded      = "ended"
	BatchStatusCanceled   = "canceled"
	BatchStatusExpired    = "expired"
	BatchStatusFailed     = "failed"
)

// Batch is one submitted batch job. The counts are filled in by the collector
// after the first successful poll.
type Batch struct {
	BatchID        string
	Provider       string
	Status         string
	SubmittedAt    time.Time
	CompletedAt    *time.Time
	PolledAt       *time.Time
	RequestCount   int
	SucceededCount int
	ErroredCount   int
	ResultsURL     string
	ErrorMessage   string
}

// BatchEntry is one recipe inside a batch. SpoonacularJSON is the raw payload
// captured at submit time — the collector deserializes it back to build the
// final post, so a result that arrives hours later doesn't need to re-hit the
// Spoonacular API.
type BatchEntry struct {
	ID              int64
	BatchID         string
	CustomID        string
	SpoonacularID   int
	SpoonacularJSON string
	ImageURL        string
	ImagePath       string
	Status          BatchEntryStatus
	ErrorMessage    string
	CreatedAt       time.Time
	ProcessedAt     *time.Time
}

// CreateBatch inserts a freshly submitted batch plus all its entries inside one
// transaction. SpoonacularJSON for each entry must already be marshaled by the
// caller — the storage layer doesn't know the recipe shape.
func (s *Storage) CreateBatch(b *Batch, entries []*BatchEntry) error {
	if b.SubmittedAt.IsZero() {
		b.SubmittedAt = time.Now()
	}
	if b.Status == "" {
		b.Status = BatchStatusInProgress
	}
	if b.RequestCount == 0 {
		b.RequestCount = len(entries)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO recipe_batches
		  (batch_id, provider, status, submitted_at, request_count)
		 VALUES (?, ?, ?, ?, ?)`,
		b.BatchID, b.Provider, b.Status, b.SubmittedAt, b.RequestCount,
	); err != nil {
		return fmt.Errorf("insert batch %q: %w", b.BatchID, err)
	}

	const q = `
		INSERT INTO recipe_batch_entries
		  (batch_id, custom_id, spoonacular_id, spoonacular_json, image_url, image_path, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	stmt, err := tx.Prepare(q)
	if err != nil {
		return fmt.Errorf("prepare entries: %w", err)
	}
	defer stmt.Close()
	for _, e := range entries {
		if e.Status == "" {
			e.Status = EntryQueued
		}
		if _, err := stmt.Exec(
			b.BatchID, e.CustomID, e.SpoonacularID, e.SpoonacularJSON,
			nullString(e.ImageURL), nullString(e.ImagePath), e.Status,
		); err != nil {
			return fmt.Errorf("insert entry %q: %w", e.CustomID, err)
		}
	}
	return tx.Commit()
}

// ListInProgressBatches returns batches the collector still needs to poll.
// Sorted oldest-first so the very first call after a long sleep handles the
// stalest job first.
func (s *Storage) ListInProgressBatches() ([]*Batch, error) {
	const q = `
		SELECT batch_id, provider, status, submitted_at, completed_at, polled_at,
		       request_count, succeeded_count, errored_count, results_url, error_message
		FROM recipe_batches
		WHERE status = ?
		ORDER BY submitted_at ASC`
	rows, err := s.db.Query(q, BatchStatusInProgress)
	if err != nil {
		return nil, fmt.Errorf("list in-progress batches: %w", err)
	}
	defer rows.Close()
	var out []*Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBatch loads one batch by id. Returns (nil, nil) if it doesn't exist.
func (s *Storage) GetBatch(batchID string) (*Batch, error) {
	const q = `
		SELECT batch_id, provider, status, submitted_at, completed_at, polled_at,
		       request_count, succeeded_count, errored_count, results_url, error_message
		FROM recipe_batches WHERE batch_id = ?`
	row := s.db.QueryRow(q, batchID)
	b, err := scanBatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ListRecentBatches returns the most recent N batches regardless of status —
// powers the -batch-status command so the user can see what's going on.
func (s *Storage) ListRecentBatches(limit int) ([]*Batch, error) {
	if limit <= 0 {
		limit = 20
	}
	const q = `
		SELECT batch_id, provider, status, submitted_at, completed_at, polled_at,
		       request_count, succeeded_count, errored_count, results_url, error_message
		FROM recipe_batches
		ORDER BY submitted_at DESC
		LIMIT ?`
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent batches: %w", err)
	}
	defer rows.Close()
	var out []*Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// rowScanner abstracts *sql.Row and *sql.Rows so the same scan code serves
// both single-row lookups and Query loops.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanBatch(r rowScanner) (*Batch, error) {
	b := &Batch{}
	var completedAt, polledAt sql.NullTime
	var resultsURL, errMsg sql.NullString
	if err := r.Scan(
		&b.BatchID, &b.Provider, &b.Status, &b.SubmittedAt,
		&completedAt, &polledAt,
		&b.RequestCount, &b.SucceededCount, &b.ErroredCount,
		&resultsURL, &errMsg,
	); err != nil {
		return nil, err
	}
	if completedAt.Valid {
		t := completedAt.Time
		b.CompletedAt = &t
	}
	if polledAt.Valid {
		t := polledAt.Time
		b.PolledAt = &t
	}
	b.ResultsURL = resultsURL.String
	b.ErrorMessage = errMsg.String
	return b, nil
}

// GetQueuedEntries returns the entries for a batch that haven't been
// processed yet. Used by the collector after results arrive.
func (s *Storage) GetQueuedEntries(batchID string) ([]*BatchEntry, error) {
	const q = `
		SELECT id, batch_id, custom_id, spoonacular_id, spoonacular_json,
		       image_url, image_path, status, error_message, created_at, processed_at
		FROM recipe_batch_entries
		WHERE batch_id = ? AND status = ?
		ORDER BY id ASC`
	rows, err := s.db.Query(q, batchID, EntryQueued)
	if err != nil {
		return nil, fmt.Errorf("get queued entries: %w", err)
	}
	defer rows.Close()
	var out []*BatchEntry
	for rows.Next() {
		e := &BatchEntry{}
		var imageURL, imagePath, errMsg sql.NullString
		var processedAt sql.NullTime
		if err := rows.Scan(
			&e.ID, &e.BatchID, &e.CustomID, &e.SpoonacularID, &e.SpoonacularJSON,
			&imageURL, &imagePath, &e.Status, &errMsg, &e.CreatedAt, &processedAt,
		); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		e.ImageURL = imageURL.String
		e.ImagePath = imagePath.String
		e.ErrorMessage = errMsg.String
		if processedAt.Valid {
			t := processedAt.Time
			e.ProcessedAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateBatchPoll updates the batch row after a poll: status, completion time
// (if the API said it's done), counts, and the results URL. Always bumps
// polled_at so the operator can see when we last checked.
func (s *Storage) UpdateBatchPoll(
	batchID, status string,
	succeeded, errored int,
	resultsURL string,
	completed *time.Time,
) error {
	now := time.Now()
	_, err := s.db.Exec(
		`UPDATE recipe_batches
		    SET status = ?,
		        succeeded_count = ?,
		        errored_count = ?,
		        results_url = COALESCE(NULLIF(?, ''), results_url),
		        polled_at = ?,
		        completed_at = COALESCE(?, completed_at)
		  WHERE batch_id = ?`,
		status, succeeded, errored, resultsURL, now, completed, batchID,
	)
	if err != nil {
		return fmt.Errorf("update batch %q: %w", batchID, err)
	}
	return nil
}

// MarkEntryProcessed flips an entry to "processed" and stamps the time.
// Called after the collector has successfully written the recipe into the
// main recipes table via SaveReady.
func (s *Storage) MarkEntryProcessed(entryID int64) error {
	_, err := s.db.Exec(
		`UPDATE recipe_batch_entries SET status = ?, processed_at = ?, error_message = NULL WHERE id = ?`,
		EntryProcessed, time.Now(), entryID,
	)
	if err != nil {
		return fmt.Errorf("mark entry processed: %w", err)
	}
	return nil
}

// MarkEntryFailed records why we couldn't turn this entry into a published
// recipe (model errored, JSON parse failed, image download AND text post both
// failed, etc.). The entry stays in the table for audit.
func (s *Storage) MarkEntryFailed(entryID int64, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE recipe_batch_entries SET status = ?, processed_at = ?, error_message = ? WHERE id = ?`,
		EntryFailed, time.Now(), nullString(errMsg), entryID,
	)
	if err != nil {
		return fmt.Errorf("mark entry failed: %w", err)
	}
	return nil
}

// ExistsAnywhere reports whether a spoonacular recipe id is already known —
// either as a final recipe or as a still-queued batch entry. The fetcher uses
// this to avoid submitting the same recipe in two overlapping batches.
func (s *Storage) ExistsAnywhere(spoonacularID int) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT 1 FROM recipes WHERE id = ?
		 UNION
		 SELECT 1 FROM recipe_batch_entries WHERE spoonacular_id = ? AND status = ?
		 LIMIT 1`,
		spoonacularID, spoonacularID, EntryQueued,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("exists anywhere: %w", err)
	}
	return true, nil
}

// CountQueuedEntries returns how many recipes are still waiting on AI results
// across all in-progress batches.
func (s *Storage) CountQueuedEntries() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM recipe_batch_entries WHERE status = ?`,
		EntryQueued,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count queued: %w", err)
	}
	return n, nil
}
