package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestAccountRepairProgressShowsStageWhileWorkerWaits(t *testing.T) {
	logs := make(chan string, 20)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runAccountRepairWithProgress(context.Background(), 5*time.Millisecond, func(message string) {
			logs <- message
		}, func(ctx context.Context) error {
			reportAccountRepairProgress(ctx, "等待邮箱验证码")
			<-release
			return nil
		})
	}()
	for _, want := range []string{"开始修复", "等待邮箱验证码", "等待邮箱验证码（修复已用时"} {
		select {
		case message := <-logs:
			if !strings.HasPrefix(message, want) {
				t.Errorf("progress=%q, want prefix %q", message, want)
			}
		case <-time.After(time.Second):
			t.Error("waiting repair has no visible progress")
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("progress reporter did not stop with worker")
	}
}

func TestRepairTargetsRotateProxyPoolOncePerTask(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	group, err := s.store.SaveProxyGroup(proxyGroupInput{Name: "repair", Strategy: "round_robin", Nodes: []proxyNodeInput{
		{Name: "a", URL: "http://127.0.0.1:18001"}, {Name: "b", URL: "http://127.0.0.1:18002"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.register.cfg.Proxy = "group:" + group.ID
	email, password := "repair@example.com", "test-password"
	account := Account{AccessToken: "token", Email: &email, Password: &password, Status: "异常", Proxy: "direct"}
	if err := s.store.SaveAccounts([]Account{account}); err != nil {
		t.Fatal(err)
	}
	var proxies, stages []string
	s.accountLoginSessionFactory = func(proxy string, _ int) (*registerSession, error) {
		proxies = append(proxies, proxy)
		return nil, errors.New("stop before real authorization")
	}
	ctx := context.WithValue(context.Background(), accountRepairProgressKey{}, func(stage string) { stages = append(stages, stage) })
	for i := 0; i < 3; i++ {
		if err := s.repairAccountTarget(ctx, accountRepairTarget{account: account, email: email}, i+1); err == nil {
			t.Fatal("expected mocked authorization failure")
		}
	}
	if len(proxies) != 3 || proxies[0] != group.Nodes[0].URL || proxies[1] != group.Nodes[1].URL || proxies[2] != group.Nodes[0].URL {
		t.Fatalf("incorrect repair rotation: %v", proxies)
	}
	if len(stages) != 3 || !strings.Contains(stages[0], group.Nodes[0].ID) || !strings.Contains(stages[1], group.Nodes[1].ID) {
		t.Fatalf("missing node diagnostics: %v", stages)
	}
	// Ordinary login retains its account proxy policy.
	_, _ = s.reLoginAccountWithPassword(context.Background(), account)
	if proxies[3] != "" || s.store.LoadAccounts()[0].Proxy != "direct" {
		t.Fatal("repair proxy leaked into normal login or persisted account settings")
	}
	// An invalid configured pool must fail before starting authorization.
	s.register.cfg.Proxy = "group:missing"
	if err := s.repairAccountTarget(ctx, accountRepairTarget{account: account}, 4); err == nil || len(proxies) != 4 {
		t.Fatal("repair silently fell back from an unavailable configured proxy pool")
	}
}

func TestRepairTargetRetriesTransientProxyFailure(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	group, err := s.store.SaveProxyGroup(proxyGroupInput{Name: "repair-failover", Strategy: "round_robin", Nodes: []proxyNodeInput{
		{Name: "first", URL: "http://127.0.0.1:18101"}, {Name: "second", URL: "http://127.0.0.1:18102"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.register.cfg.Proxy = "group:" + group.ID
	email, password := "failover@example.com", "saved-password"
	account := Account{AccessToken: "failover-token", Email: &email, Password: &password, Status: "异常"}
	if err := s.store.SaveAccounts([]Account{account}); err != nil {
		t.Fatal(err)
	}
	var proxies []string
	s.accountLoginSessionFactory = func(proxy string, _ int) (*registerSession, error) {
		proxies = append(proxies, proxy)
		if len(proxies) == 1 {
			return nil, fmt.Errorf("dial chatgpt: %w", syscall.ECONNRESET)
		}
		return nil, errors.New("second node unavailable")
	}
	err = s.repairAccountTarget(context.Background(), accountRepairTarget{account: account, email: email}, 1)
	if err == nil || len(proxies) != 2 || proxies[0] != group.Nodes[0].URL || proxies[1] != group.Nodes[1].URL {
		t.Fatalf("transient repair failure did not rotate once: err=%v proxies=%v", err, proxies)
	}
}

func TestMailboxPollingCancellationRetainsDiagnostics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := waitForRegisterCode(ctx, &registerMailbox{Address: "test@example.com"}, 180, 2, func() ([]map[string]any, error) {
		cancel()
		return nil, errors.New("mail provider unavailable")
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "最近轮询邮件0封") || !strings.Contains(err.Error(), "mail provider unavailable") {
		t.Fatalf("polling diagnostics lost: %v", err)
	}
}

func TestStopAbnormalRepairKeepsRunningWorkersAlive(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.register.cancel = cancel
	s.register.cfg.Enabled = true
	s.register.cfg.Stats.JobKind = "repair_abnormal"
	result := s.register.stop()
	if result.Enabled || ctx.Err() != nil {
		t.Fatal("stop must disable submission without canceling running workers")
	}
}

func TestStoppedRepairFinishesActiveMFAAndLeavesQueueUntouched(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	s.register.cfg.Threads = 5
	s.register.cfg.Enabled = true
	s.register.cfg.Stats = RegisterStats{JobID: "cancel-repair", JobKind: "repair_abnormal", Threads: 5}
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	accounts := []Account{}
	for i := 0; i < 8; i++ {
		email := fmt.Sprintf("cancel%d@icloud.com", i)
		accounts = append(accounts, Account{AccessToken: fmt.Sprintf("cancel-token%d", i), Email: &email, Status: "异常", MFAStatus: "pending", MFASecret: &secret})
	}
	if err := s.store.SaveAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	s.register.mfaSessionFactory = func(string) (*registerSession, error) {
		return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			body := `{"success":true}`
			if strings.HasSuffix(req.URL.Path, "/enroll") {
				entered <- struct{}{}
				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				body = `{"secret":"` + secret + `","session_id":"session"}`
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.register.cancel = cancel
	done := make(chan struct{})
	go func() { defer close(done); s.register.runRepairAbnormalJob(ctx) }()
	for i := 0; i < 5; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
			t.Fatal("repair workers did not enter enrollment")
		}
	}
	s.register.stop()
	if ctx.Err() != nil || s.register.snapshot().Stats.Running != 5 {
		t.Fatal("stop canceled active repair requests")
	}
	// Clicking repair again while draining must not cancel/restart the batch.
	if result := s.register.repairAbnormal(); result.Stats.JobID != "cancel-repair" || result.Enabled || ctx.Err() != nil {
		t.Fatal("repair restarted before active workers drained")
	}
	unblock()
	// MFA can wait up to five seconds for a fresh TOTP window.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("repair did not finish active accounts")
	}
	cfg := s.register.snapshot()
	if cfg.Stats.Fail != 0 || cfg.Stats.Success != 5 || cfg.Stats.Done != 5 || cfg.Stats.Running != 0 || len(cfg.Failures) != 0 {
		t.Fatalf("active tasks did not finish normally: %+v", cfg.Stats)
	}
	for _, log := range cfg.Logs {
		if strings.Contains(log.Text, "修复失败") {
			t.Fatalf("cancellation logged as failure: %s", log.Text)
		}
	}
	last := cfg.Logs[len(cfg.Logs)-1].Text
	if !strings.Contains(last, "成功5，失败0，未处理3") {
		t.Fatalf("incorrect stop summary: %s", last)
	}
	for i, account := range s.store.LoadAccounts() {
		if i < 5 {
			if account.MFAStatus != "enabled" || account.Status != "正常" {
				t.Fatal("active account did not finish MFA")
			}
		} else if account.MFAStatus != "pending" || ptrString(account.MFASecret) != secret || account.Status != "异常" {
			t.Fatalf("queued account was processed after stop: %s", ptrString(account.Email))
		}
	}
}

func TestAccountRepairTargetsDeduplicateEmailAcrossPoolAndPending(t *testing.T) {
	first, second := " amicus.medias-9q@icloud.com ", "AMICUS.MEDIAS-9Q@icloud.com"
	accounts := []Account{{AccessToken: "first", Email: &first, Status: "异常"}, {AccessToken: "second", Email: &second, Status: "异常"}, {AccessToken: "normal", Status: "正常"}, {AccessToken: "no-email", Status: "异常"}}
	targets := accountRepairTargets(accounts, []string{second, "other@icloud.com", " OTHER@icloud.com ", ""})
	if len(targets) != 3 || targets[0].account.AccessToken != "first" || targets[1].pending || !targets[2].pending {
		t.Fatalf("duplicate repair targets: %+v", targets)
	}
}

func TestAccountRepairWorkersUseConfiguredThreadsAndAccurateCounts(t *testing.T) {
	targets := []accountRepairTarget{}
	for i := 0; i < 13; i++ {
		targets = append(targets, accountRepairTarget{email: fmt.Sprintf("%d@icloud.com", i)})
	}
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	entered := make(chan int, len(targets))
	done := make(chan struct{})
	running, peak, success, failed := 0, 0, 0, 0
	go func() {
		defer close(done)
		runAccountRepairTargets(context.Background(), targets, 5, time.Minute, func(ctx context.Context, _ accountRepairTarget, index int) error {
			entered <- index
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if index == 3 {
				return errors.New("test failure")
			}
			return nil
		}, func() {
			running++
			if running > peak {
				peak = running
			}
		}, func(_ accountRepairTarget, _ int, err error) {
			running--
			if err == nil {
				success++
			} else {
				failed++
			}
		})
	}()
	for i := 0; i < 5; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("configured five workers did not run concurrently")
		}
	}
	select {
	case <-entered:
		t.Fatal("worker limit exceeded before a worker finished")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker batch did not finish")
	}
	if running != 0 || peak != 5 || success != 12 || failed != 1 {
		t.Fatalf("running=%d peak=%d success=%d failed=%d", running, peak, success, failed)
	}
}

func TestAccountRepairCancellationStopsQueuedTasks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 8)
	done := make(chan struct{})
	var calls atomic.Int32
	targets := make([]accountRepairTarget, 8)
	go func() {
		defer close(done)
		runAccountRepairTargets(ctx, targets, 2, time.Minute, func(ctx context.Context, _ accountRepairTarget, _ int) error {
			calls.Add(1)
			entered <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}, nil, nil)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled workers did not drain")
	}
	if calls.Load() != 2 {
		t.Fatalf("queued tasks started after cancellation: %d", calls.Load())
	}
}

func TestAccountOperationLocksAllowDifferentEmailsAndCancelSameEmailWait(t *testing.T) {
	var locks accountOperationLocks
	release, err := locks.acquire(context.Background(), accountOperationKey(" user@icloud.com ", "first"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := locks.acquire(context.Background(), accountOperationKey("other@icloud.com", "second"))
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := locks.acquire(ctx, accountOperationKey("USER@icloud.com", "rotated")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same mailbox was not protected: %v", err)
	}
	release()
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.entries) != 0 {
		t.Fatalf("operation locks not released: %d", len(locks.entries))
	}
}

func TestPendingRepairsRunLoginAndMFAConcurrently(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	s.register.cfg.Mail = RegisterMailConfig{RequestTimeout: 2, Providers: []map[string]any{{"type": "icloud_api", "enable": true, "api_base": "http://127.0.0.1:1", "api_key": "test-key"}}}
	const count = 5
	cancels := map[string]context.CancelFunc{}
	contexts := map[string]context.Context{}
	for i := 0; i < count; i++ {
		email := fmt.Sprintf("partial%d@icloud.com", i)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancels[email] = cancel
		contexts[email] = ctx
		if err := s.register.savePendingPassword(email, "saved-password", "注册接口已接受"); err != nil {
			t.Fatal(err)
		}
	}
	entered := make(chan string, count)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	s.register.recoverySessionFactory = func(string) (*registerSession, error) {
		jar, _ := cookiejar.New(nil)
		email := ""
		return &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			status, body := 200, `{}`
			headers := make(http.Header)
			switch req.URL.Path {
			case "/api/accounts/authorize":
				email = req.URL.Query().Get("login_hint")
				status = 302
				headers.Set("Location", registerAuthBase+"/log-in/password")
			case "/log-in/password":
				body = "<html></html>"
			case "/backend-api/sentinel/req":
				body = `{"token":"sentinel"}`
			case "/api/accounts/password/verify":
				body = `{"continue_url":"https://platform.openai.com/auth/callback?code=code"}`
			case "/api/accounts/oauth/token":
				body = fmt.Sprintf(`{"access_token":%q}`, "token-"+email)
			case "/backend-api/accounts/mfa/enroll":
				entered <- email
				<-release
				body = `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`
			case "/backend-api/accounts/mfa/user/activate_enrollment":
				body = `{"success":true}`
				cancels[email]()
			default:
				t.Errorf("unexpected request: %s", req.URL.Path)
				status = 400
			}
			return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}, nil
	}
	results := make(chan error, count)
	for email, ctx := range contexts {
		go func() { results <- s.repairPendingRegistration(ctx, email) }()
	}
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			unblock()
			for j := 0; j < count; j++ {
				<-results
			}
			t.Fatal("different accounts were serialized during login or MFA")
		}
	}
	unblock()
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	accounts := s.store.LoadAccounts()
	if len(accounts) != count || len(s.pendingRegisterAccounts()) != 0 {
		t.Fatalf("completed repairs lost: accounts=%d pending=%d", len(accounts), len(s.pendingRegisterAccounts()))
	}
	for _, account := range accounts {
		if account.Status != "正常" || account.MFAStatus != "enabled" {
			t.Fatalf("incomplete repair: %+v", account)
		}
	}
	// A stale duplicate candidate must not attempt to log in again or fail after
	// the first task has already completed and cleared its pending record.
	s.register.recoverySessionFactory = func(string) (*registerSession, error) {
		t.Fatal("duplicate repair opened a new session")
		return nil, nil
	}
	if err := s.repairPendingRegistration(context.Background(), "partial0@icloud.com"); err != nil {
		t.Fatalf("completed account was reported failed: %v", err)
	}
}

func TestRegisterRepairJobReportsFiveActiveWorkers(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	s.register.cfg.Threads = 5
	s.register.cfg.Enabled = true
	s.register.cfg.Stats = RegisterStats{JobID: "repair-job", JobKind: "repair_abnormal", Threads: 5}
	accounts := []Account{}
	for i := 0; i < 8; i++ {
		email := fmt.Sprintf("account%d@icloud.com", i)
		accounts = append(accounts, Account{AccessToken: fmt.Sprintf("token%d", i), Email: &email, Status: "异常", MFAStatus: "pending"})
	}
	if err := s.store.SaveAccounts(accounts); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	s.register.mfaSessionFactory = func(string) (*registerSession, error) {
		return &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
			body := `{"success":true}`
			if strings.HasSuffix(req.URL.Path, "/enroll") {
				entered <- struct{}{}
				<-release
				body = `{"secret":"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ","session_id":"session"}`
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); s.register.runRepairAbnormalJob(context.Background()) }()
	for i := 0; i < 5; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			unblock()
			<-done
			t.Fatal("repair job did not use configured workers")
		}
	}
	stats := s.register.snapshot().Stats
	if stats.Running != 5 || stats.Threads != 5 || stats.Done != 0 {
		unblock()
		<-done
		t.Fatalf("incorrect live worker count: %+v", stats)
	}
	unblock()
	<-done
	stats = s.register.snapshot().Stats
	if stats.Running != 0 || stats.Success != 8 || stats.Fail != 0 || stats.Done != 8 || stats.FinishedAt == "" {
		t.Fatalf("incorrect final repair counts: %+v", stats)
	}
}

func TestRepairBudgetAllowsSeparateOTPStages(t *testing.T) {
	mail := RegisterMailConfig{WaitTimeout: 180, Providers: []map[string]any{{"type": "icloud_api", "wait_timeout": 240}}}
	finished := false
	runAccountRepairTargets(context.Background(), []accountRepairTarget{{pending: true}}, 1, accountRepairTimeout(mail), func(ctx context.Context, _ accountRepairTarget, _ int) error {
		deadline, ok := ctx.Deadline()
		// A Platform continuation, a Web fallback and password setup may each
		// wait for a new code. The task budget must leave time for all three.
		if !ok || time.Until(deadline) < 3*240*time.Second+2*time.Minute {
			t.Error("whole-account deadline truncates independent OTP stages")
		}
		return nil
	}, nil, func(_ accountRepairTarget, _ int, err error) {
		finished = err == nil
	})
	if !finished {
		t.Fatal("repair did not finish")
	}
}
