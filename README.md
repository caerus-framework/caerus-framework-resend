# caerus-framework-resend

[![CI](https://github.com/caerus-framework/caerus-framework-resend/actions/workflows/ci.yml/badge.svg)](https://github.com/caerus-framework/caerus-framework-resend/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/caerus-framework/caerus-framework-resend/graph/badge.svg)](https://codecov.io/gh/caerus-framework/caerus-framework-resend)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Caerus Framework Resend Component. A thin framework wrapper around the
[resend-go SDK](https://github.com/resend/resend-go) so components and apps can
send email without importing the SDK directly: framework-owned lifecycle,
configuration (file + env + flags), live config reload with last-good
semantics, logging through the framework `logs` component, and observability
health/metrics.

## Wiring

Two wiring shapes are supported. Prefer the **app-owned** shape (demoapp
golden path): `main` declares only the chassis (resend alongside postgres /
valkey) and the app class; product machinery that sends email lives under the
app and resolves resend as a peer at `Init`. Use the simple `main`-level shape
for one-off binaries.

### App-owned consumer (golden — demoapp pattern)

`main` declares resend as chassis and runs the app class; it never touches
resend itself:

```go
fw := cf.New(&cf.FrameworkOptions{
	Logs: &cf.LogsSettings{Format: "json", Level: "info", ConfigSource: "logs"},
	Observability: &cf.ObservabilitySettings{Bind: ":9090", ConfigSource: "observability"},
	Components: []cf.CaerusComponent{
		cf_postgres.New(cf_postgres.WithConfigSource("postgresql", "config/postgresql.json")),
		cf_resend.New(cf_resend.WithConfigSource("resend", "config/resend.json")),
		app.New(app.Options{}),
	},
})
if err := fw.RunWithSignals(context.Background()); err != nil {
	log.Fatal(err)
}
```

The app resolves the resend **component pointer** once at `Init` (never a
client snapshot), declares it in `GetDependencies`, and calls `Send` per use:

```go
type App struct {
	email *cf_resend.CFResend
}

func (a *App) GetDependencies() []string {
	return []string{cf_resend.ComponentName} // + logs, chassis peers
}

func (a *App) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	email, ok := cf.Get[*cf_resend.CFResend](fw)
	if !ok {
		return errors.New("app: resend component missing")
	}
	a.email = email
	return nil
}
```

### Simple `main`-level wiring

For a one-off binary, register the components directly and use
`cf.MustGet` to reach the component:

```go
fw := cf.New()

logs := cf_logs.New(cf_logs.WithWriter(os.Stdout))
resend := cf_resend.New(cf_resend.WithConfigSource("resend", "config/resend.json"))
fw.AddComponent(logs)
fw.AddComponent(resend) // GetDependencies() -> [logs configuration]
```

In both shapes the component is `cf.ConfigSourceRegistrar`-self-sufficient:
`WithConfigSource` registers the `Source[ResendConfig]` with the configuration
component during argv absorption, so `main` never touches
`os.Getenv`/`ParseFlags`. The `--resend` path flag and per-field flags come from
the source declaration.

## Sending

`Send` takes a Caerus `Mail` value (SES-simple: From, To, Subject, HTML/Text,
ReplyTo, Tags, optional IdempotencyKey). Apps do **not** import resend-go for
ordinary mail. One extra attempt runs on HTTP **429** or **5xx** (honor
`Retry-After`, wait capped at 1s, stop if `ctx` is done). 422 and network
errors are not retried.

**From:** `from_address` / `WithFromAddress` is a **soft default**. Empty
`Mail.From` uses it. Non-empty `Mail.From` overrides that send. If both are
empty, or the resolved address or any `To` does not parse as an email
(`net/mail.ParseAddress`), Send fails. At least one of HTML or Text is required.

Attachments, Cc/Bcc, and scheduled send stay on `Client()` (that path
imports the SDK on purpose).

```go
// in the app's Init — store the component pointer
a.email, _ = cf.Get[*cf_resend.CFResend](fw)

id, err := a.email.Send(ctx, cf_resend.Mail{
	To:      []string{"user@example.com"},
	Subject: "Welcome",
	HTML:    "<p>Hi!</p>",
})
if err != nil {
	if cf_resend.HTTPStatus(err) == 429 {
		// rate limited
	}
	return err
}

// per-call From override
id, err = a.email.Send(ctx, cf_resend.Mail{
	From:    "brand@example.com",
	To:      []string{"user@example.com"},
	Subject: "Welcome",
	Text:    "Hi",
})
```

Peers resolve the component once at `Init` (declare `cf_resend.ComponentName`
in `GetDependencies`) and call `Send`/`Client()` per use — never snapshot the
client, since config reload swaps it.

## Options

| Option | Description |
| --- | --- |
| `WithConfig(ResendConfig)` | static config snapshot; non-zero fields override option-set defaults |
| `WithConfigSource(name, path, …)` | bind a configuration source for Init + `OnConfigReload`; the module registers the `Source[ResendConfig]` itself (declares `configuration` dep) |
| `WithAPIKey(key)` | set the Resend API key directly (tests, embedded use) |
| `WithFromAddress(from)` | soft-default sender; empty `Mail.From` uses it; must be a valid address when used |
| `WithBaseURL(url)` | override the Resend API endpoint (self-hosted / test stub) |
| `WithTimeout(d)` | per-send HTTP timeout (default `10s`) |
| `WithHTTPClient(*http.Client)` | override the HTTP client (stub RoundTripper in tests) |
| `WithName(name)` | custom component name for multiple instances (default `"resend"`) |
| `WithLogger(*slog.Logger)` | explicit logger override; defaults to the framework `logs` component's logger (re-delivered on `logs` `Reconfigure`), falling back to `slog.Default()` |

## Configuration

Load `ResendConfig` through the configuration component. The default
`EnvPrefix` is `RESEND_` (from the source name); `env` tags map `RESEND_API_KEY`,
`RESEND_FROM_ADDRESS`, `RESEND_BASE_URL`, `RESEND_TIMEOUT_SEC`.

```json
{
  "api_key": "…",
  "from_address": "noreply@example.com"
}
```

**Files are canonical in Kubernetes**, including the API key — mount a
Secret/ConfigMap and let `fsnotify` + `OnConfigReload` rotate the client without
a restart. `api_key` is tagged `secret:"redact"`: reload logs `api_key_set`,
never the key. Do not log the config struct. On reload failure the previous client stays live (last-good). Resend
is a stateless HTTP wrapper, so `Client()` and `Send` keep working through a
swap.

`Health` reports initialized/uninitialized (Resend has no liveness endpoint;
send failures surface per call). `Metrics` emits the following while
initialized, nil before Init/after Shutdown:

| Metric | Type | Labels |
| --- | --- | --- |
| `resend_info` | gauge 1 | `component`, `from`, `base_url` |
| `resend_config_reloads_total` | counter | `component` |
| `resend_emails_sent_total` | counter | `component`, `from` |
| `resend_send_retries_total` | counter | `component`, `from` |
| `resend_send_duration_seconds_sum` | counter | `component`, `from` |
| `resend_send_duration_seconds_count` | counter | `component`, `from` |

`resend_info` is a snapshot descriptor (the "component is initialized" marker);
its labels always reflect the live configured sender identity. Error codes are
the real HTTP statuses (e.g. `429`, `422`, `500`), or `network` for transport
errors — recorded at the transport layer because the resend SDK swallows status
codes in its errors. `Send` wraps those failures as `*SendError`:
`cf_resend.HTTPStatus(err)` is the same number (0 = network / never an HTTP
response). `errors.As` to `*SendError` works. Direct `Client().Emails` calls
are not wrapped. Average send latency is
`resend_send_duration_seconds_sum / resend_send_duration_seconds_count`; the
send error rate is
`rate(resend_emails_failed_total[5m]) / rate(resend_emails_sent_total[5m])`
grouped by `error_code`.

The `from` label on the traffic counters is the **actual sender of each email** —
the resolved `req.From`, or the configured default when the request leaves it
empty. Sends are bucketed per sender at send time, so overrides and a
`from_address` change never relabel history. Sends made directly through
`Client().Emails` (bypassing `Send`) are attributed to `from="unknown"`.
Before any traffic, the counters appear at zero for the configured default
sender.

## Tests

Unit tests cover config layering, the Init contract, reload last-good, and
health/metrics transitions using a stub `http.RoundTripper` — no external
service.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
