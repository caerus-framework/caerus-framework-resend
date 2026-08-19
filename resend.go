package cf_resend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	cf_observability "github.com/caerus-framework/caerus-framework-observability"
	"github.com/resend/resend-go/v2"
)

const (
	// ComponentName is the framework component name for the resend component.
	// It is the identifier other components use in GetDependencies to require
	// resend.
	ComponentName = "resend"

	// ComponentStage is the stage data-layer components initialize in. It is
	// not a built-in bootstrap stage; AddComponent registers it automatically
	// the first time a component declares it.
	ComponentStage = cf.Stage("data")
)

// ResendConfig is the file/env-drivable configuration. Load it through the
// configuration component (caerus-framework-configuration) and pass it via
// WithConfigSource; both JSON and YAML tags are provided.
type ResendConfig struct {
	// APIKey is the Resend API key (resend.com, or a self-hosted instance).
	APIKey string `json:"api_key" yaml:"api_key" env:"API_KEY" secret:"redact"`
	// FromAddress is the soft-default sender (e.g. "noreply@example.com"
	// or `Name <noreply@example.com>`). Send uses it when Mail.From is empty.
	FromAddress string `json:"from_address" yaml:"from_address" env:"FROM_ADDRESS"`
	// BaseURL overrides the Resend API endpoint. Empty uses the SDK default.
	// Useful for self-hosted instances or tests that stub the API.
	BaseURL string `json:"base_url,omitempty" yaml:"base_url,omitempty" env:"BASE_URL"`
	// TimeoutSec bounds each Resend HTTP call (default 10s).
	TimeoutSec float64 `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty" env:"TIMEOUT_SEC"`
}

// Option configures the resend component at construction time.
type Option func(*options)

type options struct {
	loaded       *ResendConfig // set by WithConfig; overrides option-set defaults
	configSource string        // named configuration source for live reload
	configPath   string        // source file path (module self-registration)
	srcEnvPrefix string        // source env overlay prefix (default: NAME_)
	srcFormat    cf_configuration.Format
	srcFormatSet bool
	apiKey       string
	fromAddress  string
	baseURL      string
	timeout      time.Duration
	httpClient   *http.Client
	logger       *slog.Logger
	loggerSet    bool   // true when WithLogger was called explicitly
	name         string // custom component name; empty means use ComponentName
}

// SourceOption configures the self-registered configuration source created by
// WithConfigSource.
type SourceOption func(*sourceOptions)

type sourceOptions struct {
	envPrefix string
	format    cf_configuration.Format
	formatSet bool
}

// WithSourceEnvPrefix sets the environment overlay prefix for the source
// (default: the uppercase source name with "-" replaced by "_", plus "_").
// An empty prefix disables env overlay.
func WithSourceEnvPrefix(prefix string) SourceOption {
	return func(o *sourceOptions) { o.envPrefix = prefix }
}

// WithSourceFormat forces the file format instead of inferring it from the
// path extension (".yaml"/".yml" → YAML; anything else JSON).
func WithSourceFormat(f cf_configuration.Format) SourceOption {
	return func(o *sourceOptions) { o.format = f; o.formatSet = true }
}

// defaultSourceEnvPrefix derives an environment prefix from a source name.
func defaultSourceEnvPrefix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_"
}

// WithConfig sets a static configuration snapshot. Non-zero fields of cfg
// override the values set by the convenience options. Prefer WithConfigSource
// when using caerus-framework-configuration with hot-reload.
func WithConfig(cfg ResendConfig) Option {
	return func(o *options) { o.loaded = &cfg }
}

// WithConfigSource binds this component to a named configuration source and
// registers that source with the configuration component (via the framework's
// ConfigSourceRegistrar pass during argv absorption). The module owns the
// Source: the config type, the default EnvPrefix and its Owner (Name(), so
// named instances reload correctly). main only points the instance at where
// the config lives.
//
//	cf_resend.New(cf_resend.WithConfigSource("resend", "config/resend.json"))
//	cf_resend.New(cf_resend.WithConfigSource("mailer", "/etc/app/mailer.yaml",
//	    cf_resend.WithSourceFormat(cf_configuration.FormatYAML)))
//
// A path of "" registers an env-only (fileless) source when the EnvPrefix is
// non-empty. The path CLI override stays --<source-name> (ParseFlags).
// Declares a dependency on "configuration".
func WithConfigSource(name, path string, opts ...SourceOption) Option {
	return func(o *options) {
		so := sourceOptions{envPrefix: defaultSourceEnvPrefix(name)}
		for _, opt := range opts {
			opt(&so)
		}
		o.configSource = name
		o.configPath = path
		o.srcEnvPrefix = so.envPrefix
		o.srcFormat = so.format
		o.srcFormatSet = so.formatSet
	}
}

