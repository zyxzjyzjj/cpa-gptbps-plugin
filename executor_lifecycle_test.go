package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type delayedCloseAckWriter struct {
	http.ResponseWriter
	started chan struct{}
	release <-chan struct{}
}

func (w delayedCloseAckWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	delayed := &delayedCloseAckConn{Conn: conn, started: w.started, release: w.release}
	rw.Writer = bufio.NewWriter(delayed)
	return delayed, rw, nil
}

type delayedCloseAckConn struct {
	net.Conn
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *delayedCloseAckConn) Write(raw []byte) (int, error) {
	if len(raw) > 0 && raw[0] == 0x88 {
		c.once.Do(func() { close(c.started) })
		<-c.release
	}
	return c.Conn.Write(raw)
}

func TestExecutorTerminalPreservesOriginalWebSocketClose(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"response.completed", "response.incomplete", "response.failed", "error"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, kind), func(t *testing.T) {
				peerClosed := make(chan websocket.StatusCode, 1)
				ackStarted := make(chan struct{})
				ackRelease := make(chan struct{})
				var releaseOnce sync.Once
				releaseAck := func() { releaseOnce.Do(func() { close(ackRelease) }) }
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := websocket.Accept(delayedCloseAckWriter{w, ackStarted, ackRelease}, r, &websocket.AcceptOptions{
						Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"},
					})
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
					defer cancel()
					var request map[string]any
					if err := wsjson.Read(ctx, conn, &request); err != nil {
						t.Error(err)
						return
					}
					if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.output_text.delta", "delta": "OK"}); err != nil {
						t.Error(err)
						return
					}
					event := map[string]any{"type": kind}
					if kind == "error" {
						event["error"] = map[string]any{"code": "invalid_request_error"}
					} else {
						response := map[string]any{
							"id": "resp-lifecycle", "status": kind[len("response."):],
							"output": []any{}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0, "total_tokens": 10},
						}
						if kind == "response.failed" {
							response["error"] = map[string]any{"code": "invalid_request_error"}
						}
						event["response"] = response
					}
					if err := wsjson.Write(ctx, conn, event); err != nil {
						t.Error(err)
						return
					}
					_, _, err = conn.Read(ctx)
					peerClosed <- websocket.CloseStatus(err)
				}))
				defer up.Close()
				hostClosed := make(chan map[string]any, 1)
				p := newPlugin(func(method string, request any, result any) error {
					if method == "host.stream.close" {
						hostClosed <- request.(map[string]any)
					}
					return nil
				})
				p.cfg.StateDir = filepath.Join(t.TempDir(), "state")
				p.cfg.upstream = up.URL
				defer p.close()
				defer releaseAck()
				req := executorRequest{
					AuthProvider: provider, Format: "codex", Model: "gpt-5.6-sol", StreamID: "lifecycle-fixture",
					Payload: []byte(`{"input":"hello"}`), StorageJSON: marshalTest(t, testCredential(t)),
				}
				requestDone := make(chan error, 1)
				raw := marshalTest(t, req)
				go func() { _, err := p.execute(raw, stream); requestDone <- err }()
				failed := kind == "response.failed" || kind == "error"
				if stream {
					if err := <-requestDone; err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-ackStarted:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream close handshake was not initiated")
				}
				var closed map[string]any
				var requestErr error
				returnedEarly := false
				if stream {
					select {
					case closed = <-hostClosed:
						returnedEarly = true
						t.Error("host stream closed before the standalone WebSocket cleanup completed")
					case <-time.After(50 * time.Millisecond):
					}
				} else {
					select {
					case requestErr = <-requestDone:
						returnedEarly = true
						t.Error("executor returned before the standalone WebSocket cleanup completed")
					case <-time.After(50 * time.Millisecond):
					}
				}
				releaseAck()
				if !returnedEarly {
					if stream {
						select {
						case closed = <-hostClosed:
						case <-time.After(3 * time.Second):
							t.Fatal("host stream did not close")
						}
					} else {
						select {
						case requestErr = <-requestDone:
						case <-time.After(3 * time.Second):
							t.Fatal("executor did not return")
						}
					}
				}
				if stream {
					if (closed["error_detail"] != nil) != failed {
						t.Fatal("terminal failure was lost in the host stream result")
					}
				} else if (requestErr != nil) != failed {
					t.Fatalf("terminal failure = %v, got %v", failed, requestErr)
				}
				select {
				case code := <-peerClosed:
					if code != websocket.StatusNormalClosure {
						t.Fatalf("CPA adapter closed terminal WebSocket with %d; standalone core requires 1000", code)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("upstream terminal WebSocket was not released")
				}
			})
		}
	}
}
