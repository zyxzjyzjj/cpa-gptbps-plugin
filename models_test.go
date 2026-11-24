package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func catalogFixture(id string) []byte {
	return []byte(`{"allowed":true,"model_catalog":{"default_model":` + fmt.Sprintf("%q", id) + `,"restricted_models":[],"models":[{"id":` + fmt.Sprintf("%q", id) + `,"label":"Upstream label","description":"Upstream description","efforts":[{"value":"low"},{"value":"high"}]}]}}`)
}
func catalogFixtureHost(t *testing.T, do func(map[string]any) (int, []byte)) hostCall {
	t.Helper()
	return func(method string, request any, result any) error {
		switch method {
		case "host.http.operation_open":
			return json.Unmarshal([]byte(`{"operation_id":"fixture-operation"}`), result)
		case "host.http.cancel":
			return nil
		case "host.http.do":
			req := request.(map[string]any)
			if req["url"] != bpsCatalogURL || req["method"] != "GET" {
				t.Error("discovery must query the authenticated BPS access catalog")
			}
			headers := req["headers"].(http.Header)
			if headers.Get("Authorization") == "" || headers.Get("Chatgpt-Account-Id") == "" || headers.Get("Origin") != "https://bps.openai.com" {
				t.Error("BPS credential/profile headers missing")
			}
			if req["host_callback_id"] != "callback-fixture" {
				t.Error("model RPC callback scope was not forwarded")
			}
			if req["operation_id"] != "fixture-operation" {
				t.Error("host HTTP operation not bound")
			}
			status, body := do(req)
			responseHeaders := http.Header{"Content-Type": {"application/json"}}
			if status >= 300 && status < 400 {
				responseHeaders.Set("Location", "https://untrusted.invalid/catalog")
			}
			reply, _ := json.Marshal(map[string]any{"StatusCode": status, "Headers": responseHeaders, "Body": body})
			return json.Unmarshal(reply, result)
		default:
			return fmt.Errorf("unexpected fixture callback")
		}
	}
}
func catalogRequest(t *testing.T, c credential, authID string) []byte {
	return marshalTest(t, authMaterial{AuthID: authID, AuthProvider: provider, StorageJSON: marshalTest(t, c), HostCallbackID: "callback-fixture"})
}
func returnedModels(t *testing.T, p *plugin, raw []byte) []modelInfo {
	t.Helper()
	result, err := p.models(raw)
	if err != nil {
		t.Fatal(err)
	}
	return result.(map[string]any)["Models"].([]modelInfo)
}
func expireCatalog(p *plugin, ago time.Duration) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	for key, entry := range p.catalogs {
		entry.nextFetch = time.Now().Add(-time.Second)
		if ago > 0 {
			entry.fetched = time.Now().Add(-ago)
		}
		p.catalogs[key] = entry
	}
}

func TestCatalogComesFromUpstreamAndIgnoresLegacyFixedModels(t *testing.T) {
	p := newPlugin(catalogFixtureHost(t, func(req map[string]any) (int, []byte) { return 200, catalogFixture("bps-model-from-upstream") }))
	defer p.close()
	raw := marshalTest(t, lifecycleRequest{ConfigYAML: []byte("models: [obsolete-fixed-model]\n")})
	if err := p.configure(raw); err != nil {
		t.Fatal(err)
	}
	models := returnedModels(t, p, catalogRequest(t, testCredential(t), "first"))
	if len(models) != 1 || models[0].ID != "bps-model-from-upstream" || models[0].DisplayName != "Upstream label (BPS)" || models[0].Description != "Upstream description" {
		t.Fatal("models were replaced by a static/default list")
	}
	if strings.Join(models[0].Thinking.Levels, ",") != "low,high" {
		t.Fatal("reasoning levels were invented instead of reading upstream")
	}
	models[0].Thinking.Levels[0] = "mutated"
	if returnedModels(t, p, catalogRequest(t, testCredential(t), "first"))[0].Thinking.Levels[0] != "low" {
		t.Fatal("caller mutated shared cached model metadata")
	}
}

