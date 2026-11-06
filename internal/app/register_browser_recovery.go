package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/chromedp/chromedp"
)

// resetPendingRegisterPassword uses a separate browser session because the
// ChatGPT password-reset authorization is bound to its own CSRF cookie.
func resetPendingRegisterPassword(ctx context.Context, provider registerProvider, mailbox *registerMailbox, password, proxyURL string) (map[string]any, error) {
	session, ok := provider.Session().(*registerSession)
	if !ok {
		return nil, errors.New("注册会话不可用于浏览器密码重置")
	}
	return setRegisterPasswordInBrowser(ctx, session, mailbox, password, proxyURL, false, func(ctx context.Context, notBefore time.Time) (string, error) {
		mailbox.CodeNotBefore = notBefore
		return provider.WaitForCode(ctx, mailbox)
	})
}

// resetPendingRegisterPasswordForRepair keeps the browser reset flow
// injectable for recovery tests while using the real password-reset flow in
// production. A stored candidate password is not trusted until this flow
// receives the provider's successful password response.
func (s *registerService) resetPendingRegisterPasswordForRepair(ctx context.Context, provider registerProvider, mailbox *registerMailbox, password, proxyURL string) (map[string]any, error) {
	if s != nil && s.passwordResetBrowserFactory != nil {
		return s.passwordResetBrowserFactory(ctx, provider, mailbox, password, proxyURL)
	}
	return resetPendingRegisterPassword(ctx, provider, mailbox, password, proxyURL)
}

func setRegisterPasswordInBrowser(ctx context.Context, session *registerSession, mailbox *registerMailbox, password, proxyURL string, add bool, waitCode func(context.Context, time.Time) (string, error)) (map[string]any, error) {
	if waitCode == nil {
		return nil, errors.New("没有可用的邮箱验证码读取方式，无法设置密码")
	}
	solver := &browserSentinelSolver{proxy: proxyURL, userAgent: session.userAgent}
	defer solver.close()
	if err := solver.ensurePage(ctx); err != nil {
		return nil, fmt.Errorf("打开密码重置浏览器: %w", err)
	}
	runCtx, done := solver.runContext(ctx, 6*time.Minute)
	defer done()
	if progress, ok := ctx.Value(accountRepairProgressKey{}).(func(string)); ok {
		runCtx = context.WithValue(runCtx, accountRepairProgressKey{}, progress)
	}
	trace, err := observeRegisterBrowser(runCtx, mailbox.Address)
	if err != nil {
		return nil, err
	}
	flow := &registerPasswordBrowserFlow{ctx: runCtx, email: mailbox.Address, password: password, add: add, trace: trace, waitCode: waitCode}
	flow.accepted = func() error {
		session.passwordConfirmed = true
		if session.onPasswordAccepted != nil {
			return session.onPasswordAccepted()
		}
		return nil
	}
	if err := flow.authorize(); err != nil {
		return nil, err
	}
	if err := flow.verifyEmail(); err != nil {
		return nil, err
	}
	passwordErr := flow.setPassword()
	if passwordErr != nil && !isPasswordAlreadyUsedError(passwordErr) {
		return nil, passwordErr
	}
	if !add || passwordErr != nil {
		// A signed-out reset ends on reset_password_success rather than issuing
		// ChatGPT cookies. Password acceptance is already persisted above.
		reportAccountRepairProgress(ctx, "使用保存密码和已保存 2FA 验证登录并获取令牌")
		if err := resetRegisterAuthSession(session); err != nil {
			return nil, err
		}
		tokens, err := session.loginWithPasswordContinuation(ctx, mailbox.Address, password, session.loginMFASecret, nil, waitCode)
		if err != nil {
			return nil, fmt.Errorf("密码流程后的登录未完成: %w", err)
		}
		if !session.passwordConfirmed {
			return nil, errors.New("重新登录未验证保存密码，保留待修复状态")
		}
		if passwordErr != nil {
			// Reuse rejection may refer to a historical password. Only a real
			// successful password login confirms the stored candidate.
			if err := flow.accepted(); err != nil {
				return nil, err
			}
		}
		tokens["email"], tokens["password_confirmed"] = mailbox.Address, true
		return tokens, nil
	}
	return flow.session()
}

func waitRegisterBrowserPath(ctx context.Context, path, selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	selectorJSON, _ := json.Marshal(selector)
	for time.Now().Before(deadline) {
		location, err := chromedp.Run(ctx, chromedp.Location())
		if err == nil {
			parsed, parseErr := url.Parse(location)
			if parseErr == nil && parsed.Hostname() == "auth.openai.com" && parsed.Path == path {
				ready, readyErr := chromedp.Run(ctx, chromedp.Evaluate[bool](`document.readyState === 'complete' && !!document.querySelector(`+string(selectorJSON)+`)`))
				if readyErr == nil && ready {
					return nil
				}
			}
		}
		if !sleepContext(ctx, time.Second) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("等待 %s 超时", path)
}

func waitRegisterBrowserHost(ctx context.Context, host string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		location, err := chromedp.Run(ctx, chromedp.Location())
		if err == nil {
			parsed, parseErr := url.Parse(location)
			if parseErr == nil && parsed.Hostname() == host {
				return nil
			}
		}
		if !sleepContext(ctx, time.Second) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("等待 %s 超时", host)
}
