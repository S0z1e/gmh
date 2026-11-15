package llm

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// StreamError preserves a provider failure sent after HTTP headers succeeded.
type StreamError struct {
	API, Type, Code, Message string
	StatusCode               int
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("LLM %s stream error (%s): %s", e.API, strings.Trim(strings.Join([]string{e.Type, e.Code}, "/"), "/"), e.Message)
}

func (e *StreamError) retryable() bool {
	switch e.Code {
	case "context_length_exceeded", "invalid_request_error", "invalid_api_key", "authentication_error", "permission_error", "content_policy_violation":
		return false
	}
	switch e.Type {
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error":
		return false
	}
	for _, code := range []string{e.Type, e.Code} {
		switch code {
		case "overloaded_error", "overloaded", "rate_limit_error", "rate_limit_exceeded", "server_error", "api_error", "internal_error", "timeout_error", "request_timeout":
			return true
		}
	}
	switch e.StatusCode {
	case 408, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

func streamProviderError(api, data string) error {
	var event struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(data), &event) != nil {
		return nil
	}
	raw := event.Error
	if event.Type == "error" && (len(raw) == 0 || string(raw) == "null") {
		raw = json.RawMessage(data)
	}
	if event.Type == "response.failed" {
		raw = event.Response.Error
	}
	if (len(raw) == 0 || string(raw) == "null") && event.Type != "error" && event.Type != "response.failed" {
		return nil
	}
	var detail struct {
		Type       string          `json:"type"`
		Code       json.RawMessage `json:"code"`
		Message    string          `json:"message"`
		Status     int             `json:"status"`
		StatusCode int             `json:"status_code"`
	}
	_ = json.Unmarshal(raw, &detail)
	code := ""
	if len(detail.Code) > 0 && string(detail.Code) != "null" {
		if json.Unmarshal(detail.Code, &code) != nil {
			code = string(detail.Code)
		}
	}
	if detail.Message == "" {
		detail.Message = "provider reported a failed stream"
	}
	if detail.StatusCode == 0 {
		detail.StatusCode = detail.Status
	}
	if detail.StatusCode == 0 {
		detail.StatusCode, _ = strconv.Atoi(code)
	}
	return &StreamError{API: api, Type: detail.Type, Code: code, Message: detail.Message, StatusCode: detail.StatusCode}
}
