package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

type webRegisterFlow struct {
	ctx             context.Context
	email, password string
	profile         registerProfileConfig
	waitCode        func(context.Context, time.Time) (string, error)
	trace           *registerBrowserTrace
	cutoff          time.Time
}

func (f *webRegisterFlow) step(name string, action func() error) error {
	return runRegisterBrowserStep(f.ctx, "signup", name, action)
}

func (f *webRegisterFlow) authorize() error {
	return f.step(browserStepAuthorize, func() error {
		f.cutoff = time.Now().UTC().Truncate(time.Second)
		return authorizeRegisterBrowser(f.ctx, "", "")
	})
}

func (f *webRegisterFlow) submitEmail(state webRegisterPage) error {
	return f.submitAndAdvance(browserStepEmail, state, func() error {
		f.cutoff = time.Now().UTC().Truncate(time.Second)
		if err := fillRegisterBrowserField(f.ctx, `input[type="email"]`, f.email); err != nil {
			return err
		}
		return submitRegisterBrowserField(f.ctx)
	})
}

func (f *webRegisterFlow) submitPassword(state webRegisterPage, signup bool) error {
	name, path := browserStepVerifyPassword, "/api/accounts/password/verify"
	if signup {
		name, path = browserStepRegisterPassword, "/api/accounts/user/register"
	}
	return f.submitAndAdvance(name, state, func() error {
		f.cutoff = time.Now().UTC().Truncate(time.Second)
		cursor := f.trace.cursor()
		if err := fillRegisterBrowserPassword(f.ctx, f.password); err != nil {
			return err
		}
		if err := submitRegisterBrowserPassword(f.ctx); err != nil {
			return err
		}
		_, err := f.trace.wait(f.ctx, cursor, path)
		return err
	})
}

func (f *webRegisterFlow) verifyEmail(state webRegisterPage) error {
	cursor := f.trace.cursor()
	if err := verifyRegisterBrowserEmail(f.ctx, "signup", f.cutoff, f.waitCode, func(code string) error {
		return validateRegisterBrowserCode(f.ctx, f.trace, code)
	}, func(ctx context.Context) (time.Time, error) { return resendRegisterBrowserCode(ctx, f.trace) }); err != nil {
		return err
	}
	return f.step("email_code_navigation", func() error { return f.waitNextPage(state, cursor) })
}

func (f *webRegisterFlow) submitProfile(state webRegisterPage) error {
	return f.submitAndAdvance(browserStepProfile, state, func() error {
		cursor := f.trace.cursor()
		if err := fillWebRegisterProfile(f.ctx, f.profile); err != nil {
			return err
		}
		// Mark the already-filled name field to select its own form's button.
		if err := markRegisterBrowserField(f.ctx, `input[name="name"]`); err != nil {
			return err
		}
		if err := submitRegisterBrowserField(f.ctx); err != nil {
			return err
		}
		_, err := f.trace.wait(f.ctx, cursor, "/api/accounts/create_account")
		return err
	})
}

func (f *webRegisterFlow) submitAndAdvance(name string, state webRegisterPage, submit func() error) error {
	return f.step(name, func() error {
		cursor := f.trace.cursor()
		if err := submit(); err != nil {
			return err
		}
		return f.waitNextPage(state, cursor)
	})
}

func (f *webRegisterFlow) waitNextPage(from webRegisterPage, cursor int) error {
	ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		state, err := chromedp.Run(ctx, chromedp.Evaluate[webRegisterPage](webRegisterPageScript))
		if err == nil {
			if state.Host != from.Host || state.Path != from.Path {
				return nil
			}
			if state.Error != "" {
				return fmt.Errorf("%w; %s", webRegisterPageError(state, f.email, f.password), f.trace.diagnostic(cursor))
			}
		}
		if !sleepContext(ctx, 250*time.Millisecond) {
			break
		}
	}
	return fmt.Errorf("页面 %s 未进入下一步; %s: %w", from.Path, f.trace.diagnostic(cursor), ctx.Err())
}

func (f *webRegisterFlow) session() (map[string]any, error) {
	var auth map[string]any
	err := f.step(browserStepSession, func() error {
		ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
		defer cancel()
		var lastErr error
		for ctx.Err() == nil {
			auth, lastErr = readRegisterBrowserSession(ctx, f.email)
			if lastErr == nil {
				auth["password_confirmed"] = f.trace.passwordAccepted("/api/accounts/user/register", "/api/accounts/password/verify")
				return nil
			}
			if !sleepContext(ctx, 500*time.Millisecond) {
				break
			}
		}
		return fmt.Errorf("未取得同一账号的登录会话（%v）: %w", lastErr, ctx.Err())
	})
	return auth, err
}

// Page scripts may submit OTP automatically on the last digit. Observe that
// request before considering an explicit click, then validate the actual server
// response separately from waiting for the page to navigate.
func validateRegisterBrowserCode(ctx context.Context, trace *registerBrowserTrace, code string) error {
	cursor := trace.cursor()
	if strings.TrimSpace(code) == "" {
		return errors.New("邮箱验证码为空")
	}
	if err := fillRegisterBrowserField(ctx, `input[name="code"]`, strings.TrimSpace(code)); err != nil {
		return fmt.Errorf("填写邮箱验证码: %w", err)
	}
	const path = "/api/accounts/email-otp/validate"
	until := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(until) {
		if _, ok := trace.latest(cursor, path); ok {
			break
		}
		if !sleepContext(ctx, 50*time.Millisecond) {
			return ctx.Err()
		}
	}
	if _, ok := trace.latest(cursor, path); !ok {
		state, err := chromedp.Run(ctx, chromedp.Evaluate[webRegisterPage](webRegisterPageScript))
		if err != nil {
			return err
		}
		if state.OTPReady {
			if err := submitRegisterBrowserField(ctx); err != nil {
				return fmt.Errorf("提交邮箱验证码: %w", err)
			}
		}
	}
	response, err := trace.wait(ctx, cursor, path)
	if err != nil {
		return err
	}
	if err := response.validateCode(); err != nil {
		return err
	}
	reportAccountRepairProgress(ctx, "邮箱验证码校验通过："+response.summary())
	return nil
}