func TestCatalogFiltersRestrictedAndDeduplicates(t *testing.T) {
	catalog, err := parseCatalog([]byte(`{"allowed":true,"model_catalog":{"default_model":"blocked","restricted_models":["blocked"],"models":[{"id":"open","efforts":[{"value":"none"},{"value":"low"},{"value":"low"},{"value":"future-level"}]},{"id":"open"},{"id":"blocked"}]}}`))
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "open" || catalog.DefaultModel != "" {
		t.Fatal("upstream model restrictions were lost")
	}
	if strings.Join(catalog.Models[0].Thinking.Levels, ",") != "low" {
		t.Fatal("advertised unsupported bridge reasoning")
	}
	empty, err := parseCatalog([]byte(`{"allowed":true,"model_catalog":{"models":[]}}`))
	if err != nil || empty.Models == nil || len(empty.Models) != 0 {
		t.Fatal("empty upstream catalog must remain empty")
	}
	for _, body := range []string{`{}`, `{"allowed":true}`, `{"allowed":true,"model_catalog":{"models":null}}`, `{"allowed":true,"model_catalog":{"models":[{"id":"bad\nid"}]}}`, `{"allowed":true,"model_catalog":{"models":[{"id":42}]}}`} {
		if _, err := parseCatalog([]byte(body)); err == nil || err.Code != "model_catalog_invalid" {
			t.Fatal("accepted invalid catalog or used a fixed fallback")
		}
	}
}

func TestCatalogCacheIsIsolatedAndRefreshesExpiredEntries(t *testing.T) {
	var count atomic.Int32
	p := newPlugin(catalogFixtureHost(t, func(map[string]any) (int, []byte) {
		return 200, catalogFixture(fmt.Sprintf("upstream-%d", count.Add(1)))
	}))
	defer p.close()
	c := testCredential(t)
	first := catalogRequest(t, c, "first")
	for i := 0; i < 3; i++ {
		if returnedModels(t, p, first)[0].ID != "upstream-1" {
			t.Fatal("cache hit changed catalog")
		}
	}
	c.AccessToken += "other-signature"
	second := catalogRequest(t, c, "first")
	if returnedModels(t, p, second)[0].ID != "upstream-2" || count.Load() != 2 {
		t.Fatal("rotated credentials reused an old grant's model catalog")
	}
	expireCatalog(p, 0)
	if returnedModels(t, p, first)[0].ID != "upstream-3" || count.Load() != 3 {
		t.Fatal("expired catalog was not fetched again")
	}
}

func TestConcurrentCatalogLoadsShareOneUpstreamRequest(t *testing.T) {
	var count atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	p := newPlugin(catalogFixtureHost(t, func(map[string]any) (int, []byte) {
		if count.Add(1) == 1 {
			close(started)
		}
		<-release
		return 200, catalogFixture("shared-upstream")
	}))
	defer p.close()
	request := catalogRequest(t, testCredential(t), "same-auth")
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models := returnedModels(t, p, request)
			if len(models) != 1 || models[0].ID != "shared-upstream" {
				t.Error("incorrect concurrent catalog")
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if count.Load() != 1 {
		t.Fatal("concurrent registrations duplicated upstream discovery")
	}
}

func TestCatalogTransientFailureUsesOnlyBoundedObservedCatalog(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var count atomic.Int32
	p := newPlugin(catalogFixtureHost(t, func(map[string]any) (int, []byte) {
		count.Add(1)
		return int(status.Load()), catalogFixture("previous-upstream")
	}))
	defer p.close()
	request := catalogRequest(t, testCredential(t), "first")
	returnedModels(t, p, request)
	expireCatalog(p, 0)
	status.Store(503)
	if returnedModels(t, p, request)[0].ID != "previous-upstream" {
		t.Fatal("lost bounded last upstream catalog")
	}
	_, snapshot := p.catalogStatus()
	if len(snapshot) != 1 || snapshot[0]["stale"] != true || snapshot[0]["last_error_code"] != "model_catalog_upstream" {
		t.Fatal("stale catalog was not disclosed")
	}
	returnedModels(t, p, request)
	if count.Load() != 2 {
		t.Fatal("failed refresh created an upstream retry storm")
	}
	expireCatalog(p, catalogMaxStale+time.Second)
	if _, err := p.models(request); err == nil {
		t.Fatal("served catalog beyond bounded stale window")
	}
}

func TestCatalogAuthFailuresClearOldCatalogAndNeverInventModels(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var replies atomic.Int32
			p := newPlugin(catalogFixtureHost(t, func(map[string]any) (int, []byte) {
				if replies.Add(1) == 1 {
					return 200, catalogFixture("previous-upstream")
				}
				return status, []byte(`{"error":"credential-shaped-private-description"}`)
			}))
			defer p.close()
			request := catalogRequest(t, testCredential(t), "first")
			returnedModels(t, p, request)
			expireCatalog(p, 0)
			result, err := p.models(request)
			if result != nil || err == nil || strings.Contains(err.Error(), "credential-shaped") {
				t.Fatal("failed auth used stale models or exposed upstream body")
			}
			ids, _ := p.catalogStatus()
			if len(ids) != 0 {
				t.Fatal("revoked catalog remained advertised in plugin status")
			}
		})
	}
	p := newPlugin(catalogFixtureHost(t, func(map[string]any) (int, []byte) {
		return 200, []byte(`{"allowed":false,"model_catalog":{"models":[{"id":"never-advertise"}]}}`)
	}))
	defer p.close()
	if result, err := p.models(catalogRequest(t, testCredential(t), "denied")); result != nil || err == nil {
		t.Fatal("BPS access denial was ignored")
	}
}

