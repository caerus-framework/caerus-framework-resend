package cf_resend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	cf_observability "github.com/caerus-framework/caerus-framework-observability"
	"github.com/resend/resend-go/v2"
)

func TestComponentContract(t *testing.T) {
	r := New()
	if r.Name() != ComponentName {
		t.Fatalf("Name() = %q, want %q", r.Name(), ComponentName)
	}
	if r.GetInitOrderStage() != ComponentStage {
		t.Fatalf("GetInitOrderStage() = %q, want %q", r.GetInitOrderStage(), ComponentStage)
	}
	var _ cf.CaerusComponent = r

	if c := r.Client(); c != nil {
		t.Fatal("Client() should be nil before Init")
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Init: %v", err)
	}
}

func TestHealthAndMetricsBeforeInit(t *testing.T) {
	r := New()
	if err := r.Health(context.Background()); err == nil {
		t.Fatal("Health before Init should fail")
	}
	if ms := r.Metrics(); ms != nil {
		t.Fatalf("Metrics before Init = %+v, want nil", ms)
	}
	var _ cf.HealthProvider = r
	var _ cf_observability.MetricsProvider = r
}

func TestNewDefaults(t *testing.T) {
	r := New()
	if r.timeout != 10*time.Second {
		t.Fatalf("default timeout = %v, want 10s", r.timeout)
	}
	if r.name != "" {
		t.Fatalf("default name = %q, want empty", r.name)
	}
}

func TestNewWithName(t *testing.T) {
	r := New(WithName("mailer"))
	if r.Name() != "mailer" {
		t.Fatalf("Name() = %q, want mailer", r.Name())
	}
}

func TestWithConfigOverridesOptions(t *testing.T) {
	r := New(
		WithAPIKey("opt-key"),
		WithFromAddress("opt@x.io"),
		WithConfig(ResendConfig{APIKey: "cfg-key", FromAddress: "cfg@x.io"}),
	)
	if r.apiKey != "cfg-key" {
		t.Fatalf("apiKey = %q, want cfg-key", r.apiKey)
	}
	if r.from != "cfg@x.io" {
		t.Fatalf("from = %q, want cfg@x.io", r.from)
	}
}

func TestInitRequiresAPIKey(t *testing.T) {
	r := New(WithFromAddress("a@x.io"))
	if err := r.Init(context.Background(), cf.New()); err == nil {
		t.Fatal("Init without an API key should fail")
	}
}

func TestInitBuildsClient(t *testing.T) {
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if c := r.Client(); c == nil {
		t.Fatal("Client() should be non-nil after Init")
	} else if c.ApiKey != "re_key" {
		t.Fatalf("client ApiKey = %q, want re_key", c.ApiKey)
	}
	if r.From() != "noreply@x.io" {
		t.Fatalf("From() = %q, want noreply@x.io", r.From())
	}
}

func TestInitTwiceIsIdempotent(t *testing.T) {
	r := New(WithAPIKey("re_key"))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("second Init: %v", err)
	}
}

// stubRoundTripper captures the last request and returns a canned response.
type stubRoundTripper struct {
	lastReq *http.Request
	body    string
	code    int
	err     error
}

func (s *stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.lastReq = r
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.code,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
	}, nil
}

func TestSendUsesConfiguredFrom(t *testing.T) {
	stub := &stubRoundTripper{body: `{"id":"email_1"}`, code: 200}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	resp, err := r.Send(context.Background(), &resend.SendEmailRequest{
		To:      []string{"user@example.com"},
		Subject: "hi",
		Html:    "<p>hi</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp == nil || resp.Id != "email_1" {
		t.Fatalf("resp = %+v, want id email_1", resp)
	}
	if stub.lastReq == nil {
		t.Fatal("no request captured")
	}
	auth := stub.lastReq.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer re_key") {
		t.Fatalf("Authorization = %q, want Bearer re_key", auth)
	}
	if !strings.HasSuffix(stub.lastReq.URL.Path, "/emails") {
		t.Fatalf("path = %q, want /emails", stub.lastReq.URL.Path)
	}
}