// WithAPIKey sets the Resend API key directly (tests, embedded use). Prefer
// WithConfigSource for production so the key rotates via config reload.
func WithAPIKey(apiKey string) Option {
	return func(o *options) { o.apiKey = apiKey }
}

// WithFromAddress sets the soft-default sender. Send uses it when Mail.From
// is empty. A non-empty Mail.From overrides this call only. Both empty, or
// a value that does not parse as an email address, is an error.
func WithFromAddress(from string) Option {
	return func(o *options) { o.fromAddress = from }
}

// WithBaseURL overrides the Resend API endpoint. Empty uses the SDK default.
func WithBaseURL(baseURL string) Option {
	return func(o *options) { o.baseURL = baseURL }
}

// WithTimeout sets the per-send HTTP timeout (default 10s).
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithHTTPClient overrides the HTTP client used for Resend calls. Useful for
// tests (a stub RoundTripper) and for sharing a process-wide transport. The
// component does not close a client it did not create.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) { o.httpClient = hc }
}

// WithLogger overrides the logger used for component diagnostics. By default
// the component logs through the framework logs component (declared in
// GetDependencies); WithLogger is an explicit override for tests and embedded
// use and wins over the framework logger. slog.Default() remains the fallback
// only when neither is available.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger; o.loggerSet = true }
}

// WithName sets a custom component name, allowing multiple resend instances in
// the same process. The default name is "resend" (ComponentName). Use this when
// you need several mail senders (e.g. branded vs system) in one binary.
// Retrieve named instances with GetByName[*CFResend](fw, "mailer").
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

// CFResend is the caerus-framework-resend component. It wraps the Resend SDK
// client and hands out live accessors (Client, From) to peers.
type CFResend struct {
	mu           sync.RWMutex
	configSource string
	configPath   string
	srcEnvPrefix string
	srcFormat    cf_configuration.Format
	srcFormatSet bool
	apiKey       string // base key from options/config (kept across reloads)
	from         string
	baseURL      string
	timeout      time.Duration
	httpClient   *http.Client
	loggerSet    bool
	client       *resend.Client
	logger       *slog.Logger
	logsSub      *cf_logs.Subscription
	fw           *cf.CaerusFramework
	name         string // custom name; empty means use ComponentName
	meter        *sendMeter
	reloads      atomic.Uint64
}

// New creates a resend component. The client is built at Init, not here.
func New(opts ...Option) *CFResend {
	o := options{
		logger:  slog.Default(),
		timeout: 10 * time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}
	c := &CFResend{
		configSource: o.configSource,
		configPath:   o.configPath,
		srcEnvPrefix: o.srcEnvPrefix,
		srcFormat:    o.srcFormat,
		srcFormatSet: o.srcFormatSet,
		apiKey:       o.apiKey,
		from:         o.fromAddress,
		baseURL:      o.baseURL,
		timeout:      o.timeout,
		httpClient:   o.httpClient,
		logger:       o.logger,
		loggerSet:    o.loggerSet,
		name:         o.name,
		meter:        newSendMeter(),
	}
	if o.loaded != nil {
		c.applyConfig(*o.loaded)
	}
	return c
}

// applyConfig overlays non-zero fields of cfg onto the component's base
// settings. It runs last, so a loaded config always wins over option-set
// defaults.
func (c *CFResend) applyConfig(cfg ResendConfig) {
	if cfg.APIKey != "" {
		c.apiKey = cfg.APIKey
	}
	if cfg.FromAddress != "" {
		c.from = cfg.FromAddress
	}
	if cfg.BaseURL != "" {
		c.baseURL = cfg.BaseURL
	}
	if cfg.TimeoutSec > 0 {
		c.timeout = time.Duration(cfg.TimeoutSec * float64(time.Second))
	}
}

