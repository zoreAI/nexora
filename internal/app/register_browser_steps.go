package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// These identifiers are shared by the Web signup and password recovery flows.
// A step completes only after its response/navigation has been checked.
const (
	browserStepAuthorize        = "authorize"
	browserStepEmail            = "submit_email"
	browserStepRegisterPassword = "register_password"
	browserStepVerifyPassword   = "verify_password"
	browserStepWaitCode         = "wait_email_code"
	browserStepValidateCode     = "validate_email_code"
	browserStepProfile          = "create_profile"
	browserStepSetPassword      = "set_password"
	browserStepSession          = "read_session"
)

func runRegisterBrowserStep(ctx context.Context, flow, step string, action func() error) error {
	started := time.Now()
	reportAccountRepairProgress(ctx, fmt.Sprintf("浏览器[%s] 步骤[%s] 开始", flow, step))
	err := action()
	elapsed := time.Since(started).Round(time.Millisecond)
	if err != nil {
		reportAccountRepairProgress(ctx, fmt.Sprintf("浏览器[%s] 步骤[%s] 失败，耗时%s", flow, step, elapsed))
		return fmt.Errorf("浏览器[%s] 步骤[%s] 失败（耗时%s）: %w", flow, step, elapsed, err)
	}
	reportAccountRepairProgress(ctx, fmt.Sprintf("浏览器[%s] 步骤[%s] 完成，耗时%s", flow, step, elapsed))
	return nil
}

type registerBrowserRequest struct {
	sequence int
	path     string
	result   registerBrowserOTPResponse
	complete bool
}

// Keep only whitelisted protocol metadata in memory. Raw response bodies,
// authorization URLs, cookies, codes and passwords must never reach logs.
type registerBrowserTrace struct {
	mu       sync.Mutex
	sequence int
	requests map[network.RequestID]registerBrowserRequest
	email    string
}

func observeRegisterBrowser(ctx context.Context, email string) (*registerBrowserTrace, error) {
	return observeRegisterBrowserPaths(ctx, email, registerBrowserAccountPath)
}

func observeRegisterBrowserPaths(ctx context.Context, email string, accountPath func(string) string) (*registerBrowserTrace, error) {
	if err := retainRegisterBrowserResponses(ctx); err != nil {
		return nil, fmt.Errorf("初始化浏览器响应采集失败: %w", err)
	}
	trace := &registerBrowserTrace{email: email, requests: make(map[network.RequestID]registerBrowserRequest)}
	requests := chromedp.Events(ctx, network.RequestWillBeSent)
	responses := chromedp.Events(ctx, network.ResponseReceived)
	failures := chromedp.Events(ctx, network.LoadingFailed)
	go func() {
		for event, err := range requests {
			if err != nil {
				return
			}
			if path := accountPath(event.Request.URL); path != "" {
				trace.begin(event.RequestID, path)
			}
		}
	}()
	go func() {
		for event, err := range responses {
			if err != nil {
				return
			}
			path := accountPath(event.Response.URL)
			if path == "" {
				continue
			}
			trace.begin(event.RequestID, path)
			result := readRegisterBrowserOTPResponse(ctx, event.RequestID, event.Response.Status, email)
			trace.finish(event.RequestID, result)
			reportAccountRepairProgress(ctx, "浏览器响应："+path+" "+result.summary())
		}
	}()
	go func() {
		for event, err := range failures {
			if err != nil {
				return
			}
			trace.mu.Lock()
			if req, ok := trace.requests[event.RequestID]; ok && !req.complete {
				req.complete = true
				req.result.bodyError = "网络请求未完成"
				trace.requests[event.RequestID] = req
			}
			trace.mu.Unlock()
		}
	}()
	return trace, nil
}

func registerBrowserAccountPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() != "auth.openai.com" {
		return ""
	}
	// Do not store dynamic path segments or query parameters from the provider.
	switch u.Path {
	case "/api/accounts/authorize/continue", "/api/accounts/user/register", "/api/accounts/password/verify",
		"/api/accounts/passwordless/send-otp", "/api/accounts/email-otp/send", "/api/accounts/email-otp/resend",
		"/api/accounts/email-otp/validate", "/api/accounts/create_account", "/api/accounts/password/add", "/api/accounts/password/reset":
		return u.Path
	}
	return ""
}

func (t *registerBrowserTrace) begin(id network.RequestID, path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.requests[id]; !ok {
		t.sequence++
		t.requests[id] = registerBrowserRequest{sequence: t.sequence, path: path}
	}
}

func (t *registerBrowserTrace) finish(id network.RequestID, result registerBrowserOTPResponse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	req := t.requests[id]
	req.result, req.complete = result, true
	t.requests[id] = req
}

func (t *registerBrowserTrace) cursor() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sequence
}

