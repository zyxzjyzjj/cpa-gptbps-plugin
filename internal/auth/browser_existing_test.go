package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestExistingBrowserTabCaptureClosesOnlyItsPage(t *testing.T) {
	closed := make(chan string, 1)
	callback := BrowserRedirect + "?code=fixture-code&state=fixture-state"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		write := func(value any) bool {
			raw, _ := json.Marshal(value)
			return conn.Write(ctx, websocket.MessageText, raw) == nil
		}
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message browserMessage
			if json.Unmarshal(raw, &message) != nil {
				return
			}
			result := map[string]any{}
			if message.Method == "Page.getFrameTree" {
				result["frameTree"] = map[string]any{"frame": map[string]string{"id": "main"}}
			}
			if !write(map[string]any{"id": message.ID, "result": result}) {
				return
			}
			switch message.Method {
			case "Page.navigate":
				if !write(map[string]any{
					"method": "Fetch.requestPaused",
					"params": map[string]any{
						"requestId": "callback", "frameId": "main",
						"request": map[string]string{"url": callback},
					},
				}) {
					return
				}
			case "Page.close", "Browser.close":
				closed <- message.Method
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/owned"
	got, err := CaptureBrowserAuthorizationInTab(ctx, "https://auth.openai.com/oauth/authorize", endpoint)
	if err != nil || got != callback {
		t.Fatalf("callback was not captured: %v", err)
	}
	select {
	case method := <-closed:
		if method != "Page.close" {
			t.Fatal("shared browser was closed")
		}
	default:
		t.Fatal("owned authorization tab was not closed")
	}
}

func TestExistingBrowserTabRejectsUntrustedEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"ws://192.0.2.1:9222/devtools/page/owned",
		"ws://localhost:9222/devtools/page/owned",
		"wss://127.0.0.1:9222/devtools/page/owned",
		"ws://127.0.0.1:9222/devtools/browser/shared",
		"ws://127.0.0.1:9222/devtools/page/",
		"ws://user:secret@127.0.0.1:9222/devtools/page/owned",
		"ws://127.0.0.1:9222/devtools/page/owned?private=value",
	} {
		if _, err := CaptureBrowserAuthorizationInTab(context.Background(), "https://auth.openai.com/oauth/authorize", endpoint); err == nil {
			t.Fatal("untrusted endpoint was accepted")
		}
	}
	if _, err := CaptureBrowserAuthorizationInTab(context.Background(), "https://auth.openai.com.evil.invalid/oauth/authorize", "ws://127.0.0.1:9222/devtools/page/owned"); err == nil {
		t.Fatal("untrusted authorization page was accepted")
	}
}