// Name implements cf.CaerusComponent. Returns the custom name set via WithName,
// or the default ComponentName ("resend") if no custom name was set.
func (c *CFResend) Name() string {
	if c.name != "" {
		return c.name
	}
	return ComponentName
}

// GetInitOrderStage implements cf.CaerusComponent.
func (c *CFResend) GetInitOrderStage() cf.Stage { return ComponentStage }

// GetDependencies implements cf.Dependencies. The component logs through the
// framework logs component, and depends on configuration when WithConfigSource
// is set.
func (c *CFResend) GetDependencies() []string {
	deps := []string{cf_logs.ComponentName}
	if c.configSource != "" {
		deps = append(deps, cf_configuration.ComponentName)
	}
	return deps
}

// Init implements cf.CaerusComponent. It builds the Resend client from the
// option-set or configuration-source credentials. An empty API key fails
// startup (fail-fast) so a misconfigured mailer never silently swallows sends.
func (c *CFResend) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return nil // already initialized
	}
	c.fw = fw
	if !c.loggerSet {
		if logs, ok := cf.Get[*cf_logs.Logs](fw); ok {
			c.logsSub = logs.OnReconfigureFor(c.Name(), func(l *slog.Logger) { c.logger = l })
		}
	}

	if c.configSource != "" {
		if err := c.applyConfigFromSource(); err != nil {
			return err
		}
	}

	if c.from == "" {
		c.logger.Warn("cf_resend: no from_address configured; Send must set Mail.From")
	}

	client, err := c.buildClient()
	if err != nil {
		return err
	}
	c.client = client
	c.logger.Info("cf_resend: initialized",
		"from", c.from,
		cf_logs.SecretSet("api_key", c.apiKey),
	)
	return nil
}

// applyConfigFromSource reloads the bound configuration source and overlays it
// onto the component's base settings. It must be called with the mutex held.
func (c *CFResend) applyConfigFromSource() error {
	conf, ok := cf.Get[*cf_configuration.Configuration](c.fw)
	if !ok {
		return errors.New("cf_resend: configuration component not registered")
	}
	loaded, ok := cf_configuration.Get[ResendConfig](conf, c.configSource)
	if !ok {
		return fmt.Errorf("cf_resend: configuration source %q not found", c.configSource)
	}
	c.applyConfig(loaded)
	return nil
}

// buildClient constructs a Resend client from the current settings. It must be
// called with the mutex held. Every request transport is routed through the
// component's send meter, so send traffic and error codes are recorded no
// matter which transport is in use (component-owned or WithHTTPClient).
func (c *CFResend) buildClient() (*resend.Client, error) {
	if c.apiKey == "" {
		return nil, errors.New("cf_resend: no API key configured (WithAPIKey / RESEND_API_KEY / config file)")
	}
	hc := c.httpClient
	timeout := c.timeout
	var transport http.RoundTripper
	if hc != nil {
		transport = hc.Transport
		if hc.Timeout > 0 {
			timeout = hc.Timeout
		}
	}
	client := resend.NewCustomClient(&http.Client{
		Transport: &meterTransport{base: transport, meter: c.meter},
		Timeout:   timeout,
	}, c.apiKey)
	if c.baseURL != "" {
		parsed, err := url.Parse(c.baseURL)
		if err != nil {
			return nil, fmt.Errorf("cf_resend: invalid base_url %q: %w", c.baseURL, err)
		}
		client.BaseURL = parsed
	}
	return client, nil
}

// fromCtxKey is the request-context key carrying the resolved sender address so
// the transport can attribute each send to its actual From.
type fromCtxKey struct{}

// unknownFrom is the attribution bucket for sends that bypass Send() and call
// the SDK client directly (the context then carries no from).
const unknownFrom = "unknown"

// sendMeter tallies Resend send outcomes and latency per sender at the
// transport layer. The resend SDK does not surface status codes in its errors,
// so the transport is the only reliable source for error-code labels. Counters
// are cumulative and survive client rebuilds on config reload.
type sendMeter struct {
	mu      sync.Mutex
	senders map[string]*senderStats // from -> stats
}

