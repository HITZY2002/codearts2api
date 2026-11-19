// Package telemetry stores operational metadata in SQLite.
//
// The store is intentionally metadata-only: request bodies, response bodies,
// prompts, completions, authorization headers and API keys are never stored.
package telemetry

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// RequestLog contains only request metadata, never request or response content.
type RequestLog struct {
	ID                      int64    `json:"id"`
	RequestID               string   `json:"request_id"`
	CreatedAt               int64    `json:"created_at"`
	Endpoint                string   `json:"endpoint"`
	Method                  string   `json:"method"`
	ClientIP                string   `json:"client_ip"`
	StatusCode              int      `json:"status_code"`
	Stream                  bool     `json:"stream"`
	Model                   string   `json:"model"`
	AccountUID              string   `json:"account_uid"`
	AccountName             string   `json:"account_name"`
	Attempts                int      `json:"attempts"`
	QueueWaitMs             int64    `json:"queue_wait_ms"`
	FirstOutputMs           *int64   `json:"first_output_ms"`
	TotalMs                 int64    `json:"total_ms"`
	PromptTokens            int64    `json:"prompt_tokens"`
	CompletionTokens        int64    `json:"completion_tokens"`
	TotalTokens             int64    `json:"total_tokens"`
	UsageEstimated          bool     `json:"usage_estimated"`
	CreditCost              *float64 `json:"credit_cost"`
	BillingPromptTokens     int64    `json:"billing_prompt_tokens"`
	BillingCompletionTokens int64    `json:"billing_completion_tokens"`
	BillingCachedTokens     int64    `json:"billing_cached_tokens"`
	BillingTotalTokens      int64    `json:"billing_total_tokens"`
	BillingRequestCount     int64    `json:"billing_request_count"`
	FinishReason            string   `json:"finish_reason"`
	ErrorType               string   `json:"error_type"`
}

// EventLog is a short operational event. Data may contain sanitized metadata.
type EventLog struct {
	ID         int64           `json:"id"`
	CreatedAt  int64           `json:"created_at"`
	Kind       string          `json:"kind"`
	Level      string          `json:"level"`
	AccountUID string          `json:"account_uid"`
	Model      string          `json:"model"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// AccountSnapshot records pool state at a point in time.
type AccountSnapshot struct {
	ID               int64  `json:"id"`
	CreatedAt        int64  `json:"created_at"`
	AccountUID       string `json:"account_uid"`
	AccountName      string `json:"account_name"`
	Status           string `json:"status"`
	ActiveConcurrent int    `json:"active_concurrent"`
	MaxConcurrent    int    `json:"max_concurrent"`
	ErrCount         int    `json:"err_count"`
	TokenExpiresAt   string `json:"token_expires_at"`
}

type operation struct {
	fn func() error
}

// Store is a SQLite-backed asynchronous telemetry writer.
type Store struct {
	db     *sql.DB
	path   string
	queue  chan operation
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

const schema = `
CREATE TABLE IF NOT EXISTS request_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	endpoint TEXT NOT NULL,
	method TEXT NOT NULL,
	client_ip TEXT NOT NULL DEFAULT '',
	status_code INTEGER NOT NULL DEFAULT 0,
	stream INTEGER NOT NULL DEFAULT 0,
	model TEXT NOT NULL DEFAULT '',
	account_uid TEXT NOT NULL DEFAULT '',
	account_name TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	queue_wait_ms INTEGER NOT NULL DEFAULT 0,
	first_output_ms INTEGER,
	total_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	usage_estimated INTEGER NOT NULL DEFAULT 0,
	credit_cost REAL,
	billing_prompt_tokens INTEGER NOT NULL DEFAULT 0,
	billing_completion_tokens INTEGER NOT NULL DEFAULT 0,
	billing_cached_tokens INTEGER NOT NULL DEFAULT 0,
	billing_total_tokens INTEGER NOT NULL DEFAULT 0,
	billing_request_count INTEGER NOT NULL DEFAULT 0,
	finish_reason TEXT NOT NULL DEFAULT '',
	error_type TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_request_logs_request_id ON request_logs(request_id);
CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);
CREATE INDEX IF NOT EXISTS idx_request_logs_account ON request_logs(account_uid);
CREATE INDEX IF NOT EXISTS idx_request_logs_status ON request_logs(status_code);

