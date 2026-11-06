package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type iCloudAPIRegisterProvider struct {
	registerProviderBase
	entry    map[string]any
	mailSess *registerSession
	service  *registerService
}

type iCloudAPIResponse struct {
	Success   bool   `json:"success"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Email     string `json:"email"`
	Subject   string `json:"subject"`
	MessageID string `json:"message_id"`
	Mailbox   struct {
		Email        string `json:"email"`
		APIActive    bool   `json:"api_active"`
		ICloudActive bool   `json:"icloud_active"`
	} `json:"mailbox"`
}

func (p *iCloudAPIRegisterProvider) Name() string { return "icloud_api" }

func (p *iCloudAPIRegisterProvider) Close() {
	if p.mailSess != nil {
		p.mailSess.close()
	}
	p.sess.close()
}

func (p *iCloudAPIRegisterProvider) endpoint(path string) (string, error) {
	raw := strings.TrimSpace(strAny(p.entry["api_base"], ""))
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || (base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("iCloud Privacy Mail API Base 必须是服务根地址，例如 http://127.0.0.1:8787")
	}
	return base.Scheme + "://" + base.Host + path, nil
}

func (p *iCloudAPIRegisterProvider) request(ctx context.Context, method, path string, payload any, out *iCloudAPIResponse) error {
	endpoint, err := p.endpoint(path)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(strAny(p.entry["api_key"], ""))
	if key == "" {
		return errors.New("iCloud Privacy Mail 缺少全局 API Key")
	}
	session := p.sess
	if p.mailSess != nil {
		session = p.mailSess
	}
	return session.requestJSON(ctx, method, endpoint, map[string]string{"Authorization": "Bearer " + key}, payload, out, http.StatusOK)
}

func (p *iCloudAPIRegisterProvider) CreateMailbox(username string) (*registerMailbox, error) {
	if strings.TrimSpace(username) != "" {
		return nil, errors.New("iCloud Privacy Mail 只支持从可用邮箱池自动领取")
	}
	project := strings.TrimSpace(strAny(p.entry["project"], "openai"))
	if project == "" {
		project = "openai"
	}
	var response iCloudAPIResponse
	if err := p.request(context.Background(), http.MethodPost, "/api/v1/mailboxes/claim", map[string]any{
		"project": project,
		"purpose": "register",
		"count":   1,
	}, &response); err != nil {
		return nil, err
	}
	if !response.Success {
		if response.Code == "no_available_mailbox" {
			return nil, registerCapacityExhaustedError{message: "iCloud Privacy Mail 没有可用邮箱"}
		}
		return nil, fmt.Errorf("iCloud Privacy Mail 领取邮箱失败: %s", firstNonEmpty(response.Message, response.Code, "未知错误"))
	}
	address := strings.TrimSpace(response.Mailbox.Email)
	if address == "" || !response.Mailbox.APIActive || !response.Mailbox.ICloudActive {
		return nil, errors.New("iCloud Privacy Mail 返回的邮箱不可用")
	}
	return &registerMailbox{
		Provider:    p.Name(),
		ProviderRef: strAny(p.entry["provider_ref"], ""),
		Label:       strAny(p.entry["label"], ""),
		Address:     address,
		Extra:       map[string]any{"mailbox_provider": p.Name()},
	}, nil
}

func (p *iCloudAPIRegisterProvider) MarkFailed(mailbox *registerMailbox, reason string) {
	if mailbox == nil || strings.TrimSpace(mailbox.Address) == "" {
		return
	}
	email := strings.ToLower(strings.TrimSpace(mailbox.Address))
	if p.service != nil {
		for _, account := range p.service.loadCurrentAccounts() {
			if strings.EqualFold(ptrString(account.Email), email) {
				return
			}
		}
		if p.service.hasRecoveryEmail(email) || p.service.pendingPasswordForEmail(email) != "" {
			p.service.appendLog("注册未完成，邮箱保留供手动接续["+email+"]", "yellow")
			return
		}
	}
	var response iCloudAPIResponse
	path := "/api/v1/mailboxes/" + url.PathEscape(email) + "/release"
	if err := p.request(context.Background(), http.MethodPost, path, nil, &response); err != nil {
		if p.service != nil {
			p.service.appendLog("邮箱状态恢复失败["+email+"]: "+err.Error(), "red")
		}
		return
	}
	if !response.Success && p.service != nil {
		p.service.appendLog("邮箱状态恢复失败["+email+"]: "+firstNonEmpty(response.Message, response.Code), "red")
	} else if response.Success && p.service != nil {
		p.service.appendLog("注册未完成，邮箱已恢复 available["+email+"]", "yellow")
	}
}

func (p *iCloudAPIRegisterProvider) WaitForCode(ctx context.Context, mailbox *registerMailbox) (string, error) {
	if mailbox == nil || strings.TrimSpace(mailbox.Address) == "" {
		return "", errors.New("iCloud Privacy Mail 缺少邮箱地址")
	}
	timeoutSeconds := intAny(p.entry["wait_timeout"], 30)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	// Bound each server poll; the overall OTP wait is managed here rather than
	// inheriting the mail service's configurable long-poll duration.
	query := url.Values{"keyword": {"OpenAI"}, "wait_ms": {"1000"}}
	if !mailbox.CodeNotBefore.IsZero() {
		query.Set("after", mailbox.CodeNotBefore.UTC().Format(time.RFC3339Nano))
	}
	path := "/api/v1/mailboxes/" + url.PathEscape(mailbox.Address) + "/code?" + query.Encode()
	requests, emptyResponses := 0, 0
	lastResult := "尚未收到接口响应"
	lastReport := time.Time{}
	code, err := waitForRegisterCode(waitCtx, mailbox, timeoutSeconds, intAny(p.entry["wait_interval"], 2), func() ([]map[string]any, error) {
		requests++
		defer func() {
			if lastReport.IsZero() || time.Since(lastReport) >= 15*time.Second {
				reportAccountRepairProgress(ctx, fmt.Sprintf("等待邮箱验证码（取码请求%d次，无新验证码%d次，最近结果：%s）", requests, emptyResponses, lastResult))
				lastReport = time.Now()
			}
		}()
		var response iCloudAPIResponse
		if err := p.request(waitCtx, http.MethodGet, path, nil, &response); err != nil {
			lastResult = err.Error()
			return nil, err
		}
		if !response.Success {
			lastResult = response.Code
			if response.Code == "no_code" && response.Retryable {
				emptyResponses++
				return nil, nil
			}
			return nil, fmt.Errorf("iCloud Privacy Mail 取码失败: %s", firstNonEmpty(response.Message, response.Code, "未知错误"))
		}
		if response.Email != "" && !strings.EqualFold(response.Email, mailbox.Address) {
			lastResult = "返回邮箱不匹配"
			return nil, errors.New("iCloud Privacy Mail 返回了其他邮箱的验证码")
		}
		if strings.TrimSpace(response.Code) == "" {
			lastResult = "返回空验证码"
			return nil, errors.New("iCloud Privacy Mail 返回空验证码")
		}
		lastResult = "已返回验证码"
		// The API already applies `after`; its returned timestamp loses subsecond precision.
		return []map[string]any{{
			"provider":     p.Name(),
			"mailbox":      mailbox.Address,
			"message_id":   response.MessageID,
			"subject":      response.Subject,
			"text_content": response.Code,
		}}, nil
	})
	if err != nil {
		cause := error(context.DeadlineExceeded)
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		return "", fmt.Errorf("iCloud Privacy Mail 等待验证码未完成（取码请求%d次，无新验证码%d次，最近结果：%s）: %w", requests, emptyResponses, lastResult, cause)
	}
	return code, nil
}
