package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Retain only protocol metadata. Never log the auth-session body or credentials.
type registerBrowserOTPResponse struct {
	status       int64
	pageType     string
	nextPath     string
	errorCode    string
	bodyRead     bool
	bodyError    string
	emailPresent bool
	emailMatches bool
	sendPending  *bool
}

func parseRegisterBrowserOTPResponse(status int64, body []byte, email string) registerBrowserOTPResponse {
	result := registerBrowserOTPResponse{status: status}
	var payload struct {
		ContinueURL string `json:"continue_url"`
		Page        struct {
			Type string `json:"type"`
		} `json:"page"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Session struct {
			Email       string          `json:"email"`
			Username    json.RawMessage `json:"username"`
			SendPending *bool           `json:"passwordless_email_otp_send_pending"`
		} `json:"oai-client-auth-session"`
	}
	if json.Unmarshal(body, &payload) != nil {
		format := "非 JSON"
		if json.Valid(body) {
			format = "JSON 结构不匹配"
		} else if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
			format = "HTML"
		}
		result.bodyError = fmt.Sprintf("%s 响应，正文%d字节", format, len(body))
		return result
	}
	result.bodyRead = true
	result.pageType = safeRegisterBrowserIdentifier(payload.Page.Type)
	result.nextPath = safeRegisterBrowserNextPath(payload.ContinueURL)
	result.errorCode = safeRegisterBrowserIdentifier(payload.Error.Code)
	result.sendPending = payload.Session.SendPending
	actual := strings.TrimSpace(payload.Session.Email)
	if actual == "" {
		var username string
		if json.Unmarshal(payload.Session.Username, &username) != nil {
			var structured struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			if json.Unmarshal(payload.Session.Username, &structured) == nil && structured.Kind == "email" {
				username = structured.Value
			}
		}
		actual = strings.TrimSpace(username)
	}
	result.emailPresent = actual != ""
	result.emailMatches = result.emailPresent && strings.EqualFold(actual, strings.TrimSpace(email))
	return result
}

func (r registerBrowserOTPResponse) validate() error {
	if r.status != 200 || r.errorCode != "" {
		return fmt.Errorf("验证码发送请求未被接受: HTTP %d code=%s", r.status, r.errorCode)
	}
	if !r.bodyRead {
		return fmt.Errorf("验证码发送响应未能读取，不能仅凭 HTTP 200 确认发送步骤: %s", r.bodyError)
	}
	if r.pageType != "email_otp_verification" {
		return fmt.Errorf("验证码发送响应未进入邮箱验证步骤: %s", r.summary())
	}
	if !r.emailPresent || !r.emailMatches {
		return fmt.Errorf("验证码发送响应未绑定当前账号邮箱，已停止等待: %s", r.summary())
	}
	return nil
}

func readRegisterBrowserOTPResponse(ctx context.Context, id network.RequestID, status int64, email string) registerBrowserOTPResponse {
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lastError := ""
	for attempt := 0; attempt < 10; attempt++ {
		body, err := chromedp.Call(readCtx, network.GetResponseBody, network.GetResponseBodyParams{RequestID: id})
		if err == nil {
			return parseRegisterBrowserOTPResponse(status, body.Body, email)
		}
		lastError = err.Error()
		if !sleepContext(readCtx, 100*time.Millisecond) {
			break
		}
	}
	return registerBrowserOTPResponse{status: status, bodyError: lastError}
}

func retainRegisterBrowserResponses(ctx context.Context) error {
	// The auth page navigates immediately after the fetch. Retain its response
	// across renderer changes so reading the send result does not race navigation.
	total, resource := int64(16<<20), int64(1<<20)
	durable := true
	_, err := chromedp.Call(ctx, network.Enable, network.EnableParams{
		MaxTotalBufferSize: &total, MaxResourceBufferSize: &resource, EnableDurableMessages: &durable,
	})
	return err
}

func waitRegisterBrowserOTPResponse(ctx context.Context, responses <-chan registerBrowserOTPResponse) error {
	responseCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case response := <-responses:
		if err := response.validate(); err != nil {
			return err
		}
		pending := "unknown"
		if response.sendPending != nil {
			pending = fmt.Sprint(*response.sendPending)
		}
		reportAccountRepairProgress(ctx, "浏览器验证码请求已接受：邮箱匹配，下一步=email_otp_verification，send_pending="+pending)
		return nil
	case <-responseCtx.Done():
		return fmt.Errorf("未取得验证码发送响应，停止空等: %w", responseCtx.Err())
	}
}

func waitRegisterBrowserCode(ctx context.Context, cutoff time.Time, firstWait time.Duration,
	waitCode func(context.Context, time.Time) (string, error), resend func(context.Context) (time.Time, error)) (string, error) {
	firstCtx, cancel := context.WithTimeout(ctx, firstWait)
	code, err := waitCode(firstCtx, cutoff)
	cancel()
	if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
		return code, err
	}
	reportAccountRepairProgress(ctx, "未收到邮箱验证码，尝试页面重发（最多一次）")
	cutoff, err = resend(ctx)
	if err != nil {
		// A browser navigation can detach the response body even though the
		// resend click was accepted and the provider sent a new code. Keep the
		// fresh mailbox boundary and continue polling instead of discarding that
		// attempt immediately. A zero boundary still means the click itself did
		// not happen, so preserve the original error.
		if !cutoff.IsZero() && ctx.Err() == nil {
			reportAccountRepairProgress(ctx, "页面重发响应未完整返回，继续读取新验证码")
			code, waitErr := waitCode(ctx, cutoff)
			if waitErr == nil {
				return code, nil
			}
			return "", fmt.Errorf("页面重发响应异常，继续等待验证码失败: %w（原响应: %v）", waitErr, err)
		}
		return "", fmt.Errorf("未收到验证码，页面重发失败: %w", err)
	}
	reportAccountRepairProgress(ctx, "页面重发请求已接受，等待新邮箱验证码")
	return waitCode(ctx, cutoff)
}

// Click the page's own resend control so CSRF, Sentinel and session binding
// remain managed by the page. Respect its disabled/cooldown state.
func clickRegisterBrowserOTPResend(ctx context.Context) (time.Time, error) {
	clickCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	const script = `(()=>{
 if(location.hostname!=='auth.openai.com'||location.pathname!=='/email-verification')return false;
 const e=[...document.querySelectorAll('button,a,[role="button"],input[type="submit"]')].find(e=>e.getClientRects().length>0&&!e.disabled&&e.getAttribute('aria-disabled')!=='true'&&/(resend|send.*again|重新发送|重新傳送|重发|重發|再次发送)/i.test(e.innerText||e.value||''));
 if(!e)return false;
 e.setAttribute('data-nexora-otp-resend','true');return true;
})()`
	for clickCtx.Err() == nil {
		ready, err := chromedp.Run(clickCtx, chromedp.Evaluate[bool](script))
		if err == nil && ready {
			cutoff := time.Now().UTC().Truncate(time.Second)
			return cutoff, chromedp.Do(clickCtx, chromedp.Click(chromedp.CSS(`[data-nexora-otp-resend="true"]`)))
		}
		if !sleepContext(clickCtx, 500*time.Millisecond) {
			break
		}
	}
	if ctx.Err() != nil {
		return time.Time{}, ctx.Err()
	}
	return time.Time{}, errors.New("验证码页没有可用的重新发送按钮，保留账号供后续修复")
}

// Provider strings are untrusted. Keep only short symbolic identifiers and
// known next-step paths; a callback URL may contain live authorization codes.
func safeRegisterBrowserIdentifier(value string) string {
	if len(value) > 80 {
		return "unrecognized"
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return "unrecognized"
		}
	}
	return value
}

func safeRegisterBrowserNextPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "unrecognized"
	}
	switch u.Path {
	case "", "/email-verification", "/about-you", "/reset-password/new-password", "/reset-password/success", "/verify-your-identity", "/create-account/password", "/log-in/password", "/api/auth/callback/openai", "/api/accounts/email-otp/send":
		return u.Path
	default:
		return "other"
	}
}

func (r registerBrowserOTPResponse) summary() string {
	return fmt.Sprintf("HTTP %d code=%s next_page=%s next_path=%s body_read=%t", r.status, firstNonEmpty(r.errorCode, "none"), firstNonEmpty(r.pageType, "unknown"), firstNonEmpty(r.nextPath, "unknown"), r.bodyRead)
}

func (r registerBrowserOTPResponse) accepted() error {
	if r.status != 200 || r.errorCode != "" {
		return &registerBrowserResponseError{response: r}
	}
	if !r.bodyRead {
		return fmt.Errorf("账号接口响应未能读取: %s; %s", r.summary(), r.bodyError)
	}
	if r.emailPresent && !r.emailMatches {
		return errors.New("账号接口响应绑定的邮箱与当前账号不一致")
	}
	return nil
}

type registerBrowserResponseError struct{ response registerBrowserOTPResponse }

func (e *registerBrowserResponseError) Error() string {
	return "账号接口请求未被接受: " + e.response.summary()
}

func isWrongRegisterBrowserCode(err error) bool {
	var responseErr *registerBrowserResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.response.errorCode {
	case "wrong_email_otp_code", "invalid_otp", "expired_otp", "email_otp_expired":
		return true
	}
	return false
}

func (r registerBrowserOTPResponse) validateCode() error {
	if err := r.accepted(); err != nil {
		return err
	}
	if r.pageType == "email_otp_verification" || r.nextPath == "/email-verification" {
		return fmt.Errorf("验证码校验后仍停留在邮箱验证步骤: %s", r.summary())
	}
	if r.pageType == "" && r.nextPath == "" {
		return errors.New("验证码校验响应缺少下一步，无法确认验证成功")
	}
	return nil
}
