package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

type registerPasswordBrowserFlow struct {
	ctx             context.Context
	email, password string
	add             bool
	trace           *registerBrowserTrace
	cutoff          time.Time
	waitCode        func(context.Context, time.Time) (string, error)
	accepted        func() error
}

func (f *registerPasswordBrowserFlow) step(name string, action func() error) error {
	flow := "reset_password"
	if f.add {
		flow = "add_password"
	}
	return runRegisterBrowserStep(f.ctx, flow, name, action)
}

func (f *registerPasswordBrowserFlow) authorize() error {
	return f.step(browserStepAuthorize, func() error {
		flow := ""
		if f.add {
			flow = "post_login_add_password"
		}
		cursor := f.trace.cursor()
		f.cutoff = time.Now().UTC().Truncate(time.Second)
		if err := authorizeRegisterBrowser(f.ctx, f.email, flow); err != nil {
			return err
		}
		if err := f.waitEmailVerification(); err != nil {
			return fmt.Errorf("%w; %s; %s", err, f.trace.diagnostic(cursor), registerBrowserPageDiagnostic(f.ctx))
		}
		response, err := f.trace.wait(f.ctx, cursor, "/api/accounts/passwordless/send-otp", "/api/accounts/email-otp/resend")
		if err != nil {
			// Some authorizations render the OTP page directly without making a
			// separate browser send request. Use the page's resend control to
			// obtain a fresh, mailbox-bound response instead of abandoning it.
			_, observed := f.trace.latest(cursor, "/api/accounts/passwordless/send-otp", "/api/accounts/email-otp/resend")
			if !observed && errors.Is(err, context.DeadlineExceeded) && f.ctx.Err() == nil {
				// The authorization may have sent the OTP before the browser
				// observer attached, or may render an already-issued OTP page.
				// Do not immediately resend: rate limiting would invalidate the
				// original code. The mailbox boundary still prevents stale reuse;
				// the normal wait step can request one resend only after timeout.
				reportAccountRepairProgress(f.ctx, "授权已进入邮箱验证，未观察到独立发送响应，继续读取本轮验证码")
				return nil
			}
			return fmt.Errorf("%w; %s; %s", err, f.trace.diagnostic(cursor), registerBrowserPageDiagnostic(f.ctx))
		}
		return response.validate()
	})
}

// With no authenticated browser session, an existing-password account can land
// on the login form even when post_login_password_reset was requested. Follow
// the page's normal reset-password link and email form before waiting for OTP.
func (f *registerPasswordBrowserFlow) waitEmailVerification() error {
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	resetOpened, emailSubmitted, loginEmailSubmitted := false, false, false
	for ctx.Err() == nil {
		state, err := chromedp.Run(ctx, chromedp.Evaluate[webRegisterPage](webRegisterPageScript))
		if err == nil && state.Host == "auth.openai.com" {
			if !f.add && state.Path == "/log-in-or-create-account" && state.EmailReady && !loginEmailSubmitted {
				if err := fillRegisterBrowserField(ctx, `input[type="email"]`, f.email); err != nil {
					return err
				}
				if err := submitRegisterBrowserField(ctx); err != nil {
					return err
				}
				loginEmailSubmitted = true
			}
			if state.Path == "/email-verification" && state.OTPReady {
				return nil
			}
			if !f.add && state.Path == "/log-in/password" && !resetOpened {
				clicked, clickErr := chromedp.Run(ctx, chromedp.Evaluate[bool](`(()=>{const a=[...document.querySelectorAll('a[href]')].find(e=>{const u=new URL(e.href,location.href);return u.origin===location.origin&&u.pathname==='/reset-password'&&e.getClientRects().length>0});if(!a)return false;a.click();return true})()`))
				if clickErr != nil {
					return clickErr
				}
				if clicked {
					resetOpened = true
					f.cutoff = time.Now().UTC().Truncate(time.Second)
					reportAccountRepairProgress(f.ctx, "密码重置授权进入登录页，使用页面的忘记密码入口")
				}
			}
			if !f.add && state.Path == "/reset-password" && state.EmailReady && !emailSubmitted {
				if err := fillRegisterBrowserField(ctx, `input[type="email"]`, f.email); err != nil {
					return err
				}
				f.cutoff = time.Now().UTC().Truncate(time.Second)
				if err := submitRegisterBrowserField(ctx); err != nil {
					return err
				}
				emailSubmitted = true
				reportAccountRepairProgress(f.ctx, "已通过忘记密码表单请求邮箱验证")
			}
			if !f.add && state.Path == "/reset-password" && !state.EmailReady && !emailSubmitted {
				// Some reset pages already bind the email from the login form and
				// only ask for confirmation. Select the affirmative action by its
				// visible label, never an arbitrary first submit button.
				clicked, clickErr := chromedp.Run(ctx, chromedp.Evaluate[bool](`(()=>{const buttons=[...document.querySelectorAll('button[type="submit"]')].filter(e=>!e.disabled&&e.getClientRects().length>0&&/^(continue|send|reset)|继续|重置|发送/i.test((e.innerText||'').trim())&&!/back|cancel|返回|取消/i.test(e.innerText||''));if(buttons.length!==1)return false;buttons[0].click();return true})()`))
				if clickErr != nil {
					return clickErr
				}
				if clicked {
					emailSubmitted = true
					f.cutoff = time.Now().UTC().Truncate(time.Second)
					reportAccountRepairProgress(f.ctx, "已确认忘记密码请求，等待邮箱验证页面")
				}
			}
		}
		if !sleepContext(ctx, 250*time.Millisecond) {
			break
		}
	}
	return fmt.Errorf("等待 /email-verification 超时: %w", ctx.Err())
}

