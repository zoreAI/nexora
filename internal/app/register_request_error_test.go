package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPendingAuthorizationFailureRetainsCauseBeforeTruncation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		attempts int
	}{
		{"eof", io.EOF, 2},
		{"reset", syscall.ECONNRESET, 1},
		{"canceled", context.Canceled, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email := "partial@icloud.com"
			if err := s.register.savePendingPassword(email, "saved-password", "注册接口已接受"); err != nil {
				t.Fatal(err)
			}
			s.register.cfg.Mail = RegisterMailConfig{Providers: []map[string]any{{"type": "icloud_api", "enable": true, "api_base": "http://127.0.0.1:1", "api_key": "test-key"}}}
			attempts := 0
			s.register.recoverySessionFactory = func(string) (*registerSession, error) {
				jar, _ := cookiejar.New(nil)
				return &registerSession{client: &http.Client{Jar: jar, Transport: mfaRoundTripper(func(req *http.Request) (*http.Response, error) {
					attempts++
					if req.URL.Path != "/api/accounts/authorize" {
						t.Fatal("authorization failure unexpectedly advanced to another step")
					}
					return nil, tc.cause
				})}}, nil
			}
			s.register.signupBrowserFactory = func(context.Context, *registerSession, string, string, registerProfileConfig, func(context.Context, time.Time) (string, error)) (map[string]any, error) {
				t.Fatal("network failure must not be treated as a rejected signup")
				return nil, nil
			}
			err := s.repairPendingRegistration(context.Background(), email)
			if err == nil || attempts != tc.attempts {
				t.Fatalf("incorrect retry count: attempts=%d err=%v", attempts, err)
			}
			if tc.name == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation cause was lost")
				}
				return
			}
			pending := s.pendingRegisterAccounts()
			if len(pending) != 1 || len(s.store.LoadAccounts()) != 0 || !pending[0].PasswordSaved {
				t.Fatal("failed authorization lost the original registration")
			}
			message := pending[0].State
			if !strings.Contains(message, "授权入口请求失败") || !strings.Contains(message, tc.cause.Error()) || strings.Contains(message, "code_challenge=") || strings.Contains(message, "login_hint=") || strings.Contains(message, "saved-password") {
				t.Fatalf("persisted diagnostic lost failure cause or exposed authorization parameters: %s", message)
			}
		})
	}
}
