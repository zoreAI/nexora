package app

import "time"

func accountQuotaWindowReset(account Account) string {
	for _, limit := range account.LimitsProgress {
		if strAny(limit["feature_name"], "") == "image_gen" {
			return firstNonEmpty(strAny(limit["reset_after"], ""), strAny(limit["resets_at"], ""), ptrString(account.RestoreAt))
		}
	}
	return ptrString(account.RestoreAt)
}

// Lifetime counters cannot be attributed to the current quota window. Start
// separate counters when first observed, and retain totals for compatibility.
func advanceAccountWindowStats(account *Account, now time.Time) {
	if account.WindowStartedAt == "" {
		account.WindowStartedAt = now.UTC().Format(time.RFC3339Nano)
		account.WindowResetAt = accountQuotaWindowReset(*account)
		account.WindowSuccess, account.WindowFail = 0, 0
	}
	if reset, err := parseAccountTime(account.WindowResetAt); err == nil && !now.Before(reset) {
		account.WindowSuccess, account.WindowFail = 0, 0
		account.WindowStartedAt = reset.UTC().Format(time.RFC3339Nano)
		// A subsequent upstream refresh supplies the next actual boundary.
		account.WindowResetAt = ""
	}
}

func refreshAccountWindowStats(account *Account, info Account, now time.Time) {
	advanceAccountWindowStats(account, now)
	if info.ImageQuotaUnknown {
		return
	}
	reset, err := parseAccountTime(accountQuotaWindowReset(info))
	if err != nil || !reset.After(now) {
		return
	}
	previous, previousErr := parseAccountTime(account.WindowResetAt)
	if previousErr == nil && !previous.Equal(reset) {
		account.WindowSuccess, account.WindowFail = 0, 0
		account.WindowStartedAt = now.UTC().Format(time.RFC3339Nano)
	}
	account.WindowResetAt = reset.UTC().Format(time.RFC3339Nano)
}