func (f *registerPasswordBrowserFlow) verifyEmail() error {
	flow := "reset_password"
	if f.add {
		flow = "add_password"
	}
	if err := verifyRegisterBrowserEmail(f.ctx, flow, f.cutoff, f.waitCode, func(code string) error {
		return validateRegisterBrowserCode(f.ctx, f.trace, code)
	}, func(ctx context.Context) (time.Time, error) { return resendRegisterBrowserCode(ctx, f.trace) }); err != nil {
		return err
	}
	return f.step("email_code_navigation", func() error {
		if err := waitRegisterBrowserPath(f.ctx, "/reset-password/new-password", `input[type="password"]`, 30*time.Second); err != nil {
			return fmt.Errorf("验证码已通过但未进入设置密码页: %w", err)
		}
		return nil
	})
}

func (f *registerPasswordBrowserFlow) setPassword() error {
	return f.step(browserStepSetPassword, func() error {
		path := "/api/accounts/password/reset"
		if f.add {
			path = "/api/accounts/password/add"
		}
		cursor := f.trace.cursor()
		if err := fillRegisterBrowserPassword(f.ctx, f.password); err != nil {
			return err
		}
		if err := submitRegisterBrowserPassword(f.ctx); err != nil {
			return err
		}
		response, err := f.trace.wait(f.ctx, cursor, path)
		if err != nil {
			return err
		}
		if f.accepted != nil {
			if err := f.accepted(); err != nil {
				return fmt.Errorf("密码已被接受但保存确认状态失败: %w", err)
			}
		}
		if !f.add && response.pageType == "reset_password_success" {
			return nil
		}
		if err := waitRegisterBrowserHost(f.ctx, "chatgpt.com", 45*time.Second); err != nil {
			return fmt.Errorf("密码提交已被接受但未返回 ChatGPT: %w", err)
		}
		return nil
	})
}

func (f *registerPasswordBrowserFlow) session() (map[string]any, error) {
	var auth map[string]any
	err := f.step(browserStepSession, func() error {
		if !f.trace.passwordAccepted("/api/accounts/password/add", "/api/accounts/password/reset") {
			return errors.New("尚未确认密码设置成功")
		}
		var lastErr error
		for attempt := 0; attempt < 10; attempt++ {
			auth, lastErr = readRegisterBrowserSession(f.ctx, f.email)
			if lastErr == nil {
				return nil
			}
			if !sleepContext(f.ctx, time.Second) {
				return f.ctx.Err()
			}
		}
		return lastErr
	})
	return auth, err
}
