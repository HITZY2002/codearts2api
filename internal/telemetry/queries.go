package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RequestFilter selects request metadata rows for the dashboard.
type RequestFilter struct {
	From       int64
	To         int64
	StatusCode int
	Stream     *bool
	Model      string
	AccountUID string
	ErrorType  string
	Limit      int
	Offset     int
}

// EventFilter selects operational events for the dashboard.
type EventFilter struct {
	From       int64
	To         int64
	Kind       string
	Level      string
	AccountUID string
	Model      string
	Limit      int
	Offset     int
}

// MetricsOverview summarizes requests in a time range.
type MetricsOverview struct {
	TotalRequests    int64   `json:"total_requests"`
	SuccessCount     int64   `json:"success_count"`
	ErrorCount       int64   `json:"error_count"`
	SuccessRate      float64 `json:"success_rate"`
	TTFTP50Ms        int64   `json:"ttft_p50_ms"`
	TTFTP95Ms        int64   `json:"ttft_p95_ms"`
	TTFTP99Ms        int64   `json:"ttft_p99_ms"`
	TotalP50Ms       int64   `json:"total_p50_ms"`
	TotalP95Ms       int64   `json:"total_p95_ms"`
	TotalP99Ms       int64   `json:"total_p99_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
}

// MetricsSeriesPoint aggregates requests into a time bucket.
type MetricsSeriesPoint struct {
	Bucket       int64   `json:"bucket"`
	RequestCount int64   `json:"request_count"`
	SuccessCount int64   `json:"success_count"`
	ErrorCount   int64   `json:"error_count"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgTTFTMs    float64 `json:"avg_ttft_ms"`
	AvgTotalMs   float64 `json:"avg_total_ms"`
}

