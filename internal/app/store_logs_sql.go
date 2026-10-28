package app

import (
	"database/sql"
	"encoding/json"
	"strings"
)

func (s *Store) migrateLogColumns() error {
	statements := []string{
		"ALTER TABLE app_logs ADD COLUMN event_time TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_logs ADD COLUMN event_day TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_logs ADD COLUMN log_type TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_logs ADD COLUMN outcome TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_logs ADD COLUMN endpoint TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_logs ADD COLUMN model TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_logs ADD COLUMN search_text TEXT NOT NULL DEFAULT ''",
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil && !isDuplicateColumnError(err) {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.backend == "postgres" {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('chatgpt2api_log_columns_v1'))"); err != nil {
			return err
		}
	}
	var marker string
	err = tx.QueryRow("SELECT name FROM app_migrations WHERE name = 'log_columns_v1'").Scan(&marker)
	if err == nil {
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	for _, stmt := range []string{
		"CREATE INDEX IF NOT EXISTS app_logs_type_seq_idx ON app_logs (log_type, seq)",
		"CREATE INDEX IF NOT EXISTS app_logs_day_seq_idx ON app_logs (event_day, seq)",
		"CREATE INDEX IF NOT EXISTS app_logs_outcome_seq_idx ON app_logs (outcome, seq)",
		"CREATE INDEX IF NOT EXISTS app_logs_endpoint_seq_idx ON app_logs (endpoint, seq)",
		"CREATE INDEX IF NOT EXISTS app_logs_model_seq_idx ON app_logs (model, seq)",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	rows, err := tx.Query("SELECT seq, payload FROM app_logs WHERE event_time = '' OR log_type = ''")
	if err != nil {
		return err
	}
	type logRow struct {
		seq     int64
		payload string
	}
	var pending []logRow
	for rows.Next() {
		var item logRow
		if err := rows.Scan(&item.seq, &item.payload); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, row := range pending {
		var item map[string]any
		if err := json.Unmarshal([]byte(row.payload), &item); err != nil {
			continue
		}
		timeValue := strings.TrimSpace(strAny(item["time"], ""))
		day := ""
		if len(timeValue) >= 10 {
			day = timeValue[:10]
		}
		detail, _ := item["detail"].(map[string]any)
		logType := strAny(item["type"], "")
		outcome := strAny(detail["status"], "")
		endpoint := strAny(detail["endpoint"], "")
		model := strAny(detail["model"], "")
		search := strings.ToLower(strings.Join([]string{strAny(item["summary"], ""), strAny(detail["request_text"], ""), strAny(detail["error"], "")}, " "))
		query := "UPDATE app_logs SET event_time=" + s.bind(1) + ", event_day=" + s.bind(2) + ", log_type=" + s.bind(3) + ", outcome=" + s.bind(4) + ", endpoint=" + s.bind(5) + ", model=" + s.bind(6) + ", search_text=" + s.bind(7) + " WHERE seq=" + s.bind(8)
		if _, err := tx.Exec(query, timeValue, day, logType, outcome, endpoint, model, search, row.seq); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT INTO app_migrations (name) VALUES ('log_columns_v1') ON CONFLICT (name) DO NOTHING"); err != nil {
		return err
	}
	return tx.Commit()
}

func isDuplicateColumnError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate column") || strings.Contains(message, "already exists")
}

func (s *Store) mergeLegacyCallLogs() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.backend == "postgres" {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('chatgpt2api_call_logs_v1'))"); err != nil {
			return err
		}
	}
	var marker string
	if err := tx.QueryRow("SELECT name FROM app_migrations WHERE name = 'call_logs_v1'").Scan(&marker); err == nil {
		return tx.Commit()
	} else if err != sql.ErrNoRows {
		return err
	}
	rows, err := tx.Query("SELECT seq, payload FROM app_logs WHERE log_type = 'call' AND outcome IN ('started', 'success', 'failed', 'partial_success') ORDER BY seq")
	if err != nil {
		return err
	}
	type logRow struct {
		seq     int64
		payload string
	}
	var logs []logRow
	for rows.Next() {
		var row logRow
		if err := rows.Scan(&row.seq, &row.payload); err != nil {
			rows.Close()
			return err
		}
		logs = append(logs, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	starts := map[string]logRow{}
	for _, row := range logs {
		var item map[string]any
		if json.Unmarshal([]byte(row.payload), &item) != nil {
			continue
		}
		detail, _ := item["detail"].(map[string]any)
		callID := strAny(detail["call_id"], "")
		if callID == "" {
			continue
		}
		if strAny(detail["status"], "") == "started" {
			starts[callID] = row
			continue
		}
		start, ok := starts[callID]
		if !ok {
			continue
		}
		var first map[string]any
		if json.Unmarshal([]byte(start.payload), &first) != nil {
			continue
		}
		mergeCallLog(first, strAny(item["summary"], ""), detail)
		payload, err := json.Marshal(first)
		if err != nil {
			return err
		}
		query := "UPDATE app_logs SET payload = " + s.bind(1) + ", outcome = " + s.bind(2) + ", search_text = " + s.bind(3) + " WHERE seq = " + s.bind(4)
		if _, err := tx.Exec(query, string(payload), strAny(detail["status"], ""), callLogSearch(first), start.seq); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM app_logs WHERE seq = "+s.bind(1), row.seq); err != nil {
			return err
		}
		delete(starts, callID)
	}
	if _, err := tx.Exec("INSERT INTO app_migrations (name) VALUES ('call_logs_v1') ON CONFLICT (name) DO NOTHING"); err != nil {
		return err
	}
	return tx.Commit()
}

