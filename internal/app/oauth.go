package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

const codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
const webOAuthClientID = "app_2SKx67EdpoN0G6j64rFvigXD"
const codexOAuthTokenURL = "https://auth.openai.com/oauth/token"

type oauthRefreshError struct {
	StatusCode int
	Code       string
	Terminal   bool
}

func (e *oauthRefreshError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("oauth refresh failed: status=%d code=%s", e.StatusCode, e.Code)
	}
	return fmt.Sprintf("oauth refresh failed: status=%d", e.StatusCode)
}

func terminalOAuthRefreshError(err error) bool {
	var refreshErr *oauthRefreshError
	return errors.As(err, &refreshErr) && refreshErr.Terminal
}

func (s *Server) refreshOAuthAccount(ctx context.Context, oldToken string) (Account, error) {
	return s.refreshOAuthAccountAt(ctx, oldToken, codexOAuthTokenURL)
}

func accessTokenNeedsRefresh(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	return json.Unmarshal(payload, &claims) == nil && claims.ExpiresAt > 0 && claims.ExpiresAt <= time.Now().Add(24*time.Hour).Unix()
}

func (s *Server) refreshOAuthAccountAt(ctx context.Context, oldToken, tokenURL string) (Account, error) {
	accounts := s.store.LoadAccounts()
	idx := -1
	var account Account
	for i, a := range accounts {
		if a.AccessToken == oldToken {
			idx = i
			account = a
			break
		}
	}
	if idx < 0 || account.RefreshToken == nil || strings.TrimSpace(*account.RefreshToken) == "" {
		return Account{}, fmt.Errorf("refresh_token not found")
	}
	client, err := s.newAccountLoginClient(ctx, account)
	if err != nil {
		return Account{}, err
	}
	clientID := webOAuthClientID
	if strings.EqualFold(account.SourceType, "codex") || strings.EqualFold(account.ExportType, "codex") {
		clientID = codexOAuthClientID
	}
	if account.ClientID != nil && strings.TrimSpace(*account.ClientID) != "" {
		clientID = strings.TrimSpace(*account.ClientID)
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", strings.TrimSpace(*account.RefreshToken))
	if clientID == codexOAuthClientID {
		form.Set("scope", "openid profile email")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Account{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.client.Do(req)
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var failure map[string]any
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure)
		code := strings.TrimSpace(strAny(failure["error"], strAny(failure["code"], "")))
		if nested, ok := failure["error"].(map[string]any); ok {
			code = strings.TrimSpace(strAny(nested["code"], strAny(failure["code"], "")))
		}
		code = strings.ToLower(code)
		description := strings.ToLower(strings.TrimSpace(strAny(failure["error_description"], strAny(failure["message"], ""))))
		terminal := resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 &&
			(code == "invalid_grant" || code == "invalid_refresh_token" || code == "refresh_token_invalidated" || strings.Contains(description, "session has ended"))
		return Account{}, &oauthRefreshError{StatusCode: resp.StatusCode, Code: code, Terminal: terminal}
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Account{}, err
	}
	newToken := strings.TrimSpace(strAny(payload["access_token"], ""))
	if newToken == "" {
		return Account{}, fmt.Errorf("oauth refresh response missing access_token")
	}
	refreshToken := strings.TrimSpace(strAny(payload["refresh_token"], ""))
	if refreshToken == "" && account.RefreshToken != nil {
		refreshToken = *account.RefreshToken
	}
	idToken := strings.TrimSpace(strAny(payload["id_token"], ""))
	expiresIn := intAny(payload["expires_in"], 0)
	updated := false
	credentialsChanged := false
	if err := s.store.UpdateAccounts(func(current []Account) []Account {
		oldIndex := -1
		newIndex := -1
		for i := range current {
			if current[i].AccessToken == oldToken {
				oldIndex = i
			} else if current[i].AccessToken == newToken {
				newIndex = i
			}
		}
		if oldIndex < 0 {
			return current
		}
		if ptrString(current[oldIndex].RefreshToken) != ptrString(account.RefreshToken) {
			credentialsChanged = true
			return current
		}
		account = current[oldIndex]
		account.AccessToken = newToken
		account.RefreshToken = &refreshToken
		if idToken != "" {
			account.IDToken = &idToken
		}
		account.IsSignupToken = isSignupAccessToken(newToken)
		account.ClientID = &clientID
		if expiresIn > 0 {
			account.ExpiresAt = time.Now().Unix() + int64(expiresIn)
		}
		if account.AccountID == nil || *account.AccountID == "" {
			if accountID := chatGPTAccountID(newToken); accountID != "" {
				account.AccountID = &accountID
			}
		}
		if newIndex >= 0 {
			existing := current[newIndex]
			merged := account
			mergeAccount(&merged, existing)
			merged.AccessToken = newToken
			merged.IsSignupToken = account.IsSignupToken
			merged.RefreshToken = &refreshToken
			merged.ClientID = &clientID
			if idToken != "" {
				merged.IDToken = &idToken
			}
			merged.ExpiresAt = account.ExpiresAt
			merged.Quota = existing.Quota
			merged.ImageQuotaUnknown = existing.ImageQuotaUnknown
			merged.Success = existing.Success
			merged.Fail = existing.Fail
			account = merged
			current[newIndex] = account
			current = append(current[:oldIndex], current[oldIndex+1:]...)
		} else {
			current[oldIndex] = account
		}
		updated = true
		return current
	}); err != nil {
		return Account{}, err
	}
	if credentialsChanged {
		return Account{}, errors.New("account refresh credentials changed during OAuth exchange")
	}
	if !updated {
		return Account{}, fmt.Errorf("account was removed during token refresh")
	}
	return account, nil
}

func (s *Server) refreshOAuthAccessToken(ctx context.Context, oldToken string) (string, error) {
	account, err := s.refreshOAuthAccount(ctx, oldToken)
	if err != nil {
		return "", err
	}
	return account.AccessToken, nil
}

func (s *Server) upstreamClientForTokenWithRefresh(ctx context.Context, token string) (*UpstreamClient, error) {
	client, err := s.newUpstreamClientForAccount(s.accountByToken(token))
	if err == nil {
		return client, nil
	}
	newToken, refreshErr := s.refreshOAuthAccessToken(ctx, token)
	if refreshErr != nil {
		return nil, err
	}
	return s.newUpstreamClientForAccount(s.accountByToken(newToken))
}