func TestSendRequestFromWins(t *testing.T) {
	stub := &stubRoundTripper{body: `{"id":"email_1"}`, code: 200}
	r := New(WithAPIKey("re_key"), WithFromAddress("default@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	req := &resend.SendEmailRequest{
		From: "override@x.io",
		To:   []string{"user@example.com"},
	}
	if _, err := r.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if req.From != "override@x.io" {
		t.Fatal("caller's request should not be mutated")
	}
}

func TestSendErrors(t *testing.T) {
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"))
	ctx := context.Background()

	if _, err := r.Send(ctx, &resend.SendEmailRequest{To: []string{"a@x.io"}}); err == nil {
		t.Fatal("Send before Init should fail")
	}
	if err := r.Init(ctx, cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(ctx) })

	if _, err := r.Send(ctx, nil); err == nil {
		t.Fatal("nil request should fail")
	}
	noFrom := New(WithAPIKey("re_key"))
	if err := noFrom.Init(ctx, cf.New()); err != nil {
		t.Fatalf("Init(noFrom): %v", err)
	}
	t.Cleanup(func() { _ = noFrom.Shutdown(ctx) })
	if _, err := noFrom.Send(ctx, &resend.SendEmailRequest{To: []string{"a@x.io"}}); err == nil {
		t.Fatal("Send with no configured from and no req.From should fail")
	}
	if _, err := r.Send(ctx, &resend.SendEmailRequest{From: "a@x.io"}); err == nil {
		t.Fatal("Send with no To should fail")
	}
}

func TestShutdownClearsClient(t *testing.T) {
	r := New(WithAPIKey("re_key"))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if r.Client() == nil {
		t.Fatal("client should be non-nil after Init")
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if r.Client() != nil {
		t.Fatal("client should be nil after Shutdown")
	}
	if err := r.Health(context.Background()); err == nil {
		t.Fatal("Health after Shutdown should fail")
	}
}

func TestMetricsAfterInit(t *testing.T) {
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"), WithBaseURL("https://api.test.resend.com"))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	ms := r.Metrics()
	if ms == nil {
		t.Fatal("Metrics after Init should not be nil")
	}
	found := false
	for _, m := range ms {
		if m.Name == "resend_info" {
			found = true
			if m.Labels["from"] != "noreply@x.io" {
				t.Fatalf("resend_info from label = %q", m.Labels["from"])
			}
			if m.Labels["base_url"] != "https://api.test.resend.com" {
				t.Fatalf("resend_info base_url label = %q", m.Labels["base_url"])
			}
			if m.Value != 1 {
				t.Fatalf("resend_info value = %v, want 1", m.Value)
			}
		}
	}
	if !found {
		t.Fatal("resend_info metric missing")
	}
}

// findMetric returns the metric sample with the given name whose labels match,
// or nil.
func findMetric(t *testing.T, ms []cf_observability.Metric, name string, labels map[string]string) *cf_observability.Metric {
	t.Helper()
	for i := range ms {
		m := &ms[i]
		if m.Name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if m.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return m
		}
	}
	return nil
}