func requestWhere(filter RequestFilter) (string, []any) {
	clauses := []string{}
	args := []any{}
	if filter.From > 0 {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, filter.From)
	}
	if filter.To > 0 {
		clauses = append(clauses, "created_at <= ?")
		args = append(args, filter.To)
	}
	if filter.StatusCode > 0 {
		clauses = append(clauses, "status_code = ?")
		args = append(args, filter.StatusCode)
	}
	if filter.Stream != nil {
		clauses = append(clauses, "stream = ?")
		value := 0
		if *filter.Stream {
			value = 1
		}
		args = append(args, value)
	}
	if filter.Model != "" {
		clauses = append(clauses, "LOWER(model) = ?")
		args = append(args, strings.ToLower(filter.Model))
	}
	if filter.AccountUID != "" {
		clauses = append(clauses, "account_uid = ?")
		args = append(args, filter.AccountUID)
	}
	if filter.ErrorType != "" {
		clauses = append(clauses, "LOWER(error_type) = ?")
		args = append(args, strings.ToLower(filter.ErrorType))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func eventWhere(filter EventFilter) (string, []any) {
	clauses := []string{}
	args := []any{}
	if filter.From > 0 {
		clauses = append(clauses, "created_at >= ?")
		args = append(args, filter.From)
	}
	if filter.To > 0 {
		clauses = append(clauses, "created_at <= ?")
		args = append(args, filter.To)
	}
	if filter.Kind != "" {
		clauses = append(clauses, "LOWER(kind) = ?")
		args = append(args, strings.ToLower(filter.Kind))
	}
	if filter.Level != "" {
		clauses = append(clauses, "LOWER(level) = ?")
		args = append(args, strings.ToLower(filter.Level))
	}
	if filter.AccountUID != "" {
		clauses = append(clauses, "account_uid = ?")
		args = append(args, filter.AccountUID)
	}
	if filter.Model != "" {
		clauses = append(clauses, "LOWER(model) = ?")
		args = append(args, strings.ToLower(filter.Model))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func normalizePagination(limit, offset, maxLimit int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ListRequests returns a page of request metadata rows.
func (s *Store) ListRequests(filter RequestFilter) ([]RequestLog, int64, error) {
	where, args := requestWhere(filter)
	var total int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM request_logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := normalizePagination(filter.Limit, filter.Offset, 500)
	query := `SELECT id, request_id, chat_id, upstream_msg_id, created_at, endpoint, method, client_ip, status_code,
		stream, model, account_uid, account_name, attempts, queue_wait_ms, first_output_ms,
		total_ms, prompt_tokens, completion_tokens, total_tokens, usage_estimated, credit_cost,
		billing_prompt_tokens, billing_completion_tokens, billing_cached_tokens,
		billing_total_tokens, billing_request_count, finish_reason, error_type
		FROM request_logs` + where + " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]RequestLog, 0, limit)
	for rows.Next() {
		var req RequestLog
		var stream, usageEstimated int
		if err := rows.Scan(
			&req.ID, &req.RequestID, &req.ChatID, &req.UpstreamMsgID, &req.CreatedAt, &req.Endpoint, &req.Method,
			&req.ClientIP, &req.StatusCode, &stream, &req.Model, &req.AccountUID,
			&req.AccountName, &req.Attempts, &req.QueueWaitMs, &req.FirstOutputMs,
			&req.TotalMs, &req.PromptTokens, &req.CompletionTokens, &req.TotalTokens,
			&usageEstimated, &req.CreditCost, &req.BillingPromptTokens,
			&req.BillingCompletionTokens, &req.BillingCachedTokens,
			&req.BillingTotalTokens, &req.BillingRequestCount,
			&req.FinishReason, &req.ErrorType,
		); err != nil {
			return nil, 0, err
		}
		req.Stream = stream == 1
		req.UsageEstimated = usageEstimated == 1
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListEvents returns a page of operational events.
func (s *Store) ListEvents(filter EventFilter) ([]EventLog, int64, error) {
	where, args := eventWhere(filter)
	var total int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM event_logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := normalizePagination(filter.Limit, filter.Offset, 500)
	query := `SELECT id, created_at, kind, level, account_uid, model, message, data
		FROM event_logs` + where + " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]EventLog, 0, limit)
	for rows.Next() {
		var event EventLog
		var data string
		if err := rows.Scan(
			&event.ID, &event.CreatedAt, &event.Kind, &event.Level, &event.AccountUID,
			&event.Model, &event.Message, &data,
		); err != nil {
			return nil, 0, err
		}
		event.Data = json.RawMessage(data)
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// MetricsOverview computes dashboard summary statistics.
func (s *Store) MetricsOverview(from, to int64) (MetricsOverview, error) {
	where, args := requestWhere(RequestFilter{From: from, To: to})
	var out MetricsOverview
	err := s.db.QueryRow(
		`SELECT COUNT(*),
		 COALESCE(SUM(CASE WHEN status_code < 400 THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		 COALESCE(SUM(prompt_tokens), 0),
		 COALESCE(SUM(completion_tokens), 0),
		 COALESCE(SUM(total_tokens), 0)
		 FROM request_logs`+where, args...,
	).Scan(&out.TotalRequests, &out.SuccessCount, &out.ErrorCount,
		&out.PromptTokens, &out.CompletionTokens, &out.TotalTokens)
	if err != nil {
		return out, err
	}
	if out.TotalRequests > 0 {
		out.SuccessRate = float64(out.SuccessCount) / float64(out.TotalRequests)
	}
	if err := s.scanDurations("first_output_ms", from, to, &out.TTFTP50Ms, &out.TTFTP95Ms, &out.TTFTP99Ms); err != nil {
		return out, err
	}
	if err := s.scanDurations("total_ms", from, to, &out.TotalP50Ms, &out.TotalP95Ms, &out.TotalP99Ms); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) scanDurations(column string, from, to int64, p50, p95, p99 *int64) error {
	query := fmt.Sprintf(
		"SELECT %s FROM request_logs WHERE %s IS NOT NULL AND created_at >= ? AND created_at <= ? ORDER BY %s",
		column, column, column,
	)
	rows, err := s.db.Query(query, from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	values := make([]int64, 0)
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	*p50 = percentile(values, 0.50)
	*p95 = percentile(values, 0.95)
	*p99 = percentile(values, 0.99)
	return nil
}

func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * p)
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// MetricsSeries aggregates request metadata into fixed time buckets.
func (s *Store) MetricsSeries(from, to int64, interval string) ([]MetricsSeriesPoint, error) {
	intervalMs := intervalMilliseconds(interval)
	where, args := requestWhere(RequestFilter{From: from, To: to})
	query := `SELECT
		CAST(created_at / ? AS INTEGER) * ?,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status_code < 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(AVG(CASE WHEN first_output_ms IS NOT NULL THEN first_output_ms END), 0),
		COALESCE(AVG(total_ms), 0)
		FROM request_logs` + where + " GROUP BY 1 ORDER BY 1"
	boundArgs := append([]any{intervalMs, intervalMs}, args...)
	rows, err := s.db.Query(query, boundArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MetricsSeriesPoint, 0)
	for rows.Next() {
		var point MetricsSeriesPoint
		if err := rows.Scan(
			&point.Bucket, &point.RequestCount, &point.SuccessCount, &point.ErrorCount,
			&point.TotalTokens, &point.AvgTTFTMs, &point.AvgTotalMs,
		); err != nil {
			return nil, err
		}
		out = append(out, point)
	}
	return out, rows.Err()
}

func intervalMilliseconds(interval string) int64 {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "minute":
		return int64(time.Minute / time.Millisecond)
	case "hour":
		return int64(time.Hour / time.Millisecond)
	case "day":
		return int64(24 * time.Hour / time.Millisecond)
	default:
		return int64(5 * time.Minute / time.Millisecond)
	}
}

// PurgeStats reports how many rows a cleanup removed.
type PurgeStats struct {
	Requests  int64 `json:"requests"`
	Events    int64 `json:"events"`
	Snapshots int64 `json:"snapshots"`
}

func (s *Store) tableSize(table string, maxRows int) error {
	var count int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		return err
	}
	if count <= int64(maxRows) {
		return nil
	}
	deleteCount := count - int64(maxRows)
	_, err := s.db.Exec(
		"DELETE FROM "+table+" WHERE id IN (SELECT id FROM "+table+" ORDER BY id ASC LIMIT ?)",
		deleteCount,
	)
	return err
}

func (s *Store) databaseBytes() (int64, error) {
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(FULL)"); err != nil {
		return 0, err
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Purge applies retention limits and returns deletion counts.
func (s *Store) Purge(retentionDays int, maxRows int, maxBytes int64) (PurgeStats, error) {
	var stats PurgeStats
	cutoff := time.Now().AddDate(0, 0, -retentionDays).UnixMilli()
	result, err := s.db.Exec("DELETE FROM request_logs WHERE created_at < ?", cutoff)
	if err != nil {
		return stats, err
	}
	stats.Requests, _ = result.RowsAffected()
	result, err = s.db.Exec("DELETE FROM event_logs WHERE created_at < ?", cutoff)
	if err != nil {
		return stats, err
	}
	stats.Events, _ = result.RowsAffected()
	result, err = s.db.Exec("DELETE FROM account_snapshots WHERE created_at < ?", cutoff)
	if err != nil {
		return stats, err
	}
	stats.Snapshots, _ = result.RowsAffected()
	if maxRows > 0 {
		if err := s.tableSize("request_logs", maxRows); err != nil {
			return stats, err
		}
		if err := s.tableSize("event_logs", maxRows); err != nil {
			return stats, err
		}
		if err := s.tableSize("account_snapshots", maxRows); err != nil {
			return stats, err
		}
	}
	if maxBytes > 0 {
		size, err := s.databaseBytes()
		if err == nil && size > maxBytes {
			for _, table := range []string{"request_logs", "event_logs", "account_snapshots"} {
				_, err := s.db.Exec(
					"DELETE FROM " + table + " WHERE id IN (SELECT id FROM " + table + " ORDER BY id ASC LIMIT (SELECT COUNT(*)/5 FROM " + table + "))",
				)
				if err != nil {
					return stats, err
				}
			}
			_, _ = s.db.Exec("VACUUM")
		}
	}
	return stats, nil
}

// parseTimeParam accepts RFC3339 timestamps or unix milliseconds.
func ParseTimeParam(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if milliseconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return milliseconds, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	return parsed.UnixMilli(), nil
}

// nullableIntPtr copies an optional integer pointer.
func nullableIntPtr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// nullableFloatPtr copies an optional float pointer.
func nullableFloatPtr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
