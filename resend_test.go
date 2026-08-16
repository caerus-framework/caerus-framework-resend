package cf_resend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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
)

func testMail(to string) Mail {
	return Mail{To: []string{to}, Subject: "t", Text: "t"}
}

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
	calls   int
	body    string
	code    int
	header  http.Header
	err     error
	bodies  []string
	codes   []int
}

func (s *stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls++
	s.lastReq = r
	if s.err != nil {
		return nil, s.err
	}
	i := s.calls - 1
	code, body := s.code, s.body
	if i < len(s.codes) {
		code = s.codes[i]
	}
	if i < len(s.bodies) {
		body = s.bodies[i]
	}
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	if s.header != nil {
		hdr = s.header.Clone()
	}
	return &http.Response{
		StatusCode: code,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
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

	id, err := r.Send(context.Background(), Mail{
		To:      []string{"user@example.com"},
		Subject: "hi",
		HTML:    "<p>hi</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "email_1" {
		t.Fatalf("id = %q, want email_1", id)
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

	m := Mail{
		From: "override@x.io",
		To:   []string{"user@example.com"},
		Text: "hi",
	}
	if _, err := r.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if m.From != "override@x.io" {
		t.Fatal("caller's Mail should not be mutated")
	}
}

func TestSendIdempotencyKey(t *testing.T) {
	stub := &stubRoundTripper{body: `{"id":"email_1"}`, code: 200}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	m := testMail("a@x.io")
	m.IdempotencyKey = "req-1"
	if _, err := r.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if stub.lastReq.Header.Get("Idempotency-Key") != "req-1" {
		t.Fatalf("Idempotency-Key = %q", stub.lastReq.Header.Get("Idempotency-Key"))
	}
}

func TestSendErrors(t *testing.T) {
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"))
	ctx := context.Background()

	if _, err := r.Send(ctx, testMail("a@x.io")); err == nil {
		t.Fatal("Send before Init should fail")
	}
	if err := r.Init(ctx, cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(ctx) })

	if _, err := r.Send(ctx, Mail{}); err == nil {
		t.Fatal("empty Mail should fail")
	} else if HTTPStatus(err) != 0 {
		t.Fatalf("validation error should not look like HTTP: %v", err)
	}
	noFrom := New(WithAPIKey("re_key"))
	if err := noFrom.Init(ctx, cf.New()); err != nil {
		t.Fatalf("Init(noFrom): %v", err)
	}
	t.Cleanup(func() { _ = noFrom.Shutdown(ctx) })
	if _, err := noFrom.Send(ctx, testMail("a@x.io")); err == nil {
		t.Fatal("Send with no from_address and no Mail.From should fail")
	}
	if _, err := r.Send(ctx, Mail{From: "not-an-email", To: []string{"a@x.io"}, Text: "hi"}); err == nil {
		t.Fatal("invalid Mail.From should fail")
	}
	if _, err := r.Send(ctx, Mail{From: "a@x.io", To: []string{"not-an-email"}, Text: "hi"}); err == nil {
		t.Fatal("invalid Mail.To should fail")
	}
	if _, err := r.Send(ctx, Mail{From: "a@x.io", Text: "hi"}); err == nil {
		t.Fatal("Send with no To should fail")
	}
	if _, err := r.Send(ctx, Mail{From: "a@x.io", To: []string{"a@x.io"}, Subject: "x"}); err == nil {
		t.Fatal("Send with no HTML or Text should fail")
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
		if _, err := r.Send(context.Background(), testMail("a@x.io")); err != nil {
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
	stub := &stubRoundTripper{
		body:   `{"message":"rate limited"}`,
		code:   429,
		header: http.Header{"Retry-After": []string{"0"}},
	}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	if _, err := r.Send(context.Background(), testMail("a@x.io")); err == nil {
		t.Fatal("Send should fail on 429")
	} else if HTTPStatus(err) != 429 {
		t.Fatalf("HTTPStatus = %d, want 429 (%v)", HTTPStatus(err), err)
	}
	var se *SendError
	if _, err := r.Send(context.Background(), testMail("b@x.io")); err == nil {
		t.Fatal("Send should fail on 429")
	} else if !errors.As(err, &se) || se.HTTPStatus() != 429 {
		t.Fatalf("errors.As SendError: %v", err)
	}

	base := map[string]string{"from": "noreply@x.io", "component": "resend"}
	if m := findMetric(t, r.Metrics(), "resend_emails_sent_total", base); m == nil || m.Value != 0 {
		t.Fatalf("sent = %+v, want value 0 (counter emitted zero until first fire)", m)
	}
	rateLimit := map[string]string{"from": "noreply@x.io", "component": "resend", "error_code": "429"}
	if m := findMetric(t, r.Metrics(), "resend_emails_failed_total", rateLimit); m == nil || m.Value != 4 {
		t.Fatalf("failed 429 = %+v, want value 4 (two Send calls × two attempts)", m)
	}
	if m := findMetric(t, r.Metrics(), "resend_send_retries_total", base); m == nil || m.Value != 2 {
		t.Fatalf("retries = %+v, want value 2", m)
	}
	if stub.calls != 4 {
		t.Fatalf("HTTP calls = %d, want 4", stub.calls)
	}
}

func TestSendRetries429ThenOK(t *testing.T) {
	stub := &stubRoundTripper{
		codes:  []int{429, 200},
		bodies: []string{`{"message":"rate limited"}`, `{"id":"email_2"}`},
		header: http.Header{"Retry-After": []string{"0"}},
	}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	id, err := r.Send(context.Background(), testMail("a@x.io"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "email_2" {
		t.Fatalf("id = %q", id)
	}
	if stub.calls != 2 {
		t.Fatalf("calls = %d, want 2", stub.calls)
	}
	base := map[string]string{"from": "noreply@x.io", "component": "resend"}
	if m := findMetric(t, r.Metrics(), "resend_send_retries_total", base); m == nil || m.Value != 1 {
		t.Fatalf("retries = %+v, want 1", m)
	}
}

func TestSendDoesNotRetry422(t *testing.T) {
	stub := &stubRoundTripper{body: `{"message":"invalid"}`, code: 422}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Send(context.Background(), testMail("a@x.io")); err == nil {
		t.Fatal("expected 422")
	} else if HTTPStatus(err) != 422 {
		t.Fatalf("HTTPStatus = %d", HTTPStatus(err))
	}
	if stub.calls != 1 {
		t.Fatalf("422 must not retry, calls = %d", stub.calls)
	}
}

func TestSendRetryHonorsCancel(t *testing.T) {
	inner := &stubRoundTripper{
		body:   `{"message":"rate limited"}`,
		code:   429,
		header: http.Header{"Retry-After": []string{"30"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stub := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := inner.RoundTrip(r)
		cancel()
		return resp, err
	})
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	if _, err := r.Send(ctx, testMail("a@x.io")); err == nil {
		t.Fatal("expected error")
	} else if HTTPStatus(err) != 429 {
		t.Fatalf("HTTPStatus = %d (cancelled wait should keep first error)", HTTPStatus(err))
	}
	if inner.calls != 1 {
		t.Fatalf("cancelled ctx must not retry HTTP, calls = %d", inner.calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSendNetworkErrorCounter(t *testing.T) {
	stub := &stubRoundTripper{err: errors.New("connection refused")}
	r := New(WithAPIKey("re_key"), WithFromAddress("noreply@x.io"),
		WithHTTPClient(&http.Client{Transport: stub}))
	if err := r.Init(context.Background(), cf.New()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })

	if _, err := r.Send(context.Background(), testMail("a@x.io")); err == nil {
		t.Fatal("Send should fail on network error")
	} else if HTTPStatus(err) != 0 {
		t.Fatalf("HTTPStatus = %d, want 0 for network", HTTPStatus(err))
	}

	network := map[string]string{"from": "noreply@x.io", "component": "resend", "error_code": "network"}
	if m := findMetric(t, r.Metrics(), "resend_emails_failed_total", network); m == nil || m.Value != 1 {
		t.Fatalf("failed network = %+v, want value 1", m)
	}
	if stub.calls != 1 {
		t.Fatalf("network errors must not retry, calls = %d", stub.calls)
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

	if _, err := r.Send(context.Background(), testMail("a@x.io")); err != nil {
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

	if _, err := r.Send(context.Background(), testMail("a@x.io")); err != nil {
		t.Fatalf("Send after reload: %v", err)
	}

	// Traffic is attributed to the actual sender at send time: the first send
	// used the pre-reload default, the second the post-reload default.
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
		if _, err := r.Send(context.Background(), testMail("a@x.io")); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if _, err := r.Send(context.Background(), Mail{From: "brand@x.io", To: []string{"b@x.io"}, Text: "t"}); err != nil {
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
	if m := findMetric(t, r.Metrics(), "resend_send_retries_total", base); m == nil || m.Value != 0 {
		t.Fatalf("retries placeholder = %+v, want value 0", m)
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

func TestResendConfigLogArgsNeverCleartext(t *testing.T) {
	cfg := ResendConfig{APIKey: "re_live_s3cret", FromAddress: "noreply@example.com"}
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	l.Info("summary", cf_configuration.LogArgs(cfg)...)
	out := buf.String()
	if strings.Contains(out, "re_live_s3cret") {
		t.Fatalf("api_key leaked: %s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("want [redacted] in %s", out)
	}
	if !strings.Contains(out, "from_address=noreply@example.com") {
		t.Fatalf("from_address should stay visible: %s", out)
	}
}

func addComponent(t *testing.T, fw *cf.CaerusFramework, c cf.CaerusComponent) {
	t.Helper()
	if err := fw.AddComponent(c); err != nil {
		t.Fatalf("AddComponent: %v", err)
	}
}