// senderStats accumulates send traffic for one sender address.
type senderStats struct {
	sent          uint64
	failed        map[string]uint64 // error_code -> count
	durationSum   float64           // total send latency in seconds
	durationCount uint64
	retries       uint64
}

func newSendMeter() *sendMeter {
	return &sendMeter{senders: make(map[string]*senderStats)}
}

func (m *sendMeter) addRetry(from string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.senders[from]
	if s == nil {
		s = &senderStats{failed: make(map[string]uint64)}
		m.senders[from] = s
	}
	s.retries++
}

// snapshot returns a point-in-time copy of the meter for Metrics.
func (m *sendMeter) snapshot() map[string]senderStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]senderStats, len(m.senders))
	for from, s := range m.senders {
		failed := make(map[string]uint64, len(s.failed))
		for code, n := range s.failed {
			failed[code] = n
		}
		out[from] = senderStats{sent: s.sent, failed: failed, durationSum: s.durationSum, durationCount: s.durationCount, retries: s.retries}
	}
	return out
}

// meterTransport wraps a base RoundTripper and records outcomes per sender:
// HTTP 2xx is a sent email, any other response status is a failure keyed by
// the status code, and a transport error is a failure keyed "network". Every
// attempt also accumulates latency for the send-duration series.
type meterTransport struct {
	base  http.RoundTripper
	meter *sendMeter
}

func (t *meterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	from, _ := req.Context().Value(fromCtxKey{}).(string)
	if from == "" {
		from = unknownFrom
	}
	start := time.Now()
	resp, err := base.RoundTrip(req)
	d := time.Since(start).Seconds()
	if slot, ok := req.Context().Value(httpStatusKey{}).(*httpStatusSlot); ok && slot != nil {
		if err != nil {
			slot.code = 0
			slot.retryAfter = ""
		} else if resp != nil {
			slot.code = resp.StatusCode
			slot.retryAfter = resp.Header.Get("Retry-After")
		}
	}
	t.meter.mu.Lock()
	s := t.meter.senders[from]
	if s == nil {
		s = &senderStats{failed: make(map[string]uint64)}
		t.meter.senders[from] = s
	}
	s.durationSum += d
	s.durationCount++
	switch {
	case err != nil:
		s.failed["network"]++
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		s.sent++
	default:
		s.failed[strconv.Itoa(resp.StatusCode)]++
	}
	t.meter.mu.Unlock()
	return resp, err
}

// OnConfigReload implements cf.ConfigReloader. It rebuilds the client from the
// bound configuration source. The fresh value is delivered as cfg but the
// client is rebuilt from the source so the translation stays in one place. On
// failure the previous client is kept.
func (c *CFResend) OnConfigReload(source string, cfg any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source != c.configSource || c.client == nil || c.fw == nil {
		return
	}
	if _, ok := cfg.(*ResendConfig); !ok {
		c.logger.Error("cf_resend: config reload rejected", "source", source, "type", fmt.Sprintf("%T", cfg))
		return
	}
	if err := c.applyConfigFromSource(); err != nil {
		c.logger.Error("cf_resend: config reload rejected; keeping previous", "err", err)
		return
	}
	newClient, err := c.buildClient()
	if err != nil {
		c.logger.Error("cf_resend: config reload create client failed; keeping previous", "err", err)
		return
	}
	c.client = newClient
	c.reloads.Add(1)
	c.logger.Info("cf_resend: reconfigured after config reload",
		"from", c.from,
		cf_logs.SecretSet("api_key", c.apiKey),
	)
}

// RegisterConfigSources implements cf.ConfigSourceRegistrar. The framework
// calls it during argv absorption; it registers this component's configuration
// source (name, path, env prefix, format, Owner) with the configuration
// component. No-op when no source is bound.
func (c *CFResend) RegisterConfigSources(conf any) error {
	cfg, ok := conf.(*cf_configuration.Configuration)
	if !ok {
		return fmt.Errorf("cf_resend: RegisterConfigSources: expected configuration component, got %T", conf)
	}
	if c.configSource == "" {
		return nil
	}
	format := c.srcFormat
	if !c.srcFormatSet {
		if p := strings.ToLower(c.configPath); strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
			format = cf_configuration.FormatYAML
		} else {
			format = cf_configuration.FormatJSON
		}
	}
	return cf_configuration.AddSource(cfg, cf_configuration.Source[ResendConfig]{
		Name:      c.configSource,
		Path:      c.configPath,
		Format:    format,
		Owner:     c.Name(),
		EnvPrefix: c.srcEnvPrefix,
	})
}

