package app

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

type dashboardBucket struct {
	Time    string         `json:"time"`
	Success int            `json:"success"`
	Failed  int            `json:"failed"`
	Models  map[string]int `json:"models"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "7d"
	}
	if rangeName != "24h" && rangeName != "7d" && rangeName != "30d" {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	accounts := s.store.LoadAccounts()
	statuses := map[string]int{"正常": 0, "限流": 0, "异常": 0, "封号": 0, "禁用": 0}
	quota := 0
	unknownQuota := 0
	for _, account := range accounts {
		statuses[account.Status]++
		if account.ImageQuotaUnknown || account.Type == "pro" || account.Type == "prolite" {
			unknownQuota++
		}
		if account.Type != "pro" && account.Type != "prolite" && !account.ImageQuotaUnknown && account.Quota > 0 {
			quota += account.Quota
		}
	}
	pending := len(s.pendingRegisterAccountsFrom(accounts))
	statuses["异常"] += pending
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": map[string]any{"total": len(accounts) + pending, "statuses": statuses, "remaining_quota": quota, "unknown_quota": unknownQuota},
		"range":    rangeName,
		"buckets":  s.logSvc.dashboardBuckets(rangeName, time.Now()),
	})
}

func (l *logService) dashboardBuckets(rangeName string, now time.Time) []dashboardBucket {
	now = now.Local()
	hourly := rangeName == "24h"
	count := 7
	if hourly {
		count = 24
	} else if rangeName == "30d" {
		count = 30
	}
	start := now.Truncate(time.Hour)
	if hourly {
		start = start.Add(-time.Duration(count-1) * time.Hour)
	} else {
		start = time.Date(now.Year(), now.Month(), now.Day()-(count-1), 0, 0, 0, 0, now.Location())
	}
	buckets := make([]dashboardBucket, count)
	byTime := make(map[string]*dashboardBucket, count)
	for i := range buckets {
		at := start.AddDate(0, 0, i)
		format := "2006-01-02"
		if hourly {
			at = start.Add(time.Duration(i) * time.Hour)
			format = "2006-01-02 15"
		}
		buckets[i] = dashboardBucket{Time: at.Format(format), Models: map[string]int{}}
		byTime[buckets[i].Time] = &buckets[i]
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store != nil {
		startKey := start.Format("2006-01-02")
		endKey := now.Format("2006-01-02")
		if hourly {
			startKey = start.Format("2006-01-02 15")
			endKey = now.Truncate(time.Hour).Add(time.Hour).Format("2006-01-02 15")
		}
		rows, err := l.store.dashboardLogBuckets(rangeName, startKey, endKey, hourly)
		if err != nil {
			return buckets
		}
		for key, item := range rows {
			if bucket := byTime[key]; bucket != nil {
				*bucket = item
			}
		}
		return buckets
	}

	f, err := os.Open(l.path)
	if err != nil {
		return buckets
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var item struct {
			Time   string         `json:"time"`
			Type   string         `json:"type"`
			Detail map[string]any `json:"detail"`
		}
		if json.Unmarshal(scanner.Bytes(), &item) != nil || item.Type != "call" {
			continue
		}
		key := item.Time
		if hourly {
			if len(key) < 13 {
				continue
			}
			key = key[:13]
		} else {
			if len(key) < 10 {
				continue
			}
			key = key[:10]
		}
		bucket := byTime[key]
		if bucket == nil {
			continue
		}
		switch item.Detail["status"] {
		case "success", "partial_success":
			bucket.Success++
		case "failed":
			bucket.Failed++
		default:
			continue
		}
		model := strings.TrimSpace(strAny(item.Detail["model"], ""))
		if model == "" {
			model = "未指定"
		}
		bucket.Models[model]++
	}
	return buckets
}
