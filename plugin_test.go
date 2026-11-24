package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	bpsauth "github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/auth"
)

func testCredential(t *testing.T) credential {
	t.Helper()
	claims := map[string]any{"client_id": bpsauth.ExcelClientID, "sub": "user-fixture", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "fixture-account"}}
	raw, _ := json.Marshal(claims)
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".fixture"
	c, err := readCredential([]byte(fmt.Sprintf(`{"type":"codex-bps","access_token":%q,"client_id":%q}`, token, bpsauth.ExcelClientID)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func marshalTest(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCredentialBoundary(t *testing.T) {
	c := testCredential(t)
	data := credentialData(c, "")
	if data.Provider != provider || data.Metadata["type"] != provider || data.Attributes["account_id"] != "fixture-account" {
		t.Fatal("missing native credential attribution")
	}
	if data.NextRefreshAfter.Before(time.Now().Add(time.Hour)) {
		t.Fatal("AT-only import must not trigger refresh attempts")
	}
	for _, raw := range []string{
		`{"type":"codex","access_token":"fixture","client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}`,
		`{"type":"codex-bps","refresh_token":"fixture","client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}`,
		`{"client_id":"` + bpsauth.ExcelClientID + `","access_token":"fixture","tokens":{"access_token":"other"}}`,
	} {
		if _, err := readCredential([]byte(raw)); err == nil {
			t.Fatal("accepted incompatible or ambiguous credential")
		}
	}
	p := newPlugin(nil)
	defer p.close()
	result, err := p.parseAuth(marshalTest(t, authParseRequest{RawJSON: []byte(`{"type":"codex","access_token":"private"}`)}))
	if err != nil || result.(map[string]any)["Handled"] != false {
		t.Fatal("claimed native Codex credential")
	}
}

func TestTerminalAccountingAndFailures(t *testing.T) {
	usage := `{"input_tokens":1000,"output_tokens":12,"input_tokens_details":{"cached_tokens":768},"output_tokens_details":{"reasoning_tokens":4},"total_tokens":1012}`
	body := `data: {"type":"response.completed","response":{"id":"resp-fixture","output":[],"usage":` + usage + `}}` + "\n\n"
	var final []byte
	err := consumeResponses(strings.NewReader(body), func(frame, response []byte, _ string) error { final = response; return nil })
	if err != nil || !bytes.Contains(final, []byte(`"cached_tokens":768`)) || !bytes.Contains(final, []byte(`"reasoning_tokens":4`)) {
		t.Fatal("terminal usage lost")
	}
	for _, body := range []string{`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n", "data: invalid\n\n"} {
		if consumeResponses(strings.NewReader(body), func([]byte, []byte, string) error { return nil }) == nil {
			t.Fatal("accepted truncated stream")
		}
	}
	failed := `data: {"type":"response.failed","response":{"error":{"code":"usage_limit_reached"},"usage":` + usage + `}}` + "\n\n"
	count := 0
	err = consumeResponses(strings.NewReader(failed), func([]byte, []byte, string) error { count++; return nil })
	detail, ok := err.(*rpcError)
	if !ok || detail.HTTPStatus != 429 || !detail.CredentialScoped || count != 1 {
		t.Fatal("quota failure misreported")
	}
}

func TestWireNormalizerPreservesClientEventAndOtherProviders(t *testing.T) {
	frame := []byte("data: " + `{"type":"response.output_text.delta","delta":"OK"}` + "\n\n")
	tagged := tagWireFrame(frame)
	if bytes.Equal(frame, tagged) {
		t.Fatal("wire marker missing")
	}
	request := map[string]any{"FromFormat": "codex", "ToFormat": "openai-response", "Stream": true, "Body": tagged}
	result, err := normalizeWireResponse(marshalTest(t, request))
	if err != nil || !bytes.Equal(result.(map[string]any)["Body"].([]byte), frame) {
		t.Fatal("wire marker changed client event")
	}
	for _, body := range [][]byte{frame, []byte("data: " + `{"type":"response.completed","response":{"usage":{"input_tokens":10}}}` + "\n\n")} {
		request["Body"] = body
		result, err = normalizeWireResponse(marshalTest(t, request))
		if err != nil || !bytes.Equal(result.(map[string]any)["Body"].([]byte), body) {
			t.Fatal("modified an untagged native-provider response")
		}
	}
}

func TestExecutorUsesOriginalBridgeAndPreservesUsage(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	tasks := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var request map[string]any
		if err = wsjson.Read(ctx, c, &request); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if request["store"] != false || request["type"] != "response.create" || request["model"] != "gpt-5.6-sol" {
			t.Error("core request invariants lost")
		}
		metadata, _ := request["metadata"].(map[string]any)
		mu.Lock()
		tasks = append(tasks, fmt.Sprint(metadata["task_id"]))
		mu.Unlock()
		_ = wsjson.Write(ctx, c, map[string]any{"type": "response.output_text.delta", "delta": "OK"})
		_ = wsjson.Write(ctx, c, map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp-fixture", "object": "response", "model": "gpt-5.6-sol", "status": "completed", "output": []any{},
			"usage": map[string]any{"input_tokens": 1024, "output_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 768}, "output_tokens_details": map[string]any{"reasoning_tokens": 1}, "total_tokens": 1026}}})
	}))
	defer up.Close()
	var emitted [][]byte
	closed := make(chan struct{})
	p := newPlugin(func(method string, req any, result any) error {
		mu.Lock()
		defer mu.Unlock()
		m := req.(map[string]any)
		if method == "host.stream.emit" {
			if body, ok := m["payload"].([]byte); ok {
				emitted = append(emitted, append([]byte(nil), body...))
			}
		}
		if method == "host.stream.close" {
			close(closed)
		}
		return nil
	})
	p.cfg.StateDir = filepath.Join(t.TempDir(), "state")
	p.cfg.upstream = up.URL
	defer p.close()
	c := testCredential(t)
	req := executorRequest{AuthProvider: provider, Format: "openai-response", Model: "gpt-5.6-sol", Payload: []byte(`{"input":"hello"}`), StorageJSON: marshalTest(t, c)}
	result, err := p.execute(marshalTest(t, req), false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(result.(executorResponse).Payload, []byte(`"cached_tokens":768`)) {
		t.Fatal("nonstream lost cache usage")
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(result.(executorResponse).Payload, &event) != nil || event.Type != "response.completed" || len(event.Response.Usage) == 0 {
		t.Fatal("CPA Codex wire must retain nested terminal usage")
	}
	req.StreamID = "fixture-stream"
	if _, err = p.execute(marshalTest(t, req), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not close")
	}
	mu.Lock()
	joined := bytes.Join(emitted, nil)
	mu.Unlock()
	if !bytes.Contains(joined, []byte(`"cached_tokens":768`)) || !bytes.Contains(joined, []byte(`"delta":"OK"`)) || calls.Load() != 2 {
		t.Fatal("stream or upstream execution mismatch")
	}
}

func TestRefreshReceiptAndAmbiguousGrantAreNotReplayed(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			c := testCredential(t)
			c.RefreshToken = "synthetic-one-use-grant"
			c.ExpiresIn = 3600
			p := newPlugin(nil)
			p.cfg.StateDir = filepath.Join(t.TempDir(), "state")
			defer p.close()
			calls := 0
			p.host = func(method string, request any, result any) error {
				if method != "host.http.do" {
					t.Fatalf("unexpected callback %s", method)
				}
				calls++
				req := request.(map[string]any)
				form, _ := url.ParseQuery(string(req["body"].([]byte)))
				if form.Get("client_id") != bpsauth.ExcelClientID || form.Get("grant_type") != "refresh_token" {
					t.Fatal("wrong refresh issuer")
				}
				if !success {
					return fmt.Errorf("synthetic response loss")
				}
				reply := map[string]any{"access_token": c.AccessToken, "refresh_token": "rotated-fixture", "expires_in": 3600, "token_type": "Bearer"}
				body, _ := json.Marshal(reply)
				raw, _ := json.Marshal(map[string]any{"StatusCode": 200, "Body": body})
				return json.Unmarshal(raw, result)
			}
			request := marshalTest(t, authMaterial{AuthID: "fixture.json", AuthProvider: provider, StorageJSON: marshalTest(t, c)})
			first, firstErr := p.refresh(request)
			second, secondErr := p.refresh(request)
			if calls != 1 {
				t.Fatal("rotating grant was replayed")
			}
			if success {
				if firstErr != nil || secondErr != nil {
					t.Fatalf("receipt recovery failed: %v / %v", firstErr, secondErr)
				}
				for _, result := range []any{first, second} {
					data := result.(map[string]any)["Auth"].(authData)
					next, err := readCredential(data.StorageJSON)
					if err != nil || next.RefreshToken != "rotated-fixture" {
						t.Fatal("rotation not preserved")
					}
				}
			} else if firstErr == nil || secondErr == nil {
				t.Fatal("ambiguous refresh must require reauthorization")
			}
			entries, _ := os.ReadDir(p.cfg.StateDir + "-refresh")
			if len(entries) == 0 {
				t.Fatal("missing durable rotation guard")
			}
		})
	}
}

func TestPublicResourceDoesNotExecutePrivilegedActions(t *testing.T) {
	p := newPlugin(func(string, any, any) error { t.Fatal("public resource invoked host callback"); return nil })
	defer p.close()
	result, err := p.manage(marshalTest(t, managementRequest{Method: "GET", Path: "/v0/resource/plugins/codex-bps/panel", Query: map[string][]string{"import": {"private"}}}))
	if err != nil {
		t.Fatal(err)
	}
	resp := result.(managementResponse)
	if resp.StatusCode != 200 || !bytes.Contains(resp.Body, []byte("BPS")) || strings.Contains(string(resp.Body), "<script src=") {
		t.Fatal("invalid bundled panel")
	}
}

func TestManualLoginPKCEAndCallbackGuards(t *testing.T) {
	p := newPlugin(nil)
	defer p.close()
	start, err := p.startLogin(loginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	flow := start.(loginStart)
	u, _ := url.Parse(flow.URL)
	q := u.Query()
	if q.Get("client_id") != bpsauth.ExcelClientID || q.Get("redirect_uri") != bpsauth.BrowserRedirect || q.Get("code_challenge_method") != "S256" || len(flow.State) > 128 {
		t.Fatal("BPS PKCE contract lost")
	}
	if p.submitCallback(flow.State, "http://localhost:1455/auth/callback?code=fixture&state="+flow.State) == nil {
		t.Fatal("accepted Codex redirect")
	}
	if p.submitCallback(flow.State, bpsauth.BrowserRedirect+"?code=fixture&state=other") == nil {
		t.Fatal("accepted wrong OAuth state")
	}
}

func TestNativeCPACallbackFileCompletesBPSLogin(t *testing.T) {
	c := testCredential(t)
	exchanges := atomic.Int32{}
	p := newPlugin(func(method string, request any, result any) error {
		if method != "host.http.do" {
			t.Fatalf("unexpected callback %s", method)
		}
		exchanges.Add(1)
		req := request.(map[string]any)
		form, _ := url.ParseQuery(string(req["body"].([]byte)))
		if form.Get("client_id") != bpsauth.ExcelClientID || form.Get("redirect_uri") != bpsauth.BrowserRedirect || form.Get("code") != "fixture-code" || form.Get("code_verifier") == "" {
			t.Fatal("native callback lost BPS PKCE contract")
		}
		body, _ := json.Marshal(map[string]any{"access_token": c.AccessToken, "refresh_token": "fixture-refresh", "token_type": "Bearer", "expires_in": 3600})
		raw, _ := json.Marshal(map[string]any{"StatusCode": 200, "Body": body})
		return json.Unmarshal(raw, result)
	})
	defer p.close()
	dir := t.TempDir()
	start, err := p.startLogin(loginRequest{Host: hostConfig{AuthDir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	flow := start.(loginStart)
	callbackPath := filepath.Join(dir, ".oauth-"+provider+"-"+flow.State+".oauth")
	if err = os.WriteFile(callbackPath, marshalTest(t, map[string]any{"state": flow.State, "code": "fixture-code"}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = p.pollLogin(loginRequest{State: flow.State, Host: hostConfig{AuthDir: dir}}); err != nil {
		t.Fatal(err)
	}
	session, _ := p.findLogin(flow.State)
	select {
	case <-session.done:
	case <-time.After(3 * time.Second):
		t.Fatal("native callback did not complete")
	}
	result, err := p.pollLogin(loginRequest{State: flow.State, Host: hostConfig{AuthDir: dir}})
	if err != nil || result.(loginPoll).Status != "success" || result.(loginPoll).Auth.Provider != provider || exchanges.Load() != 1 {
		t.Fatal("native login not persisted as BPS auth")
	}
	if _, err = os.Stat(callbackPath); !os.IsNotExist(err) {
		t.Fatal("consumed callback left behind")
	}
}

func TestDownstreamCallbackFailureClosesUpstreamAndDrains(t *testing.T) {
	disconnected := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, OriginPatterns: []string{"bps.openai.com"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var request map[string]any
		if wsjson.Read(ctx, conn, &request) != nil {
			return
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "response.output_text.delta", "delta": "first"})
		_, _, err = conn.Read(ctx)
		if err != nil {
			close(disconnected)
		}
	}))
	defer up.Close()
	p := newPlugin(func(method string, request any, result any) error {
		if method == "host.stream.emit" {
			body, _ := request.(map[string]any)["payload"].([]byte)
			if bytes.Contains(body, []byte(`"delta"`)) {
				return fmt.Errorf("synthetic downstream disconnect")
			}
		}
		return nil
	})
	p.cfg.StateDir = filepath.Join(t.TempDir(), "state")
	p.cfg.upstream = up.URL
	c := testCredential(t)
	req := executorRequest{AuthProvider: provider, Format: "codex", Model: "gpt-5.6-sol", StreamID: "cancel-test",
		Payload: []byte(`{"input":"hello"}`), StorageJSON: marshalTest(t, c)}
	if _, err := p.execute(marshalTest(t, req), true); err != nil {
		p.close()
		t.Fatal(err)
	}
	select {
	case <-disconnected:
	case <-time.After(3 * time.Second):
		p.close()
		t.Fatal("cancelled downstream leaked upstream socket")
	}
	p.close()
	if _, err := p.execute(marshalTest(t, req), true); err == nil {
		t.Fatal("accepted work after drain")
	}
}
