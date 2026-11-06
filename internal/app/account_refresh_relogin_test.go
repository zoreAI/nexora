package app

import (
	"context"
	"errors"
	"io"
	stdhttp "net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

func TestRefreshReLoginUsesDeactivationEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code, wantStatus string
		transportErr           error
		cancel, newerToken     bool
	}{
		{name: "deactivated", code: "account_deactivated", wantStatus: "封号"},
		{name: "other-forbidden", code: "invalid_state", wantStatus: "异常"},
		{name: "network-error", transportErr: io.EOF, wantStatus: "异常"},
		{name: "canceled", cancel: true, wantStatus: "正常"},
		{name: "wrapped-cancellation", transportErr: context.Canceled, wantStatus: "正常"},
		{name: "newer-credentials", code: "account_deactivated", newerToken: true, wantStatus: "正常"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pendingRecoveryTestServer(t)
			email, password, secret := "refresh@example.com", "test-password", "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
			observed := Account{AccessToken: "old-token", Email: &email, Password: &password, MFASecret: &secret, PasswordStatus: "confirmed", MFAStatus: "enabled", Status: "正常", Proxy: "direct", Quota: 5, Fail: 2}
			if err := s.store.SaveAccounts([]Account{observed}); err != nil {
				t.Fatal(err)
			}
			client := &UpstreamClient{token: observed.AccessToken, client: upstreamDoerFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/backend-api/me" {
					return upstreamTestResponse(401, `{"error":{"code":"token_revoked"}}`), nil
				}
				return upstreamTestResponse(200, `{}`), nil
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, authErr := client.GetUserInfo(ctx)
			if !isInvalidTokenErrorText(authErr) {
				t.Fatalf("missing initial token failure: %v", authErr)
			}
			mfaCalled := false
			s.accountLoginSessionFactory = func(string, int) (*registerSession, error) {
				jar, _ := cookiejar.New(nil)
				return &registerSession{client: &stdhttp.Client{Jar: jar, Transport: mfaRoundTripper(func(r *stdhttp.Request) (*stdhttp.Response, error) {
					status, body := 200, `{}`
					headers := make(stdhttp.Header)
					switch r.URL.Path {
					case "/api/accounts/authorize":
						status = 302
						headers.Set("Location", registerAuthBase+"/log-in/password")
					case "/log-in/password":
						body = "<html></html>"
					case "/backend-api/sentinel/req":
						body = `{"token":"test-sentinel"}`
					case "/api/accounts/password/verify":
						body = `{"page":{"type":"mfa_challenge","payload":{"factor_id":"factor","factors":[{"id":"factor","factor_type":"totp"}]}}}`
					case "/api/accounts/mfa/issue_challenge":
					case "/api/accounts/mfa/verify":
						mfaCalled = true
						if tc.newerToken {
							if err := s.store.UpdateAccounts(func(accounts []Account) []Account {
								accounts[0].AccessToken = "new-token"
								return accounts
							}); err != nil {
								t.Fatal(err)
							}
						}
						if tc.cancel {
							cancel()
							return nil, ctx.Err()
						}
						if tc.transportErr != nil {
							return nil, tc.transportErr
						}
						status, body = 403, `{"error":{"code":"`+tc.code+`"}}`
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						status = 500
					}
					return &stdhttp.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}}, nil
			}
			_, err := s.reLoginAfterRefreshAuthFailure(ctx, observed, authErr)
			if err == nil || !mfaCalled {
				t.Fatalf("reauthorization did not reach MFA failure: %v", err)
			}
			if (tc.cancel || errors.Is(tc.transportErr, context.Canceled)) && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			got := s.store.LoadAccounts()[0]
			if got.Status != tc.wantStatus {
				t.Fatalf("status=%s, want %s", got.Status, tc.wantStatus)
			}
			wantQuota, wantFailures := 0, 3
			if tc.wantStatus == "正常" {
				wantQuota, wantFailures = 5, 2
			}
			if got.Quota != wantQuota || got.Fail != wantFailures || ptrString(got.Password) != password || ptrString(got.MFASecret) != secret {
				t.Fatal("incorrect failure counters, quota or stored credentials")
			}
			if tc.newerToken && got.AccessToken != "new-token" {
				t.Fatal("stale failure overwrote newer token")
			}
		})
	}
}