func TestSendTrafficCounters(t *testing.T) {
	stub := &stubRoundTripper{body: `{"id":"email_1"}`, code: 200}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	for i := 0; i < 3; i++ {
		if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"a@x.io"}}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	base := map[string]string{"from": "noreply@x.io", "component": "resend"}
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", base); m == nil || m.Value != 3 {
		t.Fatalf("sent = %+v, want value 3", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_emails_failed_total", base); m != nil {
		t.Fatalf("failed = %+v, want absent", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_send_duration_seconds_count", base); m == nil || m.Value != 3 {
		t.Fatalf("duration count = %+v, want value 3", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_send_duration_seconds_sum", base); m == nil || m.Value <= 0 {
		t.Fatalf("duration sum = %+v, want > 0", m)
	}
}

func TestSendFailureCounters(t *testing.T) {
	stub := &stubRoundTripper{body: `{"message":"rate limited"}`, code: 429}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"a@x.io"}}); err == nil {
		t.Fatal("Send should fail on 429")
	}
	if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"b@x.io"}}); err == nil {
		t.Fatal("Send should fail on 429")
	}

	base := map[string]string{"from": "noreply@x.io", "component": "resend"}
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", base); m == nil || m.Value != 0 {
		t.Fatalf("sent = %+v, want value 0 (counter emitted zero until first fire)", m)
	}
	rateLimit := map[string]string{"from": "noreply@x.io", "component": "resend", "error_code": "429"}
	if m := findMetric(t, r.Metrics(), "resend_emails_failed_total", rateLimit); m == nil || m.Value != 2 {
		t.Fatalf("failed 429 = %+v, want value 2", m)
	}
}

func TestSendNetworkErrorCounter(t *testing.T) {
	stub := &stubRoundTripper{err: errors.New("connection refused")}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"a@x.io"}}); err == nil {
		t.Fatal("Send should fail on network error")
	}

	network := map[string]string{"from": "noreply@x.io", "component": "resend", "error_code": "network"}
	if m := findMetric(t, r.Metrics(), "resend_emails_failed_total", network); m == nil || m.Value != 1 {
		t.Fatalf("failed network = %+v, want value 1", m)
	}
}

func TestSendCountersSurviveReload(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "resend.json", `{"api_key":"re_k1","from_address":"a@x.io"}`)

	fw := cf.New()
	addComponent(t, fw, cf_logs.New(cf_logs.WithWriter(io.Discard)))
	addComponent(t, fw, cf_configuration.New())
	r := New(WithConfigSource("resend", path),
		WithHTTPClient(&http.Client{Transport: &stubRoundTripper{body: `{"id":"email_1"}`, code: 200}}))
	addComponent(t, fw, r)
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = fw.Shutdown(context.Background()) })

	if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"a@x.io"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	writeConfig(t, dir, "resend.json", `{"api_key":"re_k2","from_address":"b@x.io"}`)
	conf, ok := cf.Get[*cf_configuration.Configuration](fw)
	if !ok {
		t.Fatal("configuration component not found")
	}
	if err := conf.Reload("resend"); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"a@x.io"}}); err != nil {
		t.Fatalf("Send after reload: %v", err)
	}

	// Traffic is attributed to the actual sender at send time: the first send
	// used the pre-reload default, the second the post-reload default. No
	// history is relabeled.
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", map[string]string{"component": "resend", "from": "a@x.io"}); m == nil || m.Value != 1 {
		t.Fatalf("sent a@x.io = %+v, want value 1", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", map[string]string{"component": "resend", "from": "b@x.io"}); m == nil || m.Value != 1 {
		t.Fatalf("sent b@x.io = %+v, want value 1", m)
	}
}

