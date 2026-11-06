package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMailboxNoCodeBrowserFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no-code", errors.New("获取邮箱验证码失败: 最近结果：no_code: context deadline exceeded"), true},
		{"prior-no-code-before-timeout", errors.New("获取邮箱验证码失败: 取码请求59次，无新验证码58次，最近结果：context deadline exceeded"), true},
		{"provider-unreachable", errors.New("获取邮箱验证码失败: 取码请求1次，无新验证码0次，最近结果：connection refused"), false},
		{"unrelated-timeout", context.DeadlineExceeded, false},
		{"cancelled", fmt.Errorf("获取邮箱验证码失败: no_code: %w", context.Canceled), false},
		{"rate-limited", errors.New("获取邮箱验证码失败: no_code HTTP 429 rate_limit_exceeded"), false},
		{"deactivated", errors.New("获取邮箱验证码失败: no_code code=account_deactivated"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFallbackPendingRegistrationToBrowser(tc.err); got != tc.want {
				t.Fatalf("fallback=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestPendingPasswordConfirmationSurvivesRestart(t *testing.T) {
	s := pendingRecoveryTestServer(t)
	if err := s.register.savePendingPassword("partial@example.com", "candidate", "待接续"); err != nil {
		t.Fatal(err)
	}
	if err := s.register.confirmPendingPassword("partial@example.com", "different"); err == nil {
		t.Fatal("confirmed a changed candidate")
	}
	if s.register.snapshot().PendingPasswords[0].PasswordStatus == "confirmed" {
		t.Fatal("changed candidate marked confirmed")
	}
	if err := s.register.confirmPendingPassword("PARTIAL@example.com", "candidate"); err != nil {
		t.Fatal(err)
	}
	reloaded := newRegisterService(t.TempDir(), s.store)
	if reloaded.snapshot().PendingPasswords[0].PasswordStatus != "confirmed" {
		t.Fatal("accepted password lost after restart")
	}
}