// Shutdown implements cf.CaerusComponent. It unsubscribes the logs subscription
// and releases the client. Further use of Client() after shutdown returns nil.
func (c *CFResend) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logsSub != nil {
		c.logsSub.Unsubscribe()
		c.logsSub = nil
	}
	c.client = nil
	return nil
}

// Client returns the live Resend SDK client. It is non-nil after a successful
// Init and nil before Init or after Shutdown. Call it per use rather than
// caching the pointer; the component swaps the client on config reload.
func (c *CFResend) Client() *resend.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

// From returns the configured soft-default sender (from_address /
// WithFromAddress). Empty means every Send must set Mail.From.
func (c *CFResend) From() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.from
}

// BaseURL returns the configured Resend API endpoint (empty = the SDK default).
func (c *CFResend) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// Send sends m through Resend and returns the provider message id.
//
// From: from_address / WithFromAddress is a soft default when Mail.From is
// empty. If both are empty, or the resolved From or any To address does not
// parse (`net/mail.ParseAddress`), Send fails. HTML and Text may both be set;
// at least one must be non-empty.
//
// If the first HTTP status is 429 or 5xx, Send waits (Retry-After, capped at
// 1s) and tries once more while ctx is live. 4xx other than 429 and network
// errors are not retried.
func (c *CFResend) Send(ctx context.Context, m Mail) (string, error) {
	client := c.Client()
	if client == nil {
		return "", errors.New("cf_resend: Send before Init or after Shutdown")
	}
	from, err := resolveFrom(m.From, c.From())
	if err != nil {
		return "", err
	}
	to, err := validateTo(m.To)
	if err != nil {
		return "", err
	}
	m.To = to
	if reply := strings.TrimSpace(m.ReplyTo); reply != "" {
		if err := validateMailbox("ReplyTo", reply); err != nil {
			return "", err
		}
		m.ReplyTo = reply
	}
	if m.HTML == "" && m.Text == "" {
		return "", errors.New("cf_resend: Mail needs HTML or Text")
	}
	req := m.toSDK(from)
	id, err, slot := c.sendOnce(ctx, client, from, m, req)
	if err == nil {
		return id, nil
	}
	if !shouldRetryStatus(slot.code) {
		return "", err
	}
	if waitErr := waitRetry(ctx, retryWaitDuration(slot.retryAfter)); waitErr != nil {
		return "", err
	}
	c.meter.addRetry(from)
	client = c.Client()
	if client == nil {
		return "", err
	}
	id, err, _ = c.sendOnce(ctx, client, from, m, req)
	return id, err
}

func (c *CFResend) sendOnce(ctx context.Context, client *resend.Client, from string, m Mail, req *resend.SendEmailRequest) (string, error, httpStatusSlot) {
	slot := &httpStatusSlot{}
	sendCtx := context.WithValue(ctx, fromCtxKey{}, from)
	sendCtx = context.WithValue(sendCtx, httpStatusKey{}, slot)
	var (
		resp *resend.SendEmailResponse
		err  error
	)
	if m.IdempotencyKey != "" {
		resp, err = client.Emails.SendWithOptions(sendCtx, req, &resend.SendEmailOptions{IdempotencyKey: m.IdempotencyKey})
	} else {
		resp, err = client.Emails.SendWithContext(sendCtx, req)
	}
	if err != nil {
		return "", &SendError{err: err, status: slot.code}, *slot
	}
	if resp == nil {
		return "", errors.New("cf_resend: empty send response"), *slot
	}
	return resp.Id, nil, *slot
}

// Health implements cf.HealthProvider. Resend exposes no liveness endpoint, so
// health reflects that a client is initialized (nil before Init or after
// Shutdown). Connectivity is verified on each Send error.
func (c *CFResend) Health(ctx context.Context) error {
	if c.Client() == nil {
		return errors.New("cf_resend: client is not initialized")
	}
	return nil
}

