package app

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func isAbnormalAccountStatus(status string) bool {
	return status == "异常" || status == "封号"
}

func isRateLimitErrorText(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "status=429") ||
		strings.Contains(text, "http 429") ||
		strings.Contains(text, "too many requests") ||
		strings.Contains(text, "rate_limit_exceeded") ||
		strings.Contains(text, "usage_limit_reached") ||
		strings.Contains(text, "free plan limit") ||
		strings.Contains(text, "limit for image generation") ||
		strings.Contains(text, "image generations requests") ||
		strings.Contains(text, "when the limit resets") ||
		strings.Contains(text, "图像生成请求上限") ||
		strings.Contains(text, "图片生成请求上限") ||
		strings.Contains(text, "图像生成次数上限")
}

func isInvalidTokenErrorText(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "status=401") ||
		strings.Contains(text, "http 401") ||
		strings.Contains(text, "token_invalidated") ||
		strings.Contains(text, "token_revoked") ||
		strings.Contains(text, "invalidated oauth token") ||
		strings.Contains(text, "authentication token has been invalidated")
}

func isAccountDeactivatedError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "code=account_deactivated") ||
		strings.Contains(text, `"code":"account_deactivated"`) ||
		strings.Contains(text, `"error":"account_deactivated"`) ||
		strings.Contains(text, `"error": "account_deactivated"`)
}

func isAutoConversationModelNotFound(err error, model string) bool {
	if err == nil || normalizeTextModelOptions(model).Model != "auto" {
		return false
	}
	text := err.Error()
	if !strings.Contains(strings.ToLower(text), "post /backend-api/conversation failed: status=404 body=") {
		return false
	}
	_, body, ok := strings.Cut(text, " body=")
	if !ok {
		return false
	}
	var payload struct {
		Detail string `json:"detail"`
	}
	return json.Unmarshal([]byte(body), &payload) == nil && strings.EqualFold(strings.TrimSpace(payload.Detail), "Model not found")
}

func isUpstreamBlockErrorText(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return (strings.Contains(text, "status=403") || strings.Contains(text, "http 403")) &&
		(strings.Contains(text, "<html") || strings.Contains(text, "<body") || strings.Contains(text, "meta http-equiv") || strings.Contains(text, "something seems to have gone wrong"))
}

func isTurnstileRequirementErrorText(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "turnstile") && strings.Contains(text, "required")
}

func isTemporaryUpstreamErrorText(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "timed out") || strings.Contains(text, "timeout") || strings.Contains(text, "deadline exceeded") {
		return true
	}
	for _, marker := range []string{"status=500", "status=502", "status=503", "status=504", "http 500", "http 502", "http 503", "http 504"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func rateLimitRestoreDelay(err error) time.Duration {
	if err == nil {
		return 5 * time.Minute
	}
	text := strings.ToLower(err.Error())
	hours := 0
	minutes := 0
	if m := regexp.MustCompile(`(\d+)\s*hours?`).FindStringSubmatch(text); len(m) > 1 {
		hours, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`(\d+)\s*minutes?`).FindStringSubmatch(text); len(m) > 1 {
		minutes, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`(\d+)\s*小时`).FindStringSubmatch(text); len(m) > 1 {
		hours, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`(\d+)\s*分钟`).FindStringSubmatch(text); len(m) > 1 {
		minutes, _ = strconv.Atoi(m[1])
	}
	if hours > 0 || minutes > 0 {
		return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
	}
	return 5 * time.Minute
}

func (s *Server) markAccountSuccess(token string, image bool) {
	if token == "" {
		return
	}
	now := nowISO()
	_ = s.store.UpdateAccounts(func(accounts []Account) []Account {
		for i := range accounts {
			if accounts[i].AccessToken != token {
				continue
			}
			advanceAccountWindowStats(&accounts[i], time.Now())
			accounts[i].Success++
			accounts[i].WindowSuccess++
			accounts[i].LastUsedAt = &now
			if image && !accounts[i].ImageQuotaUnknown && accounts[i].Quota > 0 {
				accounts[i].Quota--
			}
			if accounts[i].Status == "限流" && (accounts[i].ImageQuotaUnknown || accounts[i].Quota > 0) {
				accounts[i].Status = "正常"
				accounts[i].RestoreAt = nil
				accounts[i].RateLimitedAt = nil
				accounts[i].RateLimitResetAt = nil
			}
			return accounts
		}
		return accounts
	})
}

func (s *Server) markAccountFailure(token string, err error, image bool, model string) {
	if token == "" || isProxyConnectionFailure(err) || isImageDeliveryFailure(err) || isImageTextReplyFailure(err) || errors.Is(err, errNoImageGenerated) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	now := nowISO()
	_ = s.store.UpdateAccounts(func(accounts []Account) []Account {
		for i := range accounts {
			if accounts[i].AccessToken != token {
				continue
			}
			advanceAccountWindowStats(&accounts[i], time.Now())
			accounts[i].Fail++
			accounts[i].WindowFail++
			accounts[i].LastUsedAt = &now
			if accounts[i].Status == "封号" {
				return accounts
			}
			if isRateLimitErrorText(err) {
				accounts[i].Status = "限流"
				reset := time.Now().UTC().Add(rateLimitRestoreDelay(err)).Format(time.RFC3339)
				accounts[i].RestoreAt = &reset
				accounts[i].RateLimitResetAt = &reset
				accounts[i].RateLimitedAt = &now
				if s.cfg.AutoRemoveRateLimitedAccounts {
					accounts = append(accounts[:i], accounts[i+1:]...)
				}
			} else if isAccountDeactivatedError(err) {
				accounts[i].Status = "封号"
				accounts[i].Quota = 0
			} else if isInvalidTokenErrorText(err) || (!image && isAutoConversationModelNotFound(err, model)) {
				if accounts[i].Status != "封号" {
					accounts[i].Status = "异常"
				}
				if isInvalidTokenErrorText(err) {
					accounts[i].Quota = 0
				}
				if isInvalidTokenErrorText(err) && s.cfg.AutoRemoveInvalidAccounts {
					accounts = append(accounts[:i], accounts[i+1:]...)
				}
			}
			return accounts
		}
		return accounts
	})
}

func applyAccountFailureState(account *Account, err error, now string) {
	statsAt, parseErr := parseAccountTime(now)
	if parseErr != nil {
		statsAt = time.Now()
	}
	advanceAccountWindowStats(account, statsAt)
	account.Fail++
	account.WindowFail++
	account.LastUsedAt = &now
	if isAccountDeactivatedError(err) {
		account.Status = "封号"
		account.Quota = 0
	} else if account.Status == "封号" {
		return
	} else if isRateLimitErrorText(err) {
		account.Status = "限流"
		reset := time.Now().UTC().Add(rateLimitRestoreDelay(err)).Format(time.RFC3339)
		account.RestoreAt = &reset
		account.RateLimitResetAt = &reset
		account.RateLimitedAt = &now
	} else if isInvalidTokenErrorText(err) {
		account.Status = "异常"
		account.Quota = 0
	}
}