func TestCatalogShutdownCancelsHostOperation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once
	p := newPlugin(func(method string, request any, result any) error {
		switch method {
		case "host.http.operation_open":
			return json.Unmarshal([]byte(`{"operation_id":"cancel-fixture"}`), result)
		case "host.http.do":
			close(started)
			<-cancelled
			return fmt.Errorf("synthetic cancelled transport")
		case "host.http.cancel":
			once.Do(func() { close(cancelled) })
			return nil
		}
		return fmt.Errorf("unexpected callback")
	})
	request := catalogRequest(t, testCredential(t), "first")
	result := make(chan error, 1)
	go func() { _, err := p.models(request); result <- err }()
	<-started
	closed := make(chan struct{})
	go func() { p.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("plugin shutdown leaked catalog HTTP operation")
	}
	if err := <-result; err == nil {
		t.Fatal("cancelled discovery reported success")
	}
}

func TestHostOAuthTransportAcceptsBodylessRequests(t *testing.T) {
	client := http.Client{Transport: hostOAuthTransport{call: func(method string, request any, result any) error {
		if method != "host.http.do" {
			t.Fatal("unexpected HTTP callback")
		}
		raw, _ := json.Marshal(map[string]any{"StatusCode": 200, "Body": []byte(`{}`)})
		return json.Unmarshal(raw, result)
	}}}
	response, err := client.Get(bpsCatalogURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("bodyless host request failed")
	}
}

func TestCatalogDoesNotSendCredentialsToUnexpectedRedirect(t *testing.T) {
	var calls atomic.Int32
	p := newPlugin(catalogFixtureHost(t, func(map[string]any) (int, []byte) {
		calls.Add(1)
		return 302, []byte(`{"redirect":"https://untrusted.invalid"}`)
	}))
	defer p.close()
	_, err := p.models(catalogRequest(t, testCredential(t), "first"))
	if err == nil || !bytes.Contains(failure(err), []byte("model_catalog_http_error")) || calls.Load() != 1 {
		t.Fatal("accepted unexpected redirect")
	}
}

func TestHostHTTPTransportsDecodeCPANativeResponse(t *testing.T) {
	// Literal wire fixture from CPA's untagged pluginapi.HTTPResponse. Using
	// streaming callback field names here previously hid a real host failure.
	const wire = `{"StatusCode":403,"Headers":{"Content-Type":["application/json"],"X-Catalog-Fixture":["native"]},"Body":"e30="}`
	call := func(method string, request any, result any) error {
		switch method {
		case "host.http.operation_open":
			return json.Unmarshal([]byte(`{"operation_id":"native-response-fixture"}`), result)
		case "host.http.cancel":
			return nil
		case "host.http.do":
			return json.Unmarshal([]byte(wire), result)
		default:
			return fmt.Errorf("unexpected callback %s", method)
		}
	}
	for name, transport := range map[string]http.RoundTripper{
		"catalog": catalogHostTransport{call: call},
		"oauth":   hostOAuthTransport{call: call},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, bpsCatalogURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 403 || response.Header.Get("X-Catalog-Fixture") != "native" || string(body) != "{}" {
				t.Fatal("native CPA HTTP status, headers or base64 body was lost")
			}
		})
	}
}
