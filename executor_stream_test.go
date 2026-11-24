package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const startupCreated = `{"type":"response.created","response":{"id":"resp-startup","status":"in_progress","output":[]}}`
const startupProgress = `{"type":"response.in_progress","response":{"id":"resp-startup","status":"in_progress","output":[]}}`
const startupOverload = `{"type":"error","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"private-upstream-message","headers":{"retry-after":"7","retry-after-ms":"7000"}}}`

func newExecutorEventFixture(t *testing.T, events []string, host hostCall, beforeFinal ...func(context.Context)) (*plugin, *atomic.Int32) {
	t.Helper()
	attempts := new(atomic.Int32)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"},
		})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var request map[string]any
		if err := wsjson.Read(ctx, conn, &request); err != nil {
			return
		}
		attempts.Add(1)
		for index, event := range events {
			if index == len(events)-1 && len(beforeFinal) > 0 {
				beforeFinal[0](ctx)
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
				return
			}
		}
		// Acknowledge the original bridge's normal terminal close, or wait
		// for downstream cancellation in fixtures without a terminal event.
		_, _, _ = conn.Read(ctx)
	}))
	t.Cleanup(up.Close)
	p := newPlugin(host)
	p.cfg.StateDir = filepath.Join(t.TempDir(), "state")
	p.cfg.upstream = up.URL
	t.Cleanup(p.close)
	return p, attempts
}

func eventFixtureRequest(t *testing.T, payload any) []byte {
	t.Helper()
	return marshalTest(t, executorRequest{
		AuthProvider: provider, Format: "codex", Model: "gpt-5.6-sol", StreamID: "startup-fixture",
		Payload: marshalTest(t, payload), StorageJSON: marshalTest(t, testCredential(t)),
	})
}

