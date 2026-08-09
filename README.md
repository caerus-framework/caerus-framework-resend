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

```go
fw := cf.New()

logs := cf_logs.New(cf_logs.WithWriter(os.Stdout))
resend := cf_resend.New(cf_resend.WithConfigSource("resend", "config/resend.json"))
fw.AddComponent(logs)
fw.AddComponent(resend) // GetDependencies() -> [logs configuration]
```

The component is `cf.ConfigSourceRegistrar`-self-sufficient: `WithConfigSource`
registers the `Source[ResendConfig]` with the configuration component during
argv absorption, so `main` never touches `os.Getenv`/`ParseFlags`. The `--resend`
path flag and per-field flags come from the source declaration.

## Sending

`Send` fills `From` from the configured `from_address` when the request leaves
it empty, requires at least one recipient, and honors the context end-to-end
(`Emails.SendWithContext`):

```go
resend := cf.MustGet[*cf_resend.CFResend](fw)

resp, err := resend.Send(ctx, &resendv2.SendEmailRequest{
	To:      []string{"user@example.com"},
	Subject: "Welcome",
	Html:    "<p>Hi!</p>",
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
| `WithFromAddress(from)` | default sender address |
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
  "api_key": "re_...",
  "from_address": "noreply@caerus.works"
}
```

**Files are canonical in Kubernetes**, including the API key — mount a
Secret/ConfigMap and let `fsnotify` + `OnConfigReload` rotate the client without
a restart. On reload failure the previous client stays live (last-good). Resend
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
| `resend_emails_failed_total` | counter | `component`, `from`, `error_code` |
| `resend_send_duration_seconds_sum` | counter | `component`, `from` |
| `resend_send_duration_seconds_count` | counter | `component`, `from` |

`resend_info` is a snapshot descriptor (the "component is initialized" marker);
its labels always reflect the live configured sender identity. Error codes are
the real HTTP statuses (e.g. `429`, `422`, `500`), or `network` for transport
errors — recorded at the transport layer because the resend SDK swallows status
codes in its errors. Average send latency is
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
