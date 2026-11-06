package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// Use a fresh ChatGPT Web authorization, as in qweqwe.har. A candidate password
// is submitted only when the actual page offers signup or password verification.
// The browser runs the page's own CSRF, Sentinel and callback code.
func registerPendingInBrowser(ctx context.Context, session *registerSession, email, password string, profile registerProfileConfig, waitCode func(context.Context, time.Time) (string, error)) (map[string]any, error) {
	if email == "" || password == "" || waitCode == nil {
		return nil, errors.New("ChatGPT Web 接续缺少邮箱、候选密码或取码方式")
	}
	solver := &browserSentinelSolver{proxy: session.proxyRoute.URL, userAgent: session.userAgent}
	defer solver.close()
	if err := solver.ensurePage(ctx); err != nil {
		return nil, fmt.Errorf("打开 ChatGPT Web 注册浏览器: %w", err)
	}
	runCtx, cancel := solver.runContext(ctx, 8*time.Minute)
	defer cancel()
	if progress, ok := ctx.Value(accountRepairProgressKey{}).(func(string)); ok {
		runCtx = context.WithValue(runCtx, accountRepairProgressKey{}, progress)
	}
	trace, err := observeRegisterBrowser(runCtx, email)
	if err != nil {
		return nil, err
	}
	flow := &webRegisterFlow{ctx: runCtx, email: email, password: password, profile: profile, waitCode: waitCode, trace: trace}
	if err := flow.authorize(); err != nil {
		return nil, err
	}
	lastPath := ""
	pathSince := time.Now()
	// Bound unexpected redirect loops without preventing a legitimate return to
	// a previously visited page after an intervening verification step.
	visits := map[string]int{}
	for runCtx.Err() == nil {
		state, err := chromedp.Run(runCtx, chromedp.Evaluate[webRegisterPage](webRegisterPageScript))
		if err != nil {
			if !sleepContext(runCtx, 250*time.Millisecond) {
				break
			}
			continue
		}
		if state.Host+state.Path != lastPath {
			lastPath = state.Host + state.Path
			pathSince = time.Now()
			visits[lastPath]++
			reportAccountRepairProgress(ctx, "ChatGPT Web 页面："+lastPath)
			if visits[lastPath] > 3 {
				return nil, fmt.Errorf("ChatGPT Web 页面重复进入过多：%s，保留待接续记录", state.Path)
			}
		}
		if state.Host == "chatgpt.com" && !strings.HasPrefix(state.Path, "/api/auth/callback/") {
			// Callback/session cookies may take a short time to become visible.
			return flow.session()
		} else if state.Host == "auth.openai.com" {
			if err := registerIdentityVerificationError(state.Path); err != nil {
				return nil, err
			}
			handled := true
			switch {
			case state.EmailReady && !strings.Contains(state.Path, "password"):
				err = flow.submitEmail(state)
			case state.Path == "/create-account/password" && state.PasswordReady:
				err = flow.submitPassword(state, true)
			case state.Path == "/log-in/password" && state.PasswordReady:
				err = flow.submitPassword(state, false)
			case state.Path == "/email-verification" && state.OTPReady:
				err = flow.verifyEmail(state)
			case state.Path == "/about-you" && state.NameReady:
				err = flow.submitProfile(state)
			default:
				handled = false
			}
			if err != nil {
				return nil, err
			}
			if !handled && (state.Error != "" || time.Since(pathSince) > 45*time.Second) {
				return nil, webRegisterPageError(state, email, password)
			}
		} else if time.Since(pathSince) > 45*time.Second {
			return nil, errors.New("ChatGPT Web 未进入可识别的接续页面，保留原记录")
		}
		if !sleepContext(runCtx, 300*time.Millisecond) {
			break
		}
	}
	return nil, fmt.Errorf("ChatGPT Web 注册接续中断: %w", runCtx.Err())
}

