package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func randomID() string {
	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("secure randomness unavailable")
	}
	return hex.EncodeToString(data[:])
}

type pipeResponse struct {
	headers http.Header
	status  int
	ready   chan struct{}
	writer  *io.PipeWriter
	once    sync.Once
}

func (w *pipeResponse) Header() http.Header { return w.headers }
func (w *pipeResponse) WriteHeader(status int) {
	w.once.Do(func() { w.status = status; close(w.ready) })
}
func (w *pipeResponse) Write(raw []byte) (int, error) {
	w.WriteHeader(200)
	return w.writer.Write(raw)
}
func (w *pipeResponse) Flush() { w.WriteHeader(200) }

func (p *plugin) execute(raw []byte, stream bool) (any, error) {
	if err := p.beginRun(); err != nil {
		return nil, err
	}
	async := false
	defer func() {
		if !async {
			p.runs.Done()
		}
	}()
	var req executorRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.AuthProvider != provider {
		return nil, badRequest("executor requires a codex-bps credential")
	}
	if req.Format != "openai-response" && req.Format != "responses" && req.Format != "codex" {
		return nil, badRequest("codex-bps accepts Responses payloads; enable CPA native translation")
	}
	if req.Alt != "" {
		return nil, &rpcError{Code: "not_implemented", Message: "BPS does not support alternate Responses routes such as compact", HTTPStatus: 501}
	}
	if stream && req.StreamID == "" {
		return nil, badRequest("CPA stream_id required")
	}
	c, err := readCredential(materialJSON(req.StorageJSON, req.AuthMetadata))
	if err != nil {
		return nil, err
	}
	if _, _, err = c.Credentials(); err != nil {
		return nil, err
	}
	if c.Disabled {
		return nil, &rpcError{Code: "credential_disabled", Message: "BPS credential disabled", HTTPStatus: 403}
	}
	var payload map[string]json.RawMessage
	if len(req.Payload) > 64<<20 || json.Unmarshal(req.Payload, &payload) != nil || payload == nil {
		return nil, badRequest("invalid or oversized Responses request")
	}
	// The selected host model is authoritative after alias resolution.
	payload["model"], _ = json.Marshal(req.Model)
	payload["stream"] = json.RawMessage("true")
	body, _ := json.Marshal(payload)
	handler, err := p.gateway()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(p.ctx)
	reader, writer := io.Pipe()
	rw := &pipeResponse{headers: http.Header{}, ready: make(chan struct{}), writer: writer}
	done := make(chan struct{})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://bps.internal/v1/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	// No client task/session headers select server-owned BPS task identities.
	go func() {
		defer close(done)
		defer writer.Close()
		handler.ServeWithCredentials(rw, request, c, p.effectiveProxy(c.ProxyURL))
		rw.WriteHeader(200)
	}()
	cleanup := func() { cancel(); _ = reader.Close(); <-done }
	select {
	case <-rw.ready:
	case <-ctx.Done():
		cleanup()
		return nil, &rpcError{Code: "connection_lifecycle", Message: "BPS request cancelled", HTTPStatus: 499}
	}
	headers := publicHeaders(rw.headers)
	if rw.status >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(reader, 1<<20))
		cleanup()
		return nil, bridgeFailure(rw.status, errorBody, rw.headers)
	}
	if stream {
		async = true
		monitorDone := make(chan struct{})
		go func() {
			defer func() { cleanup(); <-monitorDone; p.runs.Done() }()
			err := consumeResponses(reader, func(frame, response []byte, kind string) error {
				return p.host("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": tagWireFrame(frame)}, nil)
			})
			closeReq := map[string]any{"stream_id": req.StreamID}
			if err != nil {
				var detail *rpcError
				if !errors.As(err, &detail) {
					detail = &rpcError{Code: "stream_truncated", Message: "BPS stream ended before completion", HTTPStatus: 502}
				}
				closeReq["error_detail"] = detail
			}
			_ = p.host("host.stream.close", closeReq, nil)
		}()
		// Empty chunks are ignored by CPA. They let a silent BPS stream observe
		// downstream cancellation even while no upstream token is arriving.
		go func() {
			defer close(monitorDone)
			timer := time.NewTicker(time.Second)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-done:
					return
				case <-timer.C:
					if p.host("host.stream.emit", map[string]any{"stream_id": req.StreamID}, nil) != nil {
						cancel()
						return
					}
				}
			}
		}()
		headers.Set("Content-Type", "text/event-stream")
		return streamResponse{Headers: headers}, nil
	}
	defer cleanup()
	var response []byte
	var terminalKind string
	err = consumeResponses(reader, func(frame, terminal []byte, kind string) error {
		if len(terminal) > 0 {
			response = append(response[:0], terminal...)
			terminalKind = kind
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(response) == 0 {
		return nil, &rpcError{Code: "stream_truncated", Message: "BPS omitted the terminal response", HTTPStatus: 502}
	}
	headers.Set("Content-Type", "application/json")
	// CPA's Codex output contract is a terminal event. Besides reusing its
	// native unwrapping/translation, nesting usage avoids older CPA parsers
	// mistaking top-level prompt_cache_retention alone for complete usage.
	// Clients still receive the original Responses object after host translation.
	event, _ := json.Marshal(map[string]any{"type": terminalKind, "response": json.RawMessage(response)})
	return executorResponse{Payload: event, Headers: headers}, nil
}

// consumeResponses preserves full terminal usage and all native tool events.
// There is no token estimation or latency-based cache inference.
func consumeResponses(reader io.Reader, consume func([]byte, []byte, string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 32<<10), 64<<20)
	var data []byte
	terminal := false
	var terminalErr error
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := bytes.TrimSuffix(data, []byte("\n"))
		data = nil
		if bytes.Equal(raw, []byte("[DONE]")) {
			return nil
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Error    json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return &rpcError{Code: "stream_truncated", Message: "Invalid BPS event", HTTPStatus: 502}
		}
		var final []byte
		switch event.Type {
		case "response.completed", "response.incomplete":
			terminal = true
			final = event.Response
			if len(final) == 0 || string(final) == "null" {
				return &rpcError{Code: "stream_truncated", Message: "BPS omitted the terminal response", HTTPStatus: 502}
			}
		case "response.failed", "error":
			terminal = true
			terminalErr = eventFailure(raw)
		}
		frame := append(append([]byte("data: "), raw...), '\n', '\n')
		if err := consume(frame, final, event.Type); err != nil {
			return err
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			if terminal {
				return terminalErr
			}
		} else if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if len(data)+len(value) > 64<<20 {
				return &rpcError{Code: "stream_truncated", Message: "BPS event exceeded size limit", HTTPStatus: 502}
			}
			data = append(data, value...)
			data = append(data, '\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return &rpcError{Code: "stream_truncated", Message: "BPS stream read failed before completion", HTTPStatus: 502}
	}
	if err := flush(); err != nil {
		return err
	}
	if !terminal {
		return &rpcError{Code: "stream_truncated", Message: "BPS stream ended without a terminal event", HTTPStatus: 502}
	}
	return terminalErr
}
func eventFailure(raw []byte) *rpcError {
	var event struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(raw, &event)
	code := event.Response.Error.Code
	if code == "" {
		code = event.Error.Code
	}
	switch code {
	case "usage_limit_reached", "insufficient_quota", "quota_exceeded":
		return &rpcError{Code: "credential_quota", Message: "BPS account quota exhausted", HTTPStatus: 429, CredentialScoped: true}
	case "rate_limit_exceeded", "rate_limit_error":
		return &rpcError{Code: "transient_rate_limit", Message: "BPS upstream rate limited", HTTPStatus: 429, RetryAfterSeconds: 1}
	case "invalid_api_key", "token_expired", "invalid_token", "authentication_error":
		return &rpcError{Code: "credential_invalid", Message: "BPS credential rejected", HTTPStatus: 401}
	case "model_not_found", "model_not_available":
		return &rpcError{Code: "model_not_found", Message: "Model is unavailable on this BPS credential", HTTPStatus: 404}
	case "invalid_request_error", "basispoints_protocol_error":
		return &rpcError{Code: "request_scoped", Message: "BPS rejected the request or returned an invalid tool envelope", HTTPStatus: 422}
	}
	return &rpcError{Code: "stream_truncated", Message: "BPS returned a failed response", HTTPStatus: 502}
}
func bridgeFailure(status int, body []byte, headers http.Header) *rpcError {
	var result struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &result)
	detail := &rpcError{Code: "upstream_error", Message: "BPS upstream request failed", HTTPStatus: status}
	switch {
	case status == 400 || status == 422:
		detail.Code = "request_scoped"
		detail.Message = "BPS request invalid; supply full history, supported image input and client tool definitions"
		switch result.Error.Message {
		case "basispoints supports tool_choice auto or none only",
			"basispoints requires expanded history instead of previous_response_id",
			"basispoints input_image requires an HTTPS image_url; file IDs are unsupported",
			"basispoints input_image requires an absolute HTTPS image URL without embedded credentials",
			"basispoints input_image does not support file_id; provide only an HTTPS image_url",
			"basispoints does not accept data:image/base64 image input; provide an HTTPS image URL, or disable Basispoints and start a new conversation to send this image":
			detail.Message = result.Error.Message
		}
	case status == 401:
		detail.Code = "credential_invalid"
		detail.Message = "BPS credential unavailable or expired"
	case status == 429:
		detail.Code = "transient_rate_limit"
		detail.Message = "BPS queue or upstream start limit reached"
		detail.RetryAfterSeconds = 1
	case status == 499:
		detail.Code = "connection_lifecycle"
		detail.Message = "BPS request cancelled"
	case status == 502:
		detail.Code = "transient_transport"
		detail.Message = "BPS WebSocket connection failed"
	}
	if n, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && n > 0 {
		detail.RetryAfterSeconds = n
	}
	return detail
}
func publicHeaders(source http.Header) http.Header {
	out := http.Header{}
	for key, values := range source {
		if strings.HasPrefix(http.CanonicalHeaderKey(key), "X-Bps-") || strings.EqualFold(key, "Retry-After") {
			out[key] = append([]string(nil), values...)
		}
	}
	out.Set("Cache-Control", "no-store")
	return out
}