CREATE TABLE IF NOT EXISTS event_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	level TEXT NOT NULL DEFAULT 'info',
	account_uid TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	data TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_event_logs_created_at ON event_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_event_logs_kind ON event_logs(kind);
CREATE INDEX IF NOT EXISTS idx_event_logs_account ON event_logs(account_uid);

CREATE TABLE IF NOT EXISTS account_snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at INTEGER NOT NULL,
	account_uid TEXT NOT NULL,
	account_name TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	active_concurrent INTEGER NOT NULL DEFAULT 0,
	max_concurrent INTEGER NOT NULL DEFAULT 0,
	err_count INTEGER NOT NULL DEFAULT 0,
	token_expires_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_account_snapshots_created_at ON account_snapshots(created_at);
CREATE INDEX IF NOT EXISTS idx_account_snapshots_account ON account_snapshots(account_uid);
`

// Open creates the SQLite database and starts the async write worker.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("telemetry sqlite path is empty")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create telemetry directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	s := &Store{
		db:    db,
		path:  path,
		queue: make(chan operation, 4096),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.worker()
	return s, nil
}

func (s *Store) worker() {
	for {
		select {
		case op := <-s.queue:
			_ = op.fn()
		case <-s.stop:
			close(s.done)
			return
		}
	}
}

func (s *Store) enqueue(op operation) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("telemetry store is closed")
	}
	select {
	case s.queue <- op:
		return nil
	default:
		return fmt.Errorf("telemetry queue is full")
	}
}

// Flush waits for all previously queued writes to complete.
func (s *Store) Flush() error {
	done := make(chan struct{})
	err := s.enqueue(operation{fn: func() error {
		close(done)
		return nil
	}})
	if err != nil {
		return err
	}
	<-done
	return nil
}

// Close stops the writer and closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.Flush()
	close(s.stop)
	<-s.done
	return s.db.Close()
}

// RecordRequest queues a request metadata row for asynchronous insertion.
func (s *Store) RecordRequest(req RequestLog) error {
	return s.enqueue(operation{fn: func() error { return s.insertRequest(req) }})
}

func (s *Store) insertRequest(req RequestLog) error {
	stream := 0
	if req.Stream {
		stream = 1
	}
	usageEstimated := 0
	if req.UsageEstimated {
		usageEstimated = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO request_logs
		(request_id, created_at, endpoint, method, client_ip, status_code, stream, model,
		 account_uid, account_name, attempts, queue_wait_ms, first_output_ms, total_ms,
		 prompt_tokens, completion_tokens, total_tokens, usage_estimated, credit_cost,
		 billing_prompt_tokens, billing_completion_tokens, billing_cached_tokens,
		 billing_total_tokens, billing_request_count, finish_reason, error_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.RequestID, req.CreatedAt, req.Endpoint, req.Method, req.ClientIP, req.StatusCode,
		stream, req.Model, req.AccountUID, req.AccountName, req.Attempts, req.QueueWaitMs,
		req.FirstOutputMs, req.TotalMs, req.PromptTokens, req.CompletionTokens,
		req.TotalTokens, usageEstimated, req.CreditCost, req.BillingPromptTokens,
		req.BillingCompletionTokens, req.BillingCachedTokens, req.BillingTotalTokens,
		req.BillingRequestCount, req.FinishReason, req.ErrorType,
	)
	return err
}

// RecordEvent queues a sanitized operational event.
func (s *Store) RecordEvent(kind, level, accountUID, model, message string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.enqueue(operation{fn: func() error {
		_, err := s.db.Exec(
			`INSERT INTO event_logs (created_at, kind, level, account_uid, model, message, data)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			time.Now().UnixMilli(), kind, level, accountUID, model, message, string(raw),
		)
		return err
	}})
}

// RecordSnapshots queues one periodic account-state sample.
func (s *Store) RecordSnapshots(snapshots []AccountSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	return s.enqueue(operation{fn: func() error {
		now := time.Now().UnixMilli()
		for _, snap := range snapshots {
			if _, err := s.db.Exec(
				`INSERT INTO account_snapshots
				 (created_at, account_uid, account_name, status, active_concurrent,
				  max_concurrent, err_count, token_expires_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				now, snap.AccountUID, snap.AccountName, snap.Status,
				snap.ActiveConcurrent, snap.MaxConcurrent, snap.ErrCount, snap.TokenExpiresAt,
			); err != nil {
				return err
			}
		}
		return nil
	}})
}