func TestStreamStartupErrorsReturnFailureBeforeAnyClientPayload(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			event  string
			status int
			retry  int
		}{
			{"overload", startupOverload, 503, 7},
			{"rate_limit", `{"type":"response.failed","response":{"status":"failed","output":[],"error":{"code":"rate_limit_exceeded","headers":{"retry-after-ms":1250}}}}`, 429, 2},
			{"invalid_token", `{"type":"error","error":{"code":"invalid_token"}}`, 401, 0},
			{"model_unavailable", `{"type":"error","error":{"code":"model_not_found"}}`, 404, 0},
		} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, tc.name), func(t *testing.T) {
				var payloads, streamCloses atomic.Int32
				p, attempts := newExecutorEventFixture(t, []string{
					startupCreated, startupProgress,
					`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg-empty","type":"message","role":"assistant","content":[]}}`,
					`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
					tc.event,
				}, func(method string, request any, result any) error {
					if method == "host.stream.emit" {
						if body, _ := request.(map[string]any)["payload"].([]byte); len(body) > 0 {
							payloads.Add(1)
						}
					}
					if method == "host.stream.close" {
						streamCloses.Add(1)
					}
					return nil
				})
				_, err := p.execute(eventFixtureRequest(t, map[string]any{"input": "hello"}), stream)
				detail, ok := err.(*rpcError)
				if !ok || detail.HTTPStatus != tc.status || detail.RetryAfterSeconds != tc.retry {
					t.Fatalf("startup error status/retry = %#v", err)
				}
				if strings.Contains(detail.Message, "private-upstream-message") {
					t.Fatal("arbitrary upstream error body escaped the credential boundary")
				}
				if payloads.Load() != 0 || streamCloses.Load() != 0 {
					t.Fatal("failed startup committed lifecycle events or a successful stream")
				}
				if attempts.Load() != 1 {
					t.Fatal("plugin replayed the rejected generation")
				}
			})
		}
	}
}

func TestLateStreamOverloadRetainsPartialOutputAndFailure(t *testing.T) {
	var mu sync.Mutex
	var frames [][]byte
	closed := make(chan map[string]any, 1)
	p, attempts := newExecutorEventFixture(t, []string{
		startupCreated, startupProgress, `{"type":"response.output_text.delta","delta":"partial"}`, startupOverload,
	}, func(method string, request any, result any) error {
		m := request.(map[string]any)
		if method == "host.stream.emit" {
			if body, _ := m["payload"].([]byte); len(body) > 0 {
				mu.Lock()
				frames = append(frames, append([]byte(nil), body...))
				mu.Unlock()
			}
		}
		if method == "host.stream.close" {
			closed <- m
		}
		return nil
	})
	if _, err := p.execute(eventFixtureRequest(t, map[string]any{"input": "hello"}), true); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-closed:
		detail, ok := result["error_detail"].(*rpcError)
		if !ok || detail.HTTPStatus != 503 || detail.RetryAfterSeconds != 7 {
			t.Fatal("late upstream overload was reported as success or lost its retry hint")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failed stream did not finish")
	}
	mu.Lock()
	body := bytes.Join(frames, nil)
	mu.Unlock()
	if !bytes.Contains(body, []byte(`"delta":"partial"`)) || !bytes.Contains(body, []byte(`"code":"server_is_overloaded"`)) || bytes.Contains(body, []byte("response.completed")) {
		t.Fatal("late failure discarded partial output or invented a completed response")
	}
	if bytes.Index(body, []byte("response.created")) > bytes.Index(body, []byte("response.output_text.delta")) || attempts.Load() != 1 {
		t.Fatal("startup changed event order or replayed a partially delivered generation")
	}
}

func TestStreamStartupPreservesToolOnlyResponses(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			tool := map[string]any{"type": kind, "name": "fixture_tool"}
			item := map[string]any{
				"type": kind + "_call", "id": "tool-fixture", "call_id": "call-fixture",
				"name": "fixture_tool", "status": "completed",
			}
			if kind == "function" {
				tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
				item["arguments"] = "{}"
			} else {
				tool["format"] = map[string]any{"type": "text"}
				item["type"] = "custom_tool_call"
				item["input"] = "tool input"
			}
			event := marshalTest(t, map[string]any{"type": "response.completed", "response": map[string]any{
				"id": "resp-tool", "status": "completed", "output": []any{item},
				"usage": map[string]any{"input_tokens": 100, "output_tokens": 5, "total_tokens": 105},
			}})
			var mu sync.Mutex
			var frames [][]byte
			closed := make(chan map[string]any, 1)
			p, attempts := newExecutorEventFixture(t, []string{startupCreated, startupProgress, string(event)}, func(method string, request any, result any) error {
				m := request.(map[string]any)
				if method == "host.stream.emit" {
					if body, _ := m["payload"].([]byte); len(body) > 0 {
						mu.Lock()
						frames = append(frames, append([]byte(nil), body...))
						mu.Unlock()
					}
				}
				if method == "host.stream.close" {
					closed <- m
				}
				return nil
			})
			req := eventFixtureRequest(t, map[string]any{"input": "use the tool", "tools": []any{tool}})
			if _, err := p.execute(req, true); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-closed:
				if result["error_detail"] != nil {
					t.Fatalf("tool-only response rejected: %#v", result["error_detail"])
				}
			case <-time.After(3 * time.Second):
				t.Fatal("tool-only stream did not finish")
			}
			mu.Lock()
			body := bytes.Join(frames, nil)
			mu.Unlock()
			if !bytes.Contains(body, []byte(`"call_id":"call-fixture"`)) || !bytes.Contains(body, []byte(`"total_tokens":105`)) || !bytes.Contains(body, []byte("response.completed")) || attempts.Load() != 1 {
				t.Fatal("tool-only output, terminal usage or request identity was lost")
			}
		})
	}
}

func TestStreamStartupCancellationDoesNotCommitAnEmptySuccess(t *testing.T) {
	var emitted atomic.Int32
	p, attempts := newExecutorEventFixture(t, []string{startupCreated}, func(method string, request any, result any) error {
		if method == "host.stream.emit" {
			if body, _ := request.(map[string]any)["payload"].([]byte); len(body) == 0 {
				return fmt.Errorf("synthetic downstream cancellation")
			}
			emitted.Add(1)
		}
		return nil
	})
	_, err := p.execute(eventFixtureRequest(t, map[string]any{"input": "hello"}), true)
	detail, ok := err.(*rpcError)
	if !ok || detail.HTTPStatus != 499 || emitted.Load() != 0 || attempts.Load() != 1 {
		t.Fatalf("cancelled startup returned an empty success or leaked a request: %#v", err)
	}
}

func TestSlowStartupFailureDoesNotDeadlockOnHostBackpressure(t *testing.T) {
	// CPA queues even empty heartbeat chunks before ExecuteStream returns.
	// A one-entry queue reaches the same boundary as its native 16-entry queue
	// without making this cancellation regression wait seventeen seconds.
	queue := make(chan struct{}, 1)
	hostAbort := make(chan struct{})
	heartbeatBlocked := make(chan struct{})
	var blockedOnce sync.Once
	var abortOnce sync.Once
	abort := func() { abortOnce.Do(func() { close(hostAbort) }) }
	p, attempts := newExecutorEventFixture(t, []string{startupCreated, startupOverload}, func(method string, request any, result any) error {
		if method == "host.stream.emit" {
			if body, _ := request.(map[string]any)["payload"].([]byte); len(body) > 0 {
				return fmt.Errorf("unexpected startup payload")
			}
			select {
			case queue <- struct{}{}:
				return nil
			default:
				blockedOnce.Do(func() { close(heartbeatBlocked) })
			}
			select {
			case <-hostAbort:
				return fmt.Errorf("native stream aborted after failed RPC")
			}
		}
		return nil
	}, func(ctx context.Context) {
		select {
		case <-heartbeatBlocked:
		case <-ctx.Done():
		}
	})
	// Abort before cleanup even if an assertion fails.
	t.Cleanup(abort)
	finished := make(chan error, 1)
	request := eventFixtureRequest(t, map[string]any{"input": "hello"})
	go func() {
		_, err := p.execute(request, true)
		abort() // Native CPA does this as soon as a startup RPC fails.
		finished <- err
	}()
	select {
	case err := <-finished:
		detail, ok := err.(*rpcError)
		if !ok || detail.HTTPStatus != 503 || detail.RetryAfterSeconds != 7 {
			t.Fatalf("slow startup error lost overload details: %#v", err)
		}
	case <-time.After(4 * time.Second):
		abort()
		t.Fatal("startup error waited for the queue that only its return can release")
	}
	p.close()
	if attempts.Load() != 1 {
		t.Fatal("slow failed startup replayed the generation")
	}
}

func TestEventFailurePreservesBoundedUpstreamRetryHints(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		status int
		retry  int
	}{
		{startupOverload, 503, 7},
		{`{"type":"error","code":"request_rejected","headers":{"Retry-After":["5"]}}`, 429, 5},
		{`{"type":"error","error":{"type":"service_unavailable_error","headers":{"retry-after-ms":"7001"}}}`, 503, 8},
		{`{"type":"response.failed","headers":{"retry-after":4},"response":{"error":{"code":"server_is_overloaded"}}}`, 503, 4},
		{`{"type":"error","error":{"code":"server_is_overloaded","headers":{"retry-after":"999999999","retry-after-ms":"9223372036854775807"}}}`, 503, 604800},
		{`{"type":"error","error":{"code":"rate_limit_exceeded","headers":{"retry-after":"-3","retry-after-ms":"invalid"}}}`, 429, 1},
		{`{"type":"error","error":{"code":"insufficient_quota"}}`, 429, 0},
		{`{"type":"error","error":{"code":"basispoints_ws_disconnected"}}`, 502, 0},
	} {
		detail := eventFailure([]byte(tc.raw))
		if detail.HTTPStatus != tc.status || detail.RetryAfterSeconds != tc.retry {
			t.Fatalf("failure status/retry = %#v for %s", detail, tc.raw)
		}
	}
}
