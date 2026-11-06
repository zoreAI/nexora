package app

import (
	"errors"
	"net/url"
	"strings"
)

type registerRequestDiagnosticError struct {
	cause   error
	message string
}

func (e *registerRequestDiagnosticError) Error() string { return e.message }
func (e *registerRequestDiagnosticError) Unwrap() error { return e.cause }

// Authorization query strings can exceed the entire persisted error limit.
// Remove their values before truncation so the actual failure remains visible.
func registerAuthorizationRequestError(err error, target string) error {
	message := strings.ReplaceAll(err.Error(), target, safeURLForLog(target))
	var requestErr *url.Error
	if errors.As(err, &requestErr) && requestErr.URL != "" {
		message = strings.ReplaceAll(message, requestErr.URL, safeURLForLog(requestErr.URL))
	}
	return &registerRequestDiagnosticError{cause: err, message: "授权入口请求失败: " + message}
}
