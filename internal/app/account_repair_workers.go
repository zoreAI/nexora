package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type accountRepairProgressKey struct{}

type accountRepairProxyKey struct{}
type accountRepairProxy struct {
	route proxyRoute
	ref   string
}

// The override belongs to one repair task and never changes the stored account
// assignment or global proxy. Login, token refresh and MFA share this route.
func (s *Server) accountLoginProxyRoute(ctx context.Context, account Account) (proxyRoute, error) {
	if selected, ok := ctx.Value(accountRepairProxyKey{}).(accountRepairProxy); ok {
		return selected.route, nil
	}
	return s.selectedProxyRouteForAccount(account)
}

func (s *Server) newAccountLoginClient(ctx context.Context, account Account) (*UpstreamClient, error) {
	route, err := s.accountLoginProxyRoute(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.newUpstreamClientForRoute(account, route)
}

func (s *registerService) accountRepairProxyRoute(ctx context.Context) (proxyRoute, error) {
	if selected, ok := ctx.Value(accountRepairProxyKey{}).(accountRepairProxy); ok {
		return selected.route, nil
	}
	return s.resolveRegisterProxy(s.snapshot().Proxy)
}

func reportAccountRepairProgress(ctx context.Context, stage string) {
	if report, ok := ctx.Value(accountRepairProgressKey{}).(func(string)); ok {
		report(stage)
	}
}

// Keep long authorization and mailbox requests visible without logging credentials.
func runAccountRepairWithProgress(ctx context.Context, interval time.Duration, log func(string), run func(context.Context) error) error {
	var mu sync.Mutex
	stage := "开始修复"
	started := time.Now()
	report := func(next string) {
		mu.Lock()
		defer mu.Unlock()
		stage = next
		log(stage)
	}
	report(stage)
	ctx = context.WithValue(ctx, accountRepairProgressKey{}, report)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				log(fmt.Sprintf("%s（修复已用时 %d 秒）", stage, int(time.Since(started).Seconds())))
				mu.Unlock()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	return run(ctx)
}

type accountOperationLocks struct {
	mu      sync.Mutex
	entries map[string]*accountOperationLock
}
type accountOperationLock struct {
	slot  chan struct{}
	users int
}

func accountOperationKey(email, token string) string {
	if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
		return "email:" + email
	}
	return "token:" + token
}

func (l *accountOperationLocks) acquire(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.entries == nil {
		l.entries = map[string]*accountOperationLock{}
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &accountOperationLock{slot: make(chan struct{}, 1)}
		l.entries[key] = entry
	}
	entry.users++
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		entry.users--
		if entry.users == 0 {
			delete(l.entries, key)
		}
	}
	select {
	case entry.slot <- struct{}{}:
		return func() { <-entry.slot; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

type accountRepairTarget struct {
	account Account
	email   string
	pending bool
}

func accountRepairTargets(accounts []Account, pending []string) []accountRepairTarget {
	targets := []accountRepairTarget{}
	seen := map[string]bool{}
	for _, account := range accounts {
		if account.Status != "异常" || account.AccessToken == "" {
			continue
		}
		email := strings.TrimSpace(ptrString(account.Email))
		key := accountOperationKey(email, account.AccessToken)
		if seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, accountRepairTarget{account: account, email: email})
	}
	for _, email := range pending {
		email = strings.TrimSpace(email)
		key := accountOperationKey(email, "")
		if email == "" || seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, accountRepairTarget{email: email, pending: true})
	}
	return targets
}

func (s *Server) accountRepairThreads() int {
	if s.register == nil {
		return 1
	}
	return maxInt(s.register.snapshot().Threads, 1)
}

