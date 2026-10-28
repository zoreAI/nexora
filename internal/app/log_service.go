package app

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type logService struct {
	mu    sync.Mutex
	path  string
	store *Store
}

func newLogService(dataDir string) *logService {
	path := filepath.Join(dataDir, "logs.jsonl")
	return &logService{path: path}
}

func (l *logService) add(typ, summary string, detail map[string]any) {
	l.addWithID(randID(8), typ, summary, detail)
}

func (l *logService) addWithID(id, typ, summary string, detail map[string]any) {
	item := map[string]any{
		"id":      id,
		"time":    time.Now().Format("2006-01-02 15:04:05"),
		"type":    typ,
		"summary": summary,
		"detail":  detail,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store != nil {
		data, err := json.Marshal(item)
		if err != nil {
			return
		}
		timeValue := strAny(item["time"], "")
		day := ""
		if len(timeValue) >= 10 {
			day = timeValue[:10]
		}
		outcome := strAny(detail["status"], "")
		endpoint := strAny(detail["endpoint"], "")
		model := strAny(detail["model"], "")
		search := strings.ToLower(strings.Join([]string{summary, strAny(detail["request_text"], ""), strAny(detail["error"], "")}, " "))
		stmt := "INSERT INTO app_logs (id, payload, event_time, event_day, log_type, outcome, endpoint, model, search_text) VALUES (" + l.store.bind(1) + ", " + l.store.bind(2) + ", " + l.store.bind(3) + ", " + l.store.bind(4) + ", " + l.store.bind(5) + ", " + l.store.bind(6) + ", " + l.store.bind(7) + ", " + l.store.bind(8) + ", " + l.store.bind(9) + ")"
		if _, err := l.store.db.Exec(stmt, item["id"], string(data), timeValue, day, typ, outcome, endpoint, model, search); err != nil {
			log.Printf("storage append log: %v", err)
		}
		return
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(item)
	f.Write(data)
	f.Write([]byte{'\n'})
}

func (s *Server) logCallStart(id *Identity, endpoint, model, action, requestText string) string {
	callID := randID(8)
	s.logMu.Lock()
	if s.callStarts != nil {
		s.callStarts[callID] = time.Now()
	}
	s.logMu.Unlock()
	detail := map[string]any{"call_id": callID, "endpoint": endpoint, "model": model, "action": action, "status": "started"}
	if id != nil {
		detail["subject_id"] = id.ID
		detail["role"] = id.Role
		detail["name"] = id.Name
	}
	if strings.TrimSpace(requestText) != "" {
		detail["request_text"] = truncateText(requestText, 500)
	}
	s.logSvc.addWithID(callID, "call", action+"开始", detail)
	return callID
}

func (s *Server) logCallSuccess(callID, endpoint, model, action string, extra map[string]any) {
	detail := map[string]any{"call_id": callID, "endpoint": endpoint, "model": model, "action": action, "status": "success"}
	if duration := s.callDurationMS(callID); duration >= 0 {
		detail["duration_ms"] = duration
	}
	for k, v := range extra {
		detail[k] = v
	}
	s.logSvc.finishCall(callID, action+"完成", detail)
}

func (s *Server) logCallFailure(callID, endpoint, model, action string, err error, extra map[string]any) {
	detail := map[string]any{"call_id": callID, "endpoint": endpoint, "model": model, "action": action, "status": "failed"}
	if duration := s.callDurationMS(callID); duration >= 0 {
		detail["duration_ms"] = duration
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	for k, v := range extra {
		detail[k] = v
	}
	s.logSvc.finishCall(callID, action+"失败", detail)
}

func (l *logService) finishCall(callID, summary string, detail map[string]any) {
	l.mu.Lock()
	updated := false
	if l.store != nil {
		var payload string
		if err := l.store.db.QueryRow("SELECT payload FROM app_logs WHERE id = "+l.store.bind(1)+" AND log_type = 'call' AND outcome = 'started' ORDER BY seq DESC LIMIT 1", callID).Scan(&payload); err == nil {
			var item map[string]any
			if json.Unmarshal([]byte(payload), &item) == nil {
				mergeCallLog(item, summary, detail)
				if data, err := json.Marshal(item); err == nil {
					search := callLogSearch(item)
					query := "UPDATE app_logs SET payload = " + l.store.bind(1) + ", outcome = " + l.store.bind(2) + ", search_text = " + l.store.bind(3) + " WHERE id = " + l.store.bind(4) + " AND log_type = 'call' AND outcome = 'started'"
					if result, err := l.store.db.Exec(query, string(data), strAny(detail["status"], ""), search, callID); err == nil {
						n, _ := result.RowsAffected()
						updated = n > 0
					} else {
						log.Printf("storage finish call: %v", err)
					}
				}
			}
		}
	} else if data, err := os.ReadFile(l.path); err == nil {
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			var item map[string]any
			if json.Unmarshal([]byte(lines[i]), &item) != nil || strAny(item["id"], "") != callID {
				continue
			}
			mergeCallLog(item, summary, detail)
			if payload, err := json.Marshal(item); err == nil {
				lines[i] = string(payload)
				if err := os.WriteFile(l.path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err == nil {
					updated = true
				}
			}
			break
		}
	}
	l.mu.Unlock()
	if !updated {
		l.addWithID(callID, "call", summary, detail)
	}
}

func mergeCallLog(item map[string]any, summary string, detail map[string]any) {
	previous, _ := item["detail"].(map[string]any)
	if previous == nil {
		previous = map[string]any{}
	}
	for key, value := range detail {
		previous[key] = value
	}
	item["detail"] = previous
	item["summary"] = summary
}

func callLogSearch(item map[string]any) string {
	detail, _ := item["detail"].(map[string]any)
	return strings.ToLower(strings.Join([]string{strAny(item["summary"], ""), strAny(detail["request_text"], ""), strAny(detail["error"], "")}, " "))
}

func (s *Server) callDurationMS(callID string) int64 {
	if callID == "" {
		return -1
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	start, ok := s.callStarts[callID]
	if !ok {
		return -1
	}
	delete(s.callStarts, callID)
	return time.Since(start).Milliseconds()
}

func truncateText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "..."
}

func (l *logService) list(typ, startDate, endDate string, limit int) []map[string]any {
	return l.listFiltered(typ, startDate, endDate, "", "", "", "", limit)
}

func (l *logService) listFiltered(typ, startDate, endDate, status, endpoint, model, query string, limit int) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	if l.store != nil {
		items, err := l.store.queryLogs(logQuery{typ: typ, startDate: startDate, endDate: endDate, status: status, endpoint: endpoint, model: model, search: query}, limit)
		if err != nil {
			log.Printf("storage list logs: %v", err)
		}
		return items
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return []map[string]any{}
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	// 从后往前读，取 limit 条
	var items []map[string]any
	for i := len(lines) - 1; i >= 0 && len(items) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) != nil {
			continue
		}
		t := strings.TrimSpace(strAny(item["time"], ""))
		day := ""
		if len(t) >= 10 {
			day = t[:10]
		}
		if typ != "" && strAny(item["type"], "") != typ {
			continue
		}
		detail, _ := item["detail"].(map[string]any)
		if status != "" && strAny(detail["status"], "") != status {
			continue
		}
		if endpoint != "" && strAny(detail["endpoint"], "") != endpoint {
			continue
		}
		if model != "" && strAny(detail["model"], "") != model {
			continue
		}
		if query != "" {
			blob := strings.ToLower(strAny(item["summary"], "") + " " + strAny(detail["request_text"], "") + " " + strAny(detail["error"], ""))
			if !strings.Contains(blob, strings.ToLower(query)) {
				continue
			}
		}
		if startDate != "" && day < startDate {
			continue
		}
		if endDate != "" && day > endDate {
			continue
		}
		if item["id"] == nil || strAny(item["id"], "") == "" {
			item["id"] = randID(8)
		}
		items = append(items, item)
	}
	// 反转恢复时间顺序
	sort.Slice(items, func(i, j int) bool {
		return strAny(items[i]["time"], "") > strAny(items[j]["time"], "")
	})
	return items
}

func (l *logService) delete(ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	del := map[string]bool{}
	for _, id := range ids {
		del[id] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store != nil {
		removed := 0
		for _, id := range ids {
			result, err := l.store.db.Exec("DELETE FROM app_logs WHERE id = "+l.store.bind(1), id)
			if err != nil {
				log.Printf("storage delete log: %v", err)
				continue
			}
			n, _ := result.RowsAffected()
			removed += int(n)
		}
		return removed
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var kept []string
	removed := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) != nil {
			kept = append(kept, line)
			continue
		}
		id := strAny(item["id"], "")
		if del[id] {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(l.path, []byte(strings.Join(kept, "\n")+"\n"), 0644); err != nil {
		return 0
	}
	return removed
}