// Metrics implements cf_observability.MetricsProvider. It reports the resend
// client's identity, per-sender send traffic and latency; before Init or after
// Shutdown it returns nil, so the observability component skips it (lazy
// pickup).
//
// resend_info is a snapshot descriptor gauge (value 1) carrying the live
// configured sender identity; it is the "component is initialized" marker.
// Traffic counters are bucketed by the actual sender of each email (the
// resolved req.From, or the configured default), so the from label is accurate
// per send and a from_address change never relabels history.
func (c *CFResend) Metrics() []cf_observability.Metric {
	if c.Client() == nil {
		return nil
	}
	infoLabels := map[string]string{
		"component": c.Name(),
		"from":      c.From(),
		"base_url":  c.BaseURL(),
	}
	ms := []cf_observability.Metric{
		{
			Name:   "resend_info",
			Help:   "Resend client descriptor; 1 while initialized.",
			Value:  1,
			Labels: copyLabels(infoLabels),
		},
		{
			Name:   "resend_config_reloads_total",
			Help:   "Total number of successful client rebuilds after a configuration reload.",
			Value:  float64(c.reloads.Load()),
			Labels: map[string]string{"component": c.Name()},
			Type:   cf_observability.MetricTypeCounter,
		},
	}
	senders := c.meter.snapshot()
	if len(senders) == 0 {
		// No traffic yet: emit a zero series for the configured default (or
		// "unknown") so dashboards see the counters before the first send.
		defaultFrom := c.From()
		if defaultFrom == "" {
			defaultFrom = unknownFrom
		}
		return c.trafficMetrics(ms, defaultFrom, senderStats{})
	}
	froms := make([]string, 0, len(senders))
	for from := range senders {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	for _, from := range froms {
		ms = c.trafficMetrics(ms, from, senders[from])
	}
	return ms
}

// trafficMetrics appends the per-sender traffic series for one sender.
func (c *CFResend) trafficMetrics(ms []cf_observability.Metric, from string, s senderStats) []cf_observability.Metric {
	labels := map[string]string{"component": c.Name(), "from": from}
	ms = append(ms,
		cf_observability.Metric{
			Name:   "resend_emails_sent_total",
			Help:   "Total number of emails accepted by Resend (HTTP 2xx).",
			Value:  float64(s.sent),
			Labels: copyLabels(labels),
			Type:   cf_observability.MetricTypeCounter,
		},
		cf_observability.Metric{
			Name:   "resend_send_duration_seconds_sum",
			Help:   "Total Resend send latency in seconds (sum of all attempts).",
			Value:  s.durationSum,
			Labels: copyLabels(labels),
			Type:   cf_observability.MetricTypeCounter,
		},
		cf_observability.Metric{
			Name:   "resend_send_duration_seconds_count",
			Help:   "Total number of Resend send attempts (2xx and failures).",
			Value:  float64(s.durationCount),
			Labels: copyLabels(labels),
			Type:   cf_observability.MetricTypeCounter,
		},
		cf_observability.Metric{
			Name:   "resend_send_retries_total",
			Help:   "Total number of second Send attempts after HTTP 429 or 5xx.",
			Value:  float64(s.retries),
			Labels: copyLabels(labels),
			Type:   cf_observability.MetricTypeCounter,
		},
	)
	codes := make([]string, 0, len(s.failed))
	for code := range s.failed {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		fl := copyLabels(labels)
		fl["error_code"] = code
		ms = append(ms, cf_observability.Metric{
			Name:   "resend_emails_failed_total",
			Help:   "Total number of failed email sends, keyed by error code (HTTP status, or \"network\" for transport errors).",
			Value:  float64(s.failed[code]),
			Labels: fl,
			Type:   cf_observability.MetricTypeCounter,
		})
	}
	return ms
}

// copyLabels returns a shallow copy of a label map so callers cannot mutate
// the component's internal state.
func copyLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var _ cf.CaerusComponent = (*CFResend)(nil)
var _ cf.Dependencies = (*CFResend)(nil)
var _ cf.HealthProvider = (*CFResend)(nil)
var _ cf_observability.MetricsProvider = (*CFResend)(nil)
var _ cf.ConfigReloader = (*CFResend)(nil)
var _ cf.ConfigSourceRegistrar = (*CFResend)(nil)