type webRegisterPage struct {
	Host          string `json:"host"`
	Path          string `json:"path"`
	EmailReady    bool   `json:"email_ready"`
	PasswordReady bool   `json:"password_ready"`
	OTPReady      bool   `json:"otp_ready"`
	NameReady     bool   `json:"name_ready"`
	Error         string `json:"error"`
}

const webRegisterPageScript = `(()=>{
 const ready=s=>[...document.querySelectorAll(s)].some(e=>!e.disabled&&e.getClientRects().length>0);
 return {host:location.hostname,path:location.pathname,email_ready:ready('input[type="email"]'),password_ready:ready('input[type="password"]'),otp_ready:ready('input[name="code"]'),name_ready:ready('input[name="name"]'),error:[...document.querySelectorAll('[role="alert"]')].map(e=>e.innerText).filter(Boolean).join(' ').slice(0,300)};
})()`

const webRegisterSessionScript = `(async()=>{const r=await fetch('/api/auth/session',{credentials:'include'});if(!r.ok)return {};const j=await r.json();return {access_token:j.accessToken||'',email:j.user?.email||''}})()`

func webRegisterPageError(state webRegisterPage, email, password string) error {
	message := strings.ReplaceAll(strings.ReplaceAll(state.Error, password, "[redacted]"), email, "[email]")
	return fmt.Errorf("ChatGPT Web 注册未完成，当前步骤 %s: %s", state.Path, firstNonEmpty(message, "页面未进入下一步，保留原记录"))
}

func fillWebRegisterProfile(ctx context.Context, profile registerProfileConfig) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.SendKeys(chromedp.CSS(`input[name="name"]`), profile.name())); err != nil {
		return fmt.Errorf("填写账号姓名失败: %w", err)
	}
	birthdate, err := time.Parse("2006-01-02", profile.birthdate())
	if err != nil {
		return errors.New("注册生日格式无效")
	}
	// Some versions ask for age and derive the hidden birthday themselves.
	ageInput, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(()=>{const e=document.querySelector('input[name="age"]');return !!e&&!e.disabled&&e.getClientRects().length>0})()`))
	if err != nil {
		return fmt.Errorf("读取账号资料控件失败: %w", err)
	}
	if ageInput {
		now := time.Now().UTC()
		age := now.Year() - birthdate.Year()
		if now.Month() < birthdate.Month() || (now.Month() == birthdate.Month() && now.Day() < birthdate.Day()) {
			age--
		}
		if err := chromedp.Do(ctx, chromedp.SendKeys(chromedp.CSS(`input[name="age"]`), fmt.Sprint(age))); err != nil {
			return fmt.Errorf("填写账号年龄失败: %w", err)
		}
		return nil
	}
	// The auth page uses an accessible segmented date input on some versions.
	for _, part := range []struct{ name, value string }{{"month", birthdate.Format("01")}, {"day", birthdate.Format("02")}, {"year", birthdate.Format("2006")}} {
		selector := `[role="spinbutton"][data-type="` + part.name + `"]`
		selectorJSON, _ := json.Marshal(selector)
		found, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`!!document.querySelector(`+string(selectorJSON)+`)`))
		if err != nil || !found {
			controls, _ := chromedp.Run(ctx, chromedp.Evaluate[string](`JSON.stringify([...document.querySelectorAll('input,select,[role="spinbutton"]')].map(e=>({tag:e.tagName,name:e.name,type:e.type,role:e.getAttribute('role'),part:e.getAttribute('data-type'),placeholder:e.getAttribute('placeholder'),label:e.getAttribute('aria-label'),visible:e.getClientRects().length>0})))`))
			return fmt.Errorf("账号生日输入控件与预期不符，保留待接续记录; controls=%s", controls)
		}
		if err := chromedp.Do(ctx, chromedp.Click(chromedp.CSS(selector)), chromedp.SendKeys(chromedp.CSS(selector), part.value)); err != nil {
			return fmt.Errorf("填写生日 %s 失败: %w", part.name, err)
		}
	}
	return nil
}