func (t *registerBrowserTrace) latest(after int, paths ...string) (registerBrowserRequest, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var latest registerBrowserRequest
	for _, req := range t.requests {
		if req.sequence <= after || req.sequence <= latest.sequence {
			continue
		}
		for _, path := range paths {
			if req.path == path {
				latest = req
				break
			}
		}
	}
	return latest, latest.sequence != 0
}

func (t *registerBrowserTrace) wait(ctx context.Context, after int, paths ...string) (registerBrowserOTPResponse, error) {
	responseCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if req, ok := t.latest(after, paths...); ok && req.complete {
			return req.result, req.result.accepted()
		}
		if !sleepContext(responseCtx, 100*time.Millisecond) {
			return registerBrowserOTPResponse{}, fmt.Errorf("未取得 %s 的完成响应: %w", strings.Join(paths, ","), responseCtx.Err())
		}
	}
}

func (t *registerBrowserTrace) passwordAccepted(paths ...string) bool {
	req, ok := t.latest(-1, paths...)
	return ok && req.complete && req.result.accepted() == nil
}

func (t *registerBrowserTrace) diagnostic(after int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var latest registerBrowserRequest
	for _, req := range t.requests {
		if req.sequence > after && req.sequence > latest.sequence {
			latest = req
		}
	}
	if latest.sequence == 0 {
		return "未观察到账号接口请求"
	}
	if !latest.complete {
		return latest.path + " 请求仍未完成"
	}
	return latest.path + " " + latest.result.summary()
}

func authorizeRegisterBrowser(ctx context.Context, email, passwordFlow string) error {
	emailJSON, _ := json.Marshal(email)
	flowJSON, _ := json.Marshal(passwordFlow)
	script := fmt.Sprintf(`(async()=>{
 const c=await fetch('/api/auth/csrf',{credentials:'include'});
 if(!c.ok)throw Error('csrf HTTP '+c.status);
 const csrf=(await c.json()).csrfToken;
 const q=new URLSearchParams({prompt:'login',screen_hint:'login_or_signup'});
 const flow=%s;
 if(flow){q.set('reauth','password');q.set('max_age','0');q.set('login_hint',%s);q.set(flow,'true')}
 const body=new URLSearchParams({callbackUrl:flow?'https://chatgpt.com/':'https://chatgpt.com/settings/security',csrfToken:csrf,json:'true'});
 const r=await fetch('/api/auth/signin/openai?'+q,{method:'POST',headers:{'content-type':'application/x-www-form-urlencoded'},credentials:'include',body});
 if(!r.ok)throw Error('signin HTTP '+r.status);
 return (await r.json()).url||'';
 })()`, flowJSON, emailJSON)
	rawURL, err := chromedp.Run(ctx, chromedp.Evaluate[string](script, chromedp.EvalAwaitPromise))
	if err != nil {
		return fmt.Errorf("建立授权: %w", err)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "auth.openai.com" || u.Path != "/oauth/authorize" || (passwordFlow != "" && u.Query().Get(passwordFlow) != "true") {
		return errors.New("浏览器授权地址无效")
	}
	return chromedp.Do(ctx, chromedp.Navigate(rawURL))
}

// Mark the visible field and its own form. A hidden honeypot or another form's
// submit button must not be selected by the action methods.
func fillRegisterBrowserField(ctx context.Context, selector, value string) error {
	if err := markRegisterBrowserField(ctx, selector); err != nil {
		return err
	}
	return chromedp.Do(ctx, chromedp.SendKeys(chromedp.CSS(`[data-nexora-step-field="true"]`), value))
}

func markRegisterBrowserField(ctx context.Context, selector string) error {
	selectorJSON, _ := json.Marshal(selector)
	script := `(()=>{
 document.querySelectorAll('[data-nexora-step-field]').forEach(e=>e.removeAttribute('data-nexora-step-field'));
 const e=[...document.querySelectorAll(` + string(selectorJSON) + `)].find(e=>!e.disabled&&e.getClientRects().length>0);
 if(!e||!e.form)return false;e.setAttribute('data-nexora-step-field','true');e.focus();e.select();return true;
 })()`
	ready, err := chromedp.Run(ctx, chromedp.Evaluate[bool](script))
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("当前页面没有可用的目标表单控件")
	}
	return nil
}

