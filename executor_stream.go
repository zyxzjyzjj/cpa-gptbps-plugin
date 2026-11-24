package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const maxStreamStartupBytes = 64 << 20

func isTerminalResponse(kind string) bool {
	return kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" || kind == "error"
}

// Lifecycle events do not establish that generation succeeded. Hold them until
// actual output or a successful terminal event, so an early upstream rejection
// can still become an HTTP error rather than an apparently empty HTTP 200.
func streamHasOutput(frame []byte, kind string) bool {
	switch kind {
	case "response.completed", "response.incomplete":
		return true
	case "response.output_text.delta", "response.refusal.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.output_text.done", "response.refusal.done",
		"response.reasoning_text.done", "response.reasoning_summary_text.done",
		"response.output_item.added", "response.output_item.done", "response.content_part.done":
	default:
		return false
	}
	type content struct {
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	var event struct {
		Delta   string  `json:"delta"`
		Text    string  `json:"text"`
		Refusal string  `json:"refusal"`
		Part    content `json:"part"`
		Item    struct {
			Type    string    `json:"type"`
			Content []content `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(frame, []byte("data:"))), &event) != nil {
		return false
	}
	if event.Delta != "" || event.Text != "" || event.Refusal != "" || event.Part.Text != "" || event.Part.Refusal != "" {
		return true
	}
	if event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call" {
		return true
	}
	for _, part := range event.Item.Content {
		if part.Text != "" || part.Refusal != "" {
			return true
		}
	}
	return false
}

func streamFailure(err error) *rpcError {
	var detail *rpcError
	if !errors.As(err, &detail) {
		detail = &rpcError{Code: "stream_truncated", Message: "BPS stream ended before completion", HTTPStatus: 502}
	}
	return detail
}

// The worker keeps the run lease until every callback drains. On startup failure
// it returns the RPC error before joining an already blocked heartbeat: CPA must
// abort its pre-opened stream to release that callback's bounded queue.
func (p *plugin) executeStream(ctx context.Context, cancel context.CancelFunc, reader *io.PipeReader, done <-chan struct{}, cleanup func(bool), streamID string, headers http.Header) (any, error) {
	started := make(chan error, 1)
	monitorDone := make(chan struct{})
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
				// CPA ignores empty payloads while still checking cancellation,
				// including during startup before any content is committed.
				if p.host("host.stream.emit", map[string]any{"stream_id": streamID}, nil) != nil {
					cancel()
					return
				}
			}
		}
	}()
	go func() {
		defer p.runs.Done()
		published, terminalReceived := false, false
		var buffered [][]byte
		bufferedBytes := 0
		emit := func(frame []byte) error {
			return p.host("host.stream.emit", map[string]any{"stream_id": streamID, "payload": tagWireFrame(frame)}, nil)
		}
		err := consumeResponses(reader, func(frame, response []byte, kind string) error {
			if !published {
				if len(frame) > maxStreamStartupBytes-bufferedBytes {
					return &rpcError{Code: "stream_truncated", Message: "BPS stream startup exceeded the response buffer limit", HTTPStatus: 502}
				}
				buffered = append(buffered, frame)
				bufferedBytes += len(frame)
				if streamHasOutput(frame, kind) {
					published = true
					started <- nil
					for _, pending := range buffered {
						if err := emit(pending); err != nil {
							return err
						}
					}
					buffered = nil
				}
			} else if err := emit(frame); err != nil {
				return err
			}
			terminalReceived = isTerminalResponse(kind)
			return nil
		})
		cancelled := ctx.Err() != nil
		cleanup(terminalReceived)
		if !published {
			if cancelled && !terminalReceived {
				err = &rpcError{Code: "connection_lifecycle", Message: "BPS request cancelled before output", HTTPStatus: 499}
			}
			started <- streamFailure(err)
			<-monitorDone
			return
		}
		<-monitorDone
		closeReq := map[string]any{"stream_id": streamID}
		if err != nil {
			closeReq["error_detail"] = streamFailure(err)
		}
		_ = p.host("host.stream.close", closeReq, nil)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	headers.Set("Content-Type", "text/event-stream")
	return streamResponse{Headers: headers}, nil
}