func (s *Server) repairAccountTarget(ctx context.Context, target accountRepairTarget, index int) error {
	var route proxyRoute
	var err error
	ref := ""
	if s.register != nil {
		ref = strings.TrimSpace(s.register.snapshot().Proxy)
	}
	if ref != "" {
		route, err = s.register.resolveRegisterProxy(ref)
	} else if !target.pending {
		route, err = s.selectedProxyRouteForAccount(target.account)
	}
	if err != nil {
		return fmt.Errorf("选择修复代理失败: %w", err)
	}
	attempted := map[string]bool{}
	for {
		if route.URL != "" {
			attempted[route.URL] = true
		}
		err = s.repairAccountTargetOnRoute(ctx, target, index, route, ref)
		if err == nil {
			if route.NodeID != "" && s.store != nil {
				_ = s.store.recordProxyNodeRuntimeResult(route, true)
			}
			return nil
		}
		if ctx.Err() != nil || !isTransientRegisterNetworkError(err) || route.NodeID == "" || !strings.HasPrefix(ref, "group:") {
			return redactProxyTransportError(err, route.URL)
		}
		if s.store != nil {
			_ = s.store.recordProxyNodeRuntimeResult(route, false)
		}
		if s.register == nil {
			return redactProxyTransportError(err, route.URL)
		}
		next, nextErr := s.register.nextRegisterMFAProxy(ref, route, attempted)
		if nextErr != nil {
			return fmt.Errorf("切换修复代理节点失败: %w; 原因: %v", nextErr, redactProxyTransportError(err, route.URL))
		}
		if next.NodeID == "" {
			return fmt.Errorf("修复代理组无其他可用节点: %w", redactProxyTransportError(err, route.URL))
		}
		s.register.appendLog(fmt.Sprintf("[任务%d] %s 代理节点[%s]连接失败，切换到[%s]重试", index, target.email, route.NodeID, next.NodeID), "yellow")
		route = next
	}
}

func (s *Server) repairAccountTargetOnRoute(ctx context.Context, target accountRepairTarget, index int, route proxyRoute, ref string) error {
	ctx = context.WithValue(ctx, accountRepairProxyKey{}, accountRepairProxy{route: route, ref: ref})
	var err error
	if route.NodeID != "" {
		reportAccountRepairProgress(ctx, "修复代理节点["+route.NodeID+"]")
	} else if route.URL != "" {
		reportAccountRepairProgress(ctx, "修复使用固定代理")
	} else {
		reportAccountRepairProgress(ctx, "修复使用直连")
	}
	if target.pending {
		err = s.repairPendingRegistration(ctx, target.email, index)
	} else {
		err = s.repairAbnormalAccount(ctx, target.account)
	}
	if err != nil {
		return err
	}
	return nil
}

func accountRepairTimeout(mail RegisterMailConfig) time.Duration {
	waitSeconds := maxInt(mail.WaitTimeout, 30)
	for _, provider := range mail.Providers {
		if !boolAny(provider["enable"], true) {
			continue
		}
		waitSeconds = maxInt(waitSeconds, intAny(provider["wait_timeout"], 0))
	}
	// Platform continuation, Web signup and password setup can each require a
	// fresh email OTP. Leave separate time for authorization, profile and MFA.
	return 6*time.Minute + 3*time.Duration(waitSeconds)*time.Second
}

// Callbacks run on the coordinator, so counts and logs reflect completed work
// consistently even when workers finish at the same time.
func runAccountRepairTargets(ctx context.Context, targets []accountRepairTarget, threads int,
	timeout time.Duration,
	run func(context.Context, accountRepairTarget, int) error,
	started func(), finished func(accountRepairTarget, int, error), shouldSubmit ...func() bool) {
	workers := min(maxInt(threads, 1), len(targets))
	if workers == 0 {
		return
	}
	type job struct {
		target accountRepairTarget
		index  int
		err    error
	}
	jobs := make(chan job)
	results := make(chan job, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				itemCtx, cancel := context.WithTimeout(ctx, timeout)
				item.err = run(itemCtx, item.target, item.index)
				cancel()
				results <- item
			}
		}()
	}
	defer func() { close(jobs); wg.Wait() }()
	next, active := 0, 0
	cancelled := ctx.Done()
	for next < len(targets) || active > 0 {
		if ctx.Err() != nil || (len(shouldSubmit) > 0 && shouldSubmit[0] != nil && !shouldSubmit[0]()) {
			next = len(targets)
		}
		var send chan job
		var item job
		if next < len(targets) && active < workers {
			send = jobs
			item = job{target: targets[next], index: next + 1}
		}
		if send == nil && active == 0 {
			break
		}
		select {
		case send <- item:
			next++
			active++
			if started != nil {
				started()
			}
		case result := <-results:
			active--
			if finished != nil {
				finished(result.target, result.index, result.err)
			}
		case <-cancelled:
			next = len(targets)
			cancelled = nil
		}
	}
}