func submitRegisterBrowserField(ctx context.Context) error {
	submitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	const script = `(()=>{
 const field=document.querySelector('[data-nexora-step-field="true"]');
 const form=field?.form;if(!form)return false;
 document.querySelectorAll('[data-nexora-step-submit]').forEach(e=>e.removeAttribute('data-nexora-step-submit'));
 const e=[...form.querySelectorAll('button[type="submit"],input[type="submit"]')].find(e=>e.getClientRects().length>0&&!e.disabled&&e.getAttribute('aria-disabled')!=='true');
 if(!e)return false;e.setAttribute('data-nexora-step-submit','true');return true;
 })()`
	for submitCtx.Err() == nil {
		ready, err := chromedp.Run(submitCtx, chromedp.Evaluate[bool](script))
		if err == nil && ready {
			return chromedp.Do(submitCtx, chromedp.Click(chromedp.CSS(`[data-nexora-step-submit="true"]`)))
		}
		if !sleepContext(submitCtx, 150*time.Millisecond) {
			break
		}
	}
	return fmt.Errorf("当前表单没有可用的提交按钮: %w", submitCtx.Err())
}

func readRegisterBrowserSession(ctx context.Context, email string) (map[string]any, error) {
	auth, err := chromedp.Run(ctx, chromedp.Evaluate[map[string]any](webRegisterSessionScript, chromedp.EvalAwaitPromise))
	if err != nil {
		return nil, fmt.Errorf("读取登录会话: %w", err)
	}
	if strings.TrimSpace(strAny(auth["access_token"], "")) == "" {
		return nil, errors.New("未取得已登录会话")
	}
	if !strings.EqualFold(strings.TrimSpace(strAny(auth["email"], "")), strings.TrimSpace(email)) {
		return nil, errors.New("登录会话邮箱与待接续记录不一致")
	}
	return auth, nil
}

func resendRegisterBrowserCode(ctx context.Context, trace *registerBrowserTrace) (time.Time, error) {
	cursor := trace.cursor()
	cutoff, err := clickRegisterBrowserOTPResend(ctx)
	if err != nil {
		return time.Time{}, err
	}
	response, err := trace.wait(ctx, cursor, "/api/accounts/passwordless/send-otp", "/api/accounts/email-otp/resend", "/api/accounts/email-otp/send")
	if err != nil {
		return cutoff, err
	}
	if err := response.validate(); err != nil {
		return cutoff, err
	}
	return cutoff, nil
}

// A rejected OTP is recoverable, unlike a deactivated account or a network
// failure. Request one fresh code through the page and keep the new mail cutoff.
func verifyRegisterBrowserEmail(ctx context.Context, flow string, cutoff time.Time,
	waitCode func(context.Context, time.Time) (string, error), validate func(string) error,
	resend func(context.Context) (time.Time, error)) error {
	for attempt := 0; attempt < 2; attempt++ {
		var code string
		if err := runRegisterBrowserStep(ctx, flow, browserStepWaitCode, func() error {
			var err error
			if attempt == 0 {
				code, err = waitRegisterBrowserCode(ctx, cutoff, time.Minute, waitCode, resend)
			} else {
				code, err = waitCode(ctx, cutoff)
			}
			return err
		}); err != nil {
			return err
		}
		err := runRegisterBrowserStep(ctx, flow, browserStepValidateCode, func() error { return validate(code) })
		if err == nil {
			return nil
		}
		if attempt > 0 || ctx.Err() != nil || !isWrongRegisterBrowserCode(err) {
			return err
		}
		reportAccountRepairProgress(ctx, "验证码已失效或不匹配，页面重发后重新校验（最多一次）")
		if err := runRegisterBrowserStep(ctx, flow, "resend_email_code", func() error {
			var err error
			cutoff, err = resend(ctx)
			return err
		}); err != nil {
			return err
		}
	}
	return errors.New("邮箱验证码校验未完成")
}

func registerBrowserPageDiagnostic(ctx context.Context) string {
	const script = `(()=>({host:location.hostname,path:location.pathname,ready:document.readyState,
 fields:[...document.querySelectorAll('input')].map(e=>({name:e.name,type:e.type,visible:e.getClientRects().length>0,disabled:e.disabled,readOnly:e.readOnly})),
 buttons:[...document.querySelectorAll('button,input[type="submit"]')].map(e=>({name:e.name,type:e.type,visible:e.getClientRects().length>0,disabled:e.disabled,resend:/(resend|send.*again|重新发送|重发)/i.test(e.innerText||''),forgotPassword:/forgot.*password|忘记密码/i.test(e.innerText||''),emailCode:/email.*code|code.*email|one.time code|验证码登录/i.test(e.innerText||'')})),
 links:[...document.querySelectorAll('a[href]')].filter(e=>e.getClientRects().length>0).map(e=>({path:new URL(e.href,location.href).pathname,forgotPassword:/forgot.*password|忘记密码/i.test(e.innerText||''),emailCode:/email.*code|code.*email|one.time code|验证码登录/i.test(e.innerText||'')})),
 alerts:document.querySelectorAll('[role="alert"]').length}))()`
	result, err := chromedp.Run(ctx, chromedp.Evaluate[map[string]any](script))
	if err != nil {
		return "页面控件状态暂不可读"
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "页面控件状态暂不可读"
	}
	return "page_controls=" + string(data)
}
