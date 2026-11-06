package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBrowserOTPSendRequiresBoundMailboxAndVerificationStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int64
		body   string
		ok     bool
	}{
		{"accepted", 200, `{"page":{"type":"email_otp_verification"},"oai-client-auth-session":{"email":"ALIAS@icloud.com","passwordless_email_otp_send_pending":false,"session_id":"private-session"}}`, true},
		{"har-username-object", 200, `{"page":{"type":"email_otp_verification"},"oai-client-auth-session":{"email":"alias@icloud.com","username":{"kind":"email","value":"alias@icloud.com"},"passwordless_email_otp_send_pending":false}}`, true},
		{"username-object-only", 200, `{"page":{"type":"email_otp_verification"},"oai-client-auth-session":{"username":{"kind":"email","value":"alias@icloud.com"}}}`, true},
		{"wrong-mailbox", 200, `{"page":{"type":"email_otp_verification"},"oai-client-auth-session":{"email":"other@icloud.com"}}`, false},
		{"missing-mailbox", 200, `{"page":{"type":"email_otp_verification"}}`, false},
		{"wrong-step", 200, `{"page":{"type":"login_password"},"oai-client-auth-session":{"email":"alias@icloud.com"}}`, false},
		{"blocked", 403, `{"error":{"code":"account_deactivated","message":"private-session"}}`, false},
		{"html-200", 200, `<html>private-session</html>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := parseRegisterBrowserOTPResponse(tc.status, []byte(tc.body), "alias@icloud.com")
			err := response.validate()
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected acceptance: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "private-session") || strings.Contains(err.Error(), "other@icloud.com")) {
				t.Fatal("private response fields leaked")
			}
		})
	}
}

func TestBrowserOTPResendsOnceAndUsesNewMailBoundary(t *testing.T) {
	for _, tc := range []string{"immediate", "resend-success", "resend-no-code", "resend-error", "resend-invalid-response", "provider-error", "cancelled"} {
		t.Run(tc, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			oldCutoff := time.Now().Add(-time.Minute)
			newCutoff := time.Now().UTC().Truncate(time.Second)
			waits, resends := 0, 0
			code, err := waitRegisterBrowserCode(ctx, oldCutoff, 5*time.Millisecond, func(roundCtx context.Context, cutoff time.Time) (string, error) {
				waits++
				if waits == 1 {
					if !cutoff.Equal(oldCutoff) {
						t.Fatal("initial boundary changed")
					}
					if tc == "immediate" {
						return "123456", nil
					}
					if tc == "provider-error" {
						return "", errors.New("mailbox unavailable")
					}
					if tc == "cancelled" {
						cancel()
					}
					<-roundCtx.Done()
					return "", roundCtx.Err()
				}
				if waits > 2 || !cutoff.Equal(newCutoff) {
					t.Fatal("resend repeated or reused old mail boundary")
				}
				if tc == "resend-no-code" {
					return "", context.DeadlineExceeded
				}
				return "654321", nil
			}, func(context.Context) (time.Time, error) {
				resends++
				if tc == "resend-error" {
					return time.Time{}, errors.New("resend button disabled")
				}
				if tc == "resend-invalid-response" {
					return newCutoff, errors.New("resend response did not include page metadata")
				}
				return newCutoff, nil
			})
			shouldResend := strings.HasPrefix(tc, "resend-")
			if (resends == 1) != shouldResend || resends > 1 {
				t.Fatalf("unexpected resend count: %d", resends)
			}
			success := tc == "immediate" || tc == "resend-success" || tc == "resend-invalid-response"
			if success != (err == nil && code != "") {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