type logQuery struct {
	typ, startDate, endDate, status, endpoint, model, search string
}

func (s *Store) queryLogs(q logQuery, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 200
	}
	where := []string{"1=1"}
	args := []any{}
	add := func(condition string, value any) {
		args = append(args, value)
		where = append(where, condition+" "+s.bind(len(args)))
	}
	if q.typ != "" {
		add("log_type =", q.typ)
	}
	if q.startDate != "" {
		add("event_day >=", q.startDate)
	}
	if q.endDate != "" {
		add("event_day <=", q.endDate)
	}
	if q.status != "" {
		add("outcome =", q.status)
	}
	if q.endpoint != "" {
		add("endpoint =", q.endpoint)
	}
	if q.model != "" {
		add("model =", q.model)
	}
	if q.search != "" {
		add("search_text LIKE", "%"+strings.ToLower(q.search)+"%")
	}
	args = append(args, limit)
	query := "SELECT payload FROM app_logs WHERE " + strings.Join(where, " AND ") + " ORDER BY seq DESC LIMIT " + s.bind(len(args))
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var item map[string]any
		if json.Unmarshal([]byte(payload), &item) == nil {
			items = append(items, item)
		}
	}
	return items, rows.Err()
}

func (s *Store) dashboardLogBuckets(rangeName string, start, end string, hourly bool) (map[string]dashboardBucket, error) {
	where := []string{"log_type = 'call'"}
	args := []any{}
	if hourly {
		where = append(where, "event_time >= "+s.bind(1), "event_time < "+s.bind(2))
		args = append(args, start+":00", end+":00")
	} else {
		where = append(where, "event_day >= "+s.bind(1), "event_day <= "+s.bind(2))
		args = append(args, start, end)
	}
	query := "SELECT event_time, event_day, outcome, model FROM app_logs WHERE " + strings.Join(where, " AND ")
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]dashboardBucket{}
	for rows.Next() {
		var eventTime, eventDay, outcome, model string
		if err := rows.Scan(&eventTime, &eventDay, &outcome, &model); err != nil {
			return nil, err
		}
		key := eventDay
		if hourly && len(eventTime) >= 13 {
			key = eventTime[:13]
		}
		bucket := out[key]
		if bucket.Models == nil {
			bucket.Time = key
			bucket.Models = map[string]int{}
		}
		switch outcome {
		case "success", "partial_success":
			bucket.Success++
		case "failed":
			bucket.Failed++
		default:
			continue
		}
		if strings.TrimSpace(model) == "" {
			model = "未指定"
		}
		bucket.Models[model]++
		out[key] = bucket
	}
	return out, rows.Err()
}
