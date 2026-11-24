package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/retryafter"
)

type upstreamFailure struct {
	Code         string                     `json:"code"`
	Type         string                     `json:"type"`
	Headers      map[string]json.RawMessage `json:"headers"`
	RetryAfter   json.RawMessage            `json:"retry_after"`
	RetryAfterMS json.RawMessage            `json:"retry_after_ms"`
}

func retryHint(raw json.RawMessage, milliseconds bool) int {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		var values []string
		if json.Unmarshal(raw, &values) == nil && len(values) > 0 {
			value = values[0]
		} else {
			value = strings.TrimSpace(string(raw))
		}
	}
	if !milliseconds {
		return retryafter.Seconds(value)
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}
	if ms > 604800000 {
		return 604800
	}
	return int((ms + 999) / 1000)
}

func upstreamRetryAfter(detail upstreamFailure) int {
	delay := retryHint(detail.RetryAfter, false)
	if n := retryHint(detail.RetryAfterMS, true); n > delay {
		delay = n
	}
	for key, value := range detail.Headers {
		var n int
		switch strings.ToLower(key) {
		case "retry-after":
			n = retryHint(value, false)
		case "retry-after-ms":
			n = retryHint(value, true)
		}
		if n > delay {
			delay = n
		}
	}
	return delay
}

func eventFailure(raw []byte) *rpcError {
	var event struct {
		upstreamFailure
		Error    upstreamFailure `json:"error"`
		Response struct {
			Error upstreamFailure `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(raw, &event)
	upstream := event.Response.Error
	if upstream.Code == "" && upstream.Type == "" {
		upstream = event.Error
	}
	if upstream.Code == "" && upstream.Type == "" {
		upstream = event.upstreamFailure
	}
	code := upstream.Code
	if code == "" {
		code = upstream.Type
	}
	var detail *rpcError
	switch code {
	case "usage_limit_reached", "insufficient_quota", "quota_exceeded":
		detail = &rpcError{Code: "credential_quota", Message: "BPS account quota exhausted", HTTPStatus: 429, CredentialScoped: true}
	case "rate_limit_exceeded", "rate_limit_error", "request_rejected":
		detail = &rpcError{Code: "transient_rate_limit", Message: "BPS upstream rate limited", HTTPStatus: 429, RetryAfterSeconds: 1}
	case "server_is_overloaded", "overloaded_error", "service_unavailable", "service_unavailable_error":
		detail = &rpcError{Code: "transient_overload", Message: "BPS upstream is overloaded; retry after the indicated delay", HTTPStatus: 503}
	case "invalid_api_key", "token_expired", "invalid_token", "authentication_error":
		detail = &rpcError{Code: "credential_invalid", Message: "BPS credential rejected", HTTPStatus: 401}
	case "model_not_found", "model_not_available":
		detail = &rpcError{Code: "model_not_found", Message: "Model is unavailable on this BPS credential", HTTPStatus: 404}
	case "invalid_request_error", "basispoints_protocol_error":
		detail = &rpcError{Code: "request_scoped", Message: "BPS rejected the request or returned an invalid tool envelope", HTTPStatus: 422}
	default:
		detail = &rpcError{Code: "stream_truncated", Message: "BPS returned a failed response", HTTPStatus: 502}
		switch upstream.Type {
		case "service_unavailable_error", "overloaded_error":
			detail = &rpcError{Code: "transient_overload", Message: "BPS upstream is overloaded; retry after the indicated delay", HTTPStatus: 503}
		case "rate_limit_error":
			detail = &rpcError{Code: "transient_rate_limit", Message: "BPS upstream rate limited", HTTPStatus: 429, RetryAfterSeconds: 1}
		}
	}
	for _, source := range []upstreamFailure{upstream, event.Error, event.upstreamFailure} {
		if n := upstreamRetryAfter(source); n > detail.RetryAfterSeconds {
			detail.RetryAfterSeconds = n
		}
	}
	return detail
}