func TestSendPerSenderBucketing(t *testing.T) {
	stub := &stubRoundTripper{body: `{"id":"email_1"}`, code: 200}
	r := New(WithAPIKey("re_key"), WithFromAddress("default@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	// Two sends from the default sender, one overriding From.
	for i := 0; i < 2; i++ {
		if _, err := r.Send(context.Background(), &resend.SendEmailRequest{To: []string{"a@x.io"}}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if _, err := r.Send(context.Background(), &resend.SendEmailRequest{From: "brand@x.io", To: []string{"b@x.io"}}); err != nil {
		t.Fatalf("Send brand: %v", err)
	}

	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", map[string]string{"component": "resend", "from": "default@x.io"}); m == nil || m.Value != 2 {
		t.Fatalf("sent default@x.io = %+v, want value 2", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", map[string]string{"component": "resend", "from": "brand@x.io"}); m == nil || m.Value != 1 {
		t.Fatalf("sent brand@x.io = %+v, want value 1", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_emails_failed_total", map[string]string{"component": "resend", "from": "brand@x.io"}); m != nil {
		t.Fatalf("brand failed = %+v, want absent", m)
	}
}

func TestSendMetricsPlaceholderBeforeTraffic(t *testing.T) {
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	base := map[string]string{"from": "noreply@x.io", "component": "resend"}
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", base); m == nil || m.Value != 0 {
		t.Fatalf("sent placeholder = %+v, want value 0", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_send_duration_seconds_count", base); m == nil || m.Value != 0 {
		t.Fatalf("duration count placeholder = %+v, want value 0", m)
	}
}

func TestWithConfigSourceDeclaresConfigurationDependency(t *testing.T) {
	r := New(WithConfigSource("resend", ""))
	deps := r.GetDependencies()
	found := false
	for _, d := range deps {
		if d == cf_configuration.ComponentName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("deps = %v, want %q", deps, cf_configuration.ComponentName)
	}
}

func TestOnConfigReloadNoopWithoutSource(t *testing.T) {
	r := New()
	r.OnConfigReload("resend", nil)
}

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestReloadUpdatesClient(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "resend.json", `{"api_key":"re_k1","from_address":"a@x.io"}`)

	fw := cf.New()
	addComponent(t, fw, cf_logs.New(cf_logs.WithWriter(io.Discard)))
	addComponent(t, fw, cf_configuration.New())
	r := New(WithConfigSource("resend", path))
	addComponent(t, fw, r)
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = fw.Shutdown(context.Background()) })

	if c := r.Client(); c == nil || c.ApiKey != "re_k1" {
		t.Fatalf("client after Init = %+v, want key re_k1", c)
	}
	if r.From() != "a@x.io" {
		t.Fatalf("From() = %q, want a@x.io", r.From())
	}

	writeConfig(t, dir, "resend.json", `{"api_key":"re_k2","from_address":"b@x.io"}`)
	conf, ok := cf.Get[*cf_configuration.Configuration](fw)
	if !ok {
		t.Fatal("configuration component not found")
	}
	if err := conf.Reload("resend"); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if c := r.Client(); c == nil || c.ApiKey != "re_k2" {
		t.Fatalf("client after reload = %+v, want key re_k2", c)
	}
	if r.From() != "b@x.io" {
		t.Fatalf("From() after reload = %q, want b@x.io", r.From())
	}
	if n := r.reloads.Load(); n != 1 {
		t.Fatalf("reloads = %d, want 1", n)
	}
	if m := findMetric(t, r.Metrics(), "resend_config_reloads_total", map[string]string{"component": "resend"}); m == nil || m.Value != 1 {
		t.Fatalf("config reloads metric = %+v, want value 1", m)
	}
}

func TestReloadLastGoodKeepsPreviousClient(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "resend.json", `{"api_key":"re_k1","from_address":"a@x.io"}`)

	fw := cf.New()
	addComponent(t, fw, cf_logs.New(cf_logs.WithWriter(io.Discard)))
	addComponent(t, fw, cf_configuration.New())
	r := New(WithConfigSource("resend", path))
	addComponent(t, fw, r)
	if err := fw.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { _ = fw.Shutdown(context.Background()) })

	// Invalid base_url: applyConfigFromSource succeeds but buildClient fails,
	// so the previous client and settings must stay in place.
	writeConfig(t, dir, "resend.json", `{"base_url":"://bad"}`)
	conf, ok := cf.Get[*cf_configuration.Configuration](fw)
	if !ok {
		t.Fatal("configuration component not found")
	}
	if err := conf.Reload("resend"); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if c := r.Client(); c == nil || c.ApiKey != "re_k1" {
		t.Fatalf("client after failed reload = %+v, want key re_k1", c)
	}
	if r.From() != "a@x.io" {
		t.Fatalf("From() after failed reload = %q, want a@x.io", r.From())
	}
	if n := r.reloads.Load(); n != 0 {
		t.Fatalf("reloads = %d, want 0", n)
	}
}

func addComponent(t *testing.T, fw *cf.CaerusFramework, c cf.CaerusComponent) {
	t.Helper()
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}
}
