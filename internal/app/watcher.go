package app

import (
	"context"
	"log"
	"strings"
	"time"
)

const defaultAccountRefreshInterval = 5 * time.Minute

func (s *Server) startLimitedAccountWatcher() {
	s.quotaRefreshChanged = make(chan struct{}, 1)
	s.setAccountQuotaRefreshInterval(s.cfg.RefreshAccountIntervalMinute)
	initial := s.store.LoadAccounts()
	go s.runAccountQuotaWatcher(context.Background(), initial, s.refreshAccountInfo)
}

func (s *Server) runAccountQuotaWatcher(ctx context.Context, accounts []Account, refresh func(context.Context, Account) error) {
	for ctx.Err() == nil {
		s.refreshAccountQuotas(ctx, accounts, time.Now(), refresh)
		s.waitForAccountQuotaRefresh(ctx)
		if ctx.Err() == nil {
			accounts = s.store.LoadAccounts()
		}
	}
}

func (s *Server) setAccountQuotaRefreshInterval(minutes int) {
	interval := time.Duration(minutes) * time.Minute
	if interval <= 0 {
		interval = defaultAccountRefreshInterval
	}
	s.quotaRefreshInterval.Store(int64(interval))
	select {
	case s.quotaRefreshChanged <- struct{}{}:
	default:
	}
}

func (s *Server) waitForAccountQuotaRefresh(ctx context.Context) {
	interval := func() time.Duration {
		if d := time.Duration(s.quotaRefreshInterval.Load()); d > 0 {
			return d
		}
		return defaultAccountRefreshInterval
	}
	timer := time.NewTimer(interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-s.quotaRefreshChanged:
			timer.Reset(interval())
		}
	}
}

func accountQuotaRefreshDue(a Account, now time.Time) bool {
	if strings.TrimSpace(a.AccessToken) == "" || (a.Status != "正常" && a.Status != "限流") {
		return false
	}
	if a.Status == "限流" || (!a.ImageQuotaUnknown && a.Quota <= 0) {
		return true
	}
	for _, reset := range []string{ptrString(a.RateLimitResetAt), ptrString(a.RestoreAt)} {
		if at, err := parseAccountTime(reset); err == nil && !now.Before(at) {
			return true
		}
	}
	checkedAt, err := parseAccountTime(a.QuotaCheckedAt)
	return err != nil || !now.Before(checkedAt.Add(24*time.Hour))
}

func (s *Server) refreshAccountQuotas(ctx context.Context, accounts []Account, now time.Time, refresh func(context.Context, Account) error) {
	var selected []Account
	for _, a := range accounts {
		if accountQuotaRefreshDue(a, now) {
			selected = append(selected, a)
		}
	}
	if len(selected) == 0 {
		return
	}
	log.Printf("[account-quota-watcher] checking %d accounts", len(selected))
	results := runAccountRefreshBatch(ctx, selected, refresh)
	failed := 0
	for _, err := range results {
		if err != nil {
			failed++
		}
	}
	log.Printf("[account-quota-watcher] refreshed=%d failed=%d", len(selected)-failed, failed)
}
