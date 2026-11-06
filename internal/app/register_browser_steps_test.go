package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

func TestBrowserCodeResponseDistinguishesRejectionAndNextStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int64
		body   string
		want   string
	}{
		{"profile", 200, `{"page":{"type":"about_you"},"continue_url":"/about-you"}`, ""},
		{"password", 200, `{"page":{"type":"reset_password_new_password"},"continue_url":"/reset-password/new-password"}`, ""},
		{"callback", 200, `{"page":{"type":"external_url"},"continue_url":"https://chatgpt.com/api/auth/callback/openai?code=private-code&state=private-state"}`, ""},
		{"rejected", 400, `{"error":{"code":"invalid_otp","message":"private-code"}}`, "invalid_otp"},
		{"blocked", 403, `{"error":{"code":"account_deactivated"}}`, "account_deactivated"},
		{"same-step", 200, `{"page":{"type":"email_otp_verification"},"continue_url":"/email-verification"}`, "仍停留"},
		{"unknown", 200, `{}`, "缺少下一步"},
		{"html", 200, `<html>private-code</html>`, "未能读取"},
		{"different-mailbox", 200, `{"page":{"type":"about_you"},"oai-client-auth-session":{"email":"other@icloud.com"}}`, "邮箱"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseRegisterBrowserOTPResponse(tc.status, []byte(tc.body), "fixture@icloud.com")
			err := r.validateCode()
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("expected %s, got %v", tc.want, err)
			}
			text := r.summary()
			if err != nil {
				text += err.Error()
			}
			for _, private := range []string{"private-code", "private-state", "other@icloud.com"} {
				if strings.Contains(text, private) {
					t.Fatal("response leaked credentials")
				}
			}
		})
	}
}

func TestBrowserTraceDoesNotReuseEarlierResponses(t *testing.T) {
	trace := &registerBrowserTrace{requests: make(map[network.RequestID]registerBrowserRequest)}
	accepted := parseRegisterBrowserOTPResponse(200, []byte(`{"page":{"type":"about_you"}}`), "fixture@icloud.com")
	trace.begin("old", "/api/accounts/email-otp/validate")
	trace.finish("old", accepted)
	cursor := trace.cursor()
	if _, ok := trace.latest(cursor, "/api/accounts/email-otp/validate"); ok {
		t.Fatal("reused old OTP")
	}
	trace.begin("new", "/api/accounts/email-otp/validate")
	if req, ok := trace.latest(cursor, "/api/accounts/email-otp/validate"); !ok || req.complete {
		t.Fatal("missing in-flight request")
	}
	trace.finish("new", parseRegisterBrowserOTPResponse(400, []byte(`{"error":{"code":"invalid_otp"}}`), "fixture@icloud.com"))
	_, err := trace.wait(context.Background(), cursor, "/api/accounts/email-otp/validate")
	if err == nil || !strings.Contains(err.Error(), "invalid_otp") {
		t.Fatalf("failed to keep latest rejection: %v", err)
	}
	if trace.passwordAccepted("/api/accounts/password/verify") {
		t.Fatal("OTP response confirmed a password")
	}
}

func TestBrowserStepRetainsStageAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs []string
	ctx = context.WithValue(ctx, accountRepairProgressKey{}, func(line string) { logs = append(logs, line) })
	err := runRegisterBrowserStep(ctx, "signup", browserStepValidateCode, func() error { return ctx.Err() })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "validate_email_code") {
		t.Fatalf("lost cause/stage: %v", err)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "开始") || !strings.Contains(logs[1], "失败") {
		t.Fatalf("missing stage logs: %v", logs)
	}
}

func TestBrowserTraceRedactsUntrustedMetadata(t *testing.T) {
	r := parseRegisterBrowserOTPResponse(400, []byte(`{"page":{"type":"private@icloud.com"},"error":{"code":"Bearer private-secret"},"continue_url":"https://chatgpt.com/private-secret?code=private-code"}`), "")
	if strings.Contains(r.summary(), "private") {
		t.Fatal("untrusted metadata leaked")
	}
	for _, raw := range []string{"https://other.example/api/accounts/create_account", "https://auth.openai.com/api/accounts/private-secret"} {
		if registerBrowserAccountPath(raw) != "" {
			t.Fatal("recorded unexpected origin/path")
		}
	}
}

func TestBrowserEmailVerificationRetriesOnlyRejectedCode(t *testing.T) {
	for _, kind := range []string{"success", "wrong-then-success", "wrong-twice", "blocked", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first, second := time.Now().Add(-time.Minute), time.Now()
			waits, validates, resends := 0, 0, 0
			err := verifyRegisterBrowserEmail(ctx, "fixture", first, func(_ context.Context, after time.Time) (string, error) {
				waits++
				want := first
				if waits > 1 {
					want = second
				}
				if !after.Equal(want) {
					t.Fatal("wrong mail boundary")
				}
				return "fixture-code", nil
			}, func(string) error {
				validates++
				if kind == "success" || kind == "wrong-then-success" && validates == 2 {
					return nil
				}
				if kind == "cancelled" {
					cancel()
					return context.Canceled
				}
				code := "wrong_email_otp_code"
				if kind == "blocked" {
					code = "account_deactivated"
				}
				return &registerBrowserResponseError{response: registerBrowserOTPResponse{status: 401, errorCode: code}}
			}, func(context.Context) (time.Time, error) { resends++; return second, nil })
			wantSuccess := kind == "success" || kind == "wrong-then-success"
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected result: %v", err)
			}
			wantRetries := 0
			if strings.HasPrefix(kind, "wrong-") {
				wantRetries = 1
			}
			if resends != wantRetries || waits != 1+wantRetries || validates != waits {
				t.Fatalf("waits=%d validates=%d resends=%d", waits, validates, resends)
			}
		})
	}
}
