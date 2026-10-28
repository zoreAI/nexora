package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func TestPasswordLoginContinuesWithEmailOTP(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	session := &registerSession{accessFlowID: "flow-id", client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Path)
		status, response := http.StatusOK, `{}`
		headers := make(http.Header)
		switch req.URL.Path {
		case "/api/accounts/authorize":
			status = http.StatusFound
			headers.Set("Location", registerAuthBase+"/log-in/password")
		case "/log-in/password":
			response = "<html></html>"
		case "/backend-api/sentinel/req":
			response = `{"token":"sentinel-token"}`
		case "/api/accounts/password/verify":
			if req.Header.Get("X-Access-Flow-Invocation-Id") != "flow-id" {
				t.Fatal("password verification lost authorization context")
			}
			response = `{"page":{"type":"email_otp_verification"}}`
		case "/api/accounts/email-otp/resend":
			if req.Header.Get("X-Access-Flow-Invocation-Id") != "flow-id" {
				t.Fatal("OTP send lost authorization context")
			}
		case "/api/accounts/email-otp/validate":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["code"] != "123456" {
				t.Fatalf("OTP body = %v, err = %v", body, err)
			}
			response = `{"continue_url":"https://platform.openai.com/auth/callback?code=oauth-code"}`
		case "/api/accounts/oauth/token":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["code"] != "oauth-code" {
				t.Fatalf("token body = %v, err = %v", body, err)
			}
			response = `{"access_token":"new-token"}`
		default:
			t.Fatalf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})}}
	codeRequested := false
	tokens, err := session.loginWithPassword(context.Background(), "user@example.com", "password", "", func(_ context.Context, notBefore time.Time) (string, error) {
		codeRequested = true
		if notBefore.IsZero() {
			t.Fatal("missing OTP cutoff time")
		}
		return "123456", nil
	})
	if err != nil || !codeRequested || tokens["access_token"] != "new-token" {
		t.Fatalf("tokens = %v, code requested = %t, err = %v", tokens, codeRequested, err)
	}
	want := "/api/accounts/authorize,/log-in/password,/backend-api/sentinel/req,/api/accounts/password/verify,/api/accounts/email-otp/resend,/backend-api/sentinel/req,/api/accounts/email-otp/validate,/api/accounts/oauth/token"
	if got := strings.Join(calls, ","); got != want {
		t.Fatalf("requests = %s, want %s", got, want)
	}
}

func TestPasswordLoginKeepsAuthorizeSession(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	authorizations := 0
	passwordVerifications := 0
	session := &registerSession{accessFlowID: "flow-id", client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		status, response := http.StatusOK, `{}`
		headers := make(http.Header)
		switch req.URL.Path {
		case "/api/accounts/authorize":
			authorizations++
			if req.URL.Query().Get("login_hint") != "user@example.com" {
				t.Fatal("password login lost its email hint")
			}
			status = http.StatusFound
			headers.Set("Location", registerAuthBase+"/log-in/password")
		case "/log-in/password":
			response = "<html></html>"
		case "/backend-api/sentinel/req":
			response = `{"token":"sentinel-token"}`
		case "/api/accounts/password/verify":
			passwordVerifications++
			if req.Header.Get("X-Access-Flow-Invocation-Id") != "flow-id" || req.Header.Get("Referer") != registerAuthBase+"/" {
				t.Fatal("password verification lost its authorization context")
			}
			response = `{"page":{"type":"email_otp_verification"}}`
		default:
			t.Fatalf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})}}
	_, err = session.loginWithPassword(context.Background(), "user@example.com", "password", "")
	if err == nil || !strings.Contains(err.Error(), "需要邮箱验证码") {
		t.Fatalf("unexpected login result: %v", err)
	}
	if authorizations != 1 || passwordVerifications != 1 {
		t.Fatalf("authorizations=%d password_verifications=%d, want one each", authorizations, passwordVerifications)
	}
}

func TestPasswordLoginStopsBeforePasswordWhenAuthorizeLandsOnChoice(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	session := &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		status, response := http.StatusOK, "<html></html>"
		headers := make(http.Header)
		switch req.URL.Path {
		case "/api/accounts/authorize":
			status = http.StatusFound
			headers.Set("Location", registerAuthBase+"/log-in-or-create-account")
		case "/log-in-or-create-account":
		default:
			t.Fatalf("unexpected request after choice page: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})}}
	_, err = session.loginWithPassword(context.Background(), "user@example.com", "password", "")
	if err == nil || !strings.Contains(err.Error(), "choice path=/log-in-or-create-account") {
		t.Fatalf("unexpected login result: %v", err)
	}
}

func TestVerifyPasswordLoginMFA(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	var calls []string
	session := &registerSession{client: &http.Client{Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		response := `{}`
		switch req.URL.Path {
		case "/api/accounts/mfa/issue_challenge":
			if body["id"] != "factor-id" || body["type"] != "totp" || body["force_fresh_challenge"] != false {
				t.Fatalf("unexpected challenge body: %#v", body)
			}
		case "/backend-api/sentinel/req":
			if body["flow"] != "verify_mfa" {
				t.Fatalf("unexpected sentinel flow: %v", body["flow"])
			}
			response = `{"token":"sentinel-token"}`
		case "/api/accounts/mfa/verify":
			if body["id"] != "factor-id" || body["type"] != "totp" || len(strAny(body["code"], "")) != 6 || req.Header.Get("OpenAI-Sentinel-Token") == "" {
				t.Fatal("invalid TOTP verification request")
			}
			response = `{"continue_url":"https://platform.openai.com/auth/callback?code=oauth-code","page":{"type":"external_url"}}`
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})}}
	result, err := session.verifyPasswordLoginMFA(context.Background(), "device-id", map[string]any{
		"page": map[string]any{"payload": map[string]any{
			"factor_id": "factor-id",
			"factors":   []any{map[string]any{"id": "factor-id", "factor_type": "totp"}},
		}},
	}, secret)
	if err != nil {
		t.Fatal(err)
	}
	if parseContinueCode(strAny(result["continue_url"], "")) != "oauth-code" {
		t.Fatal("missing OAuth code after MFA")
	}
	if strings.Join(calls, ",") != "/api/accounts/mfa/issue_challenge,/backend-api/sentinel/req,/api/accounts/mfa/verify" {
		t.Fatalf("unexpected call sequence: %v", calls)
	}
}
