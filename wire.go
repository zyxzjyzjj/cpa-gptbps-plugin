package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Wire fields mirror CPA sdk/pluginapi and internal/pluginhost/rpc_schema.go.
// Byte fields are base64 in JSON. No Go host dependency crosses the C ABI.
type hostCall func(string, any, any) error

// host.http.do marshals sdk/pluginapi.HTTPResponse without JSON field tags.
// The snake_case status_code field belongs to the separate streaming callback.
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	CredentialScoped  bool   `json:"credential_scoped,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }
func badRequest(message string) *rpcError {
	return &rpcError{Code: "request_scoped", Message: message, HTTPStatus: 400}
}
func success(result any) []byte {
	body, err := json.Marshal(result)
	if err != nil {
		return failure(err)
	}
	raw, _ := json.Marshal(envelope{OK: true, Result: body})
	return raw
}
func failure(err error) []byte {
	var detail *rpcError
	if !errors.As(err, &detail) {
		// Arbitrary dependency errors can contain secrets. Keep this boundary fixed.
		detail = &rpcError{Code: "plugin_error", Message: "codex-bps operation failed; check plugin configuration", HTTPStatus: 500}
	}
	raw, _ := json.Marshal(envelope{Error: detail})
	return raw
}
func decode(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return badRequest("invalid plugin JSON request")
	}
	return nil
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}
type hostConfig struct {
	AuthDir  string
	ProxyURL string
}
type authData struct {
	Provider         string
	ID               string
	FileName         string
	Label            string
	Prefix           string
	ProxyURL         string
	Disabled         bool
	StorageJSON      []byte
	Metadata         map[string]any
	Attributes       map[string]string
	NextRefreshAfter time.Time
}
type authParseRequest struct {
	Provider string
	Path     string
	FileName string
	RawJSON  []byte
	Host     hostConfig
}
type authMaterial struct {
	AuthID         string
	AuthProvider   string
	StorageJSON    []byte
	Metadata       map[string]any
	Attributes     map[string]string
	Host           hostConfig
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type loginRequest struct {
	Provider       string
	BaseURL        string
	State          string
	Host           hostConfig
	Metadata       map[string]any
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type loginStart struct {
	Provider  string
	URL       string
	State     string
	ExpiresAt time.Time
	Metadata  map[string]any
}
type loginPoll struct {
	Status  string
	Message string
	Auth    *authData `json:",omitempty"`
}
type executorRequest struct {
	AuthID          string
	AuthProvider    string
	Model           string
	Format          string
	Stream          bool
	Alt             string
	Headers         http.Header
	Query           url.Values
	OriginalRequest []byte
	SourceFormat    string
	Payload         []byte
	Metadata        map[string]any
	StorageJSON     []byte
	AuthMetadata    map[string]any
	AuthAttributes  map[string]string
	StreamID        string `json:"stream_id,omitempty"`
	HostCallbackID  string `json:"host_callback_id,omitempty"`
}
type executorResponse struct {
	Payload []byte
	Headers http.Header
}
type streamResponse struct {
	Headers http.Header `json:"headers"`
}
type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type modelInfo struct {
	ID                        string
	Object                    string
	OwnedBy                   string
	Type                      string
	DisplayName               string
	Description               string
	SupportedParameters       []string
	SupportedInputModalities  []string
	SupportedOutputModalities []string
	Thinking                  *thinkingSupport
}
type thinkingSupport struct{ Levels []string }
