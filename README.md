# cpa-deepseek

<p align="center">
  <img src="assets/logo.svg" alt="DeepSeek" width="200" height="200">
</p>

<p align="center">
  <strong>English</strong> · <a href="README.zh-CN.md">简体中文</a>
</p>

<p align="center">
  <a href="#install">install</a> ·
  <a href="#configuration">configuration</a> ·
  <a href="#models-and-protocols">protocols</a> ·
  <a href="#credentials-quotas-and-cooldowns">quotas</a> ·
  <a href="#build">build</a> ·
  <a href="https://github.com/dillonzq/cpa-deepseek/actions/workflows/build.yml">builds</a>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-555" alt="License: MIT"></a>
  <a href="https://help.router-for.me/plugin/development"><img src="https://img.shields.io/badge/CLIProxyAPI-8.0.0%2B-555" alt="CLIProxyAPI 8.0.0+"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.26.10%2B-555" alt="Go 1.26.10+ for source builds"></a>
</p>

---

A native dynamic Go plugin for [CLIProxyAPI](https://help.router-for.me/plugin/development) (CPA) that connects to the [official DeepSeek API](https://api-docs.deepseek.com/) as provider **`deepseek`**.

Configure one credential pool for every model and client protocol. The plugin handles model discovery, upstream requests and protocol conversion; CPA selects credentials and manages rotation, retries, session affinity and cooldowns.

The plugin ID is `cpa-deepseek`. The provider ID, credential type and default model prefix are `deepseek`.

## Features

- **Official model discovery.** Read models, context/output limits, modalities and reasoning levels from DeepSeek's `/models` endpoint; add explicit metadata overrides when needed.
- **Four client protocols.** Native Chat Completions, Responses and Anthropic Messages, plus Gemini GenerateContent through [apix](https://github.com/dillonzq/apix/tree/v0.1.0), with ordinary and streaming requests.
- **Tools and reasoning.** Preserve native tool definitions, tool results, `reasoning_content`, supported image inputs and protocol-specific extensions. Apply DeepSeek thinking controls and CPA model-name suffixes.
- **Multiple named credentials.** Stable key-derived identities, selected-key execution and a shared CPA scheduler across protocols.
- **Native CPA v8 quotas.** Fetch fresh account balances for the selected credential, with separate CNY/USD currency summaries and account availability.
- **Targeted cooldown recovery.** Restore a selected credential through CPA's native cooldown reset endpoint after a top-up or transient failure.
- **Cancellable HTTP.** Requests use CPA host HTTP operations; disconnects, timeouts and plugin shutdown cancel upstream work and release streams.

## Requirements

- **CLIProxyAPI:** `v8.0.0+` with native plugins enabled.
- **Upstream:** a [DeepSeek API key](https://platform.deepseek.com/) and sufficient account balance for inference.
- **Source builds only:** the Go version required by [go.mod](go.mod), CGO and a C compiler targeting the CPA host platform. Prebuilt libraries do not require Go.

## Install

Choose the operating system and architecture **running CPA**, including its container environment. Download the matching ZIP from [GitHub Releases](https://github.com/dillonzq/cpa-deepseek/releases) and verify it against the attached `checksums.txt`. ZIPs use the store-compatible name `cpa-deepseek_<version>_<goos>_<goarch>.zip`, for example `cpa-deepseek_0.1.0_linux_amd64.zip`. Before the first release, use a successful [Actions build artifact](https://github.com/dillonzq/cpa-deepseek/actions/workflows/build.yml), or [build locally](#build).

| CPA host | Architecture | Library |
| --- | --- | --- |
| Linux | amd64 / arm64 | `cpa-deepseek.so` |
| macOS | amd64 / arm64 | `cpa-deepseek.dylib` |
| Windows | amd64 | `cpa-deepseek.dll` |

1. Extract the artifact and put the matching library in CPA's plugin directory, for example `plugins/cpa-deepseek.so` on Linux. Keep `LICENSE` and the asset source notes with redistributed packages.
2. Add the [minimal configuration](#minimal-configuration) and set `DEEPSEEK_API_KEY` in the **CPA process environment**.
3. Restart CPA and check that `cpa-deepseek` loads successfully.
4. Query CPA's `/v1/models` with a **CPA client key**, then request a listed `deepseek/<model-id>` model.

```bash
curl http://localhost:8317/v1/models \
  -H "Authorization: Bearer $CPA_CLIENT_KEY"

curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_CLIENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek/deepseek-flash","messages":[{"role":"user","content":"Hello"}]}'
```

Adjust the host and port for your CPA deployment. The DeepSeek key is used upstream; CPA client keys authorize inference requests, and CPA management keys authorize the management operations below.

## Configuration

Settings belong under `plugins.configs.cpa-deepseek`. Enable both the global plugin switch and this plugin's switch. Unknown provider settings are rejected; environment variables expand only in `credentials[].api-key`. An unset variable expands to an empty key and fails validation.

### Minimal configuration

```yaml
config-version: 8
plugins:
  enabled: true
  configs:
    cpa-deepseek:
      enabled: true
      credentials:
        - name: Personal
          api-key: "${DEEPSEEK_API_KEY}"
```

### Multiple credentials

Replace the `credentials` array inside the plugin configuration with:

```yaml
credentials:
  - name: Personal
    api-key: "${DEEPSEEK_API_KEY}"
  - name: Team
    api-key: "${DEEPSEEK_TEAM_API_KEY}"
```

Each entry accepts `name` and `api-key`. At least one non-empty key is required, and duplicate keys are rejected. Different keys for the **same DeepSeek account share its balance and concurrency limits**; a larger key pool does not increase that account's quota.

### Configuration options

The [complete example](config.example.yaml) includes the default settings.

| Setting | Default | Meaning |
| --- | --- | --- |
| `base-url` | `https://api.deepseek.com` | API root; endpoint paths are appended |
| `model-prefix.enabled` | `true` | Prefix public model IDs |
| `model-prefix.value` | `deepseek` | For example, `deepseek/deepseek-flash` |
| `catalog.refresh-interval` | `15m` | Discovery interval, at least `1m` |
| `catalog.stale-while-unavailable` | `true` | Retain the last successful discovery snapshot during outages |
| `models` | empty | Model declarations and metadata overrides |
| `quota.timeout` | `15s` | Balance lookup or credential validation timeout, at most `1m` |
| `request-timeout` | `15m` | Total upstream request timeout, including streams |
| `max-response-bytes` | `67108864` | Accepted non-streaming response size or cumulative stream size: 64 MiB |
| `allow-http` | `false` | Allow HTTP for local test servers |

Use an API root such as `https://api.deepseek.com`; the plugin appends `/models`, `/user/balance` and the inference paths below without removing existing path segments. `max-response-bytes` is enforced by the plugin after host reads, rather than being a transport-level memory limit. HTTP requests use CPA's host transport and global proxy policy.

### Model metadata overrides

Declarations use CPA provider field names. For example, add this under `plugins.configs.cpa-deepseek`:

```yaml
models:
  - name: deepseek-flash
    display-name: DeepSeek Flash
    max-context-length: 1048576
    max-tokens: 393216
    input-modalities: [text, image]
    output-modalities: [text]
    thinking:
      zero-allowed: true
      levels: [low, high, max]
```

Explicit fields override discovered metadata. A declaration can also add a model missing from discovery, and remains available during a discovery outage. Metadata describes capabilities; declaring a model or modality does not enable unsupported upstream features. Keep overrides aligned with the [official model information](https://api-docs.deepseek.com/api/list-models/).

## Models and protocols

Discovery calls the official [`GET /models`](https://api-docs.deepseek.com/api/list-models/) using configured keys, trying another key if an attempt fails. Each attempt is limited to 3 seconds, and the complete refresh to 10 seconds. Discovery does not change execution cooldowns. With stale serving enabled, a failed refresh retains the last successful snapshot; otherwise only explicit model declarations remain.

Models are discovered rather than hard-coded. Current official examples use `deepseek-flash` and `deepseek-v4-pro`; use the IDs returned by your deployment's `/v1/models`.

| Client format | DeepSeek upstream path | Handling |
| --- | --- | --- |
| OpenAI Chat Completions | `/chat/completions` | Native |
| OpenAI Responses | `/responses` | Native |
| Anthropic Messages | `/anthropic/v1/messages` | Native |
| Gemini GenerateContent | `/chat/completions` | Request, response and stream conversion |

Native requests preserve API-specific fields and extensions. Cross-protocol conversion returns an explicit error when a capability cannot be represented. Image support depends on the selected model and protocol. Separate Files, Search and Fetch endpoint proxying and token-counting operations are not implemented; supported server-side tools carried in native inference requests are preserved.

### Thinking and tools

The API enables thinking by default. The plugin follows DeepSeek's [thinking controls](https://api-docs.deepseek.com/guides/thinking_mode/):

| Requested effort | Effective effort |
| --- | --- |
| `none` | Thinking disabled |
| `minimal`, `low` | `low` |
| `auto`, `medium`, `high`, `xhigh` | `high` |
| `max`, `ultra` | `max` |

Use native request controls or a CPA model suffix such as `deepseek/deepseek-flash(none)` or `deepseek/deepseek-flash(max)`. The suffix takes precedence over the body. When normalizing effort alone, the plugin preserves an explicit native thinking toggle; a suffix or `none` control applies its corresponding toggle. For tool conversations, include the required reasoning history; the plugin preserves `reasoning_content` rather than reconstructing it.

In Chat Completions, thinking mode does not support `tool_choice: required` or a named tool choice. Disable thinking explicitly when using those choices; upstream validation errors retain their real HTTP status.

### Responses and streaming

DeepSeek's [Responses API](https://api-docs.deepseek.com/guides/responses_api/) is stateless. Send the complete input history on every request. Non-empty `previous_response_id` or `conversation` is rejected before sending the request to avoid silently losing context. Native Responses retains supported `apply_patch` and server-side tool fields.

The plugin handles non-streaming whitespace and SSE keep-alive comments, and uses each protocol's own terminal events. HTTP errors before streaming starts can trigger CPA credential failover. Errors after output has started terminate the stream; incomplete or malformed streams report a failure.

## Credentials, quotas and cooldowns

Each key is materialized as a separate CPA credential. IDs and filenames use the key's SHA-256 digest, so renaming or reordering entries does not change identity. `name` is the display label applied when the credential is parsed or refreshed. Configuration reload preserves existing credential storage and host-managed state, including proxy and disabled settings.

Removing a key from configuration prevents its execution, quota lookup, refresh and per-auth model publication. Its physical credential file remains under CPA management and can be deleted there. Each execution, quota fetch and refresh uses the **selected key**; a failed quota lookup never falls back to another account.

Management operations require a **CPA management key**. Find the target `auth_index` and credential filename (`name`) first:

```bash
curl http://localhost:8317/v8/management/credentials \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

### Fetch fresh quota

```bash
curl -X POST http://localhost:8317/v8/management/credentials/quota/fetch \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"auth_index":"<selected-auth-index>"}'
```

`GET /v8/management/plugins/cpa-deepseek/quota?auth_index=<index>` and POST to the same plugin route also work. Every fetch makes a fresh [`GET /user/balance`](https://api-docs.deepseek.com/api/get-user-balance/) request with the selected credential.

| Official field | CPA quota summary |
| --- | --- |
| `is_available` | `api_available`: `1` means sufficient account balance, `0` means insufficient balance |
| `total_balance` | Total available balance, including granted and topped-up balances |
| `granted_balance` | Unexpired granted balance |
| `topped_up_balance` | Topped-up balance |

CNY and USD have separate currency summaries; they are not added together or converted. Decimal amounts are parsed before conversion to CPA's numeric fields. The balance API supplies no total quota allowance or reset schedule, so the plugin does not invent percentages or reset times. Malformed responses produce an error, not a fabricated zero balance. A successful lookup with `is_available: false` still returns the actual balance information.

The data belongs to the **DeepSeek account**, even though the query is selected by credential. Keys for the same account may show the same balance and share [account-level concurrency limits](https://api-docs.deepseek.com/quick_start/rate_limit/).

### Clear a selected cooldown

After a top-up or transient recovery, clear only the target credential's local routing cooldown:

```bash
curl -X POST http://localhost:8317/v8/management/routing/cooldown/reset \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"auth_index":"<selected-auth-index>"}'
```

Quota fetches preserve cooldowns. Cooldown reset does not alter DeepSeek balance or spending. New upstream 402 or 429 responses can cool the credential again; cooldown duration and retry behavior follow CPA v8's native rules. Resetting upstream balance is unsupported (`supports_reset: false`).

### Validate and refresh a credential

```bash
curl -X POST http://localhost:8317/v8/management/credentials/refresh \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"<credential-filename>.json","auth_index":"<selected-auth-index>"}'
```

Static API keys have no token-renewal flow. `auth.refresh` validates the selected key and account availability through the balance API, preserving identity, storage and host attributes. An invalid key returns 401; insufficient balance returns 402. Use the dedicated cooldown reset endpoint for balance or rate-limit cooldown recovery.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Plugin does not load | Match CPA's OS/architecture, enable both plugin switches and inspect CPA's plugin loading log |
| Configuration is rejected | Check required keys, environment variables and supported field names in [the example](config.example.yaml) |
| No DeepSeek models | Check `/models` connectivity and key validity; add explicit declarations if discovery must work offline |
| HTTP 401 | Check the selected DeepSeek key; refresh cannot renew or replace an invalid static key |
| HTTP 402 | Top up the DeepSeek account, fetch fresh quota, then reset the selected local cooldown |
| HTTP 429 | Pace requests and check account concurrency; keys for one account share the limit |
| HTTP 400 / 422 | Check parameters, thinking/tool-choice restrictions and stateless Responses history |
| HTTP 500 / 503 or a timeout | Check [DeepSeek service status](https://status.deepseek.com/) and retry through CPA's policy |

See the [official error meanings](https://api-docs.deepseek.com/quick_start/error_codes/) for upstream failures.

## Build

Use the toolchain required by [go.mod](go.mod), with CGO enabled and a C compiler for the target platform:

```bash
mkdir -p dist
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/cpa-deepseek.so .
```

On macOS, use `dist/cpa-deepseek.dylib`. On Windows, run `go build -trimpath -buildmode=c-shared -o dist/cpa-deepseek.dll .` with `CGO_ENABLED=1` and an available C compiler.

Version metadata defaults to `0.0.0-dev`. Inject a version with `-ldflags '-X github.com/dillonzq/cpa-deepseek/internal/plugin.pluginVersion=0.1.0'`, or run `bash scripts/package.sh v0.1.0` (requires Python 3) to build `dist/cpa-deepseek_0.1.0_<goos>_<goarch>.zip` for the current Go target, with the dynamic library at the ZIP root. CI defines Linux/macOS amd64 and arm64 builds, plus Windows amd64. Build artifacts include both READMEs, the sample configuration, license and icon source notes. Branch and pull request packages use `0.0.0`; tag builds prepare a draft GitHub Release with all five ZIPs and SHA-256 entries in `checksums.txt`; see [the release procedure](RELEASE.md).

## Testing

```bash
go mod download all
go test ./...
go test -race ./internal/...
go vet -tags debug ./...
go test -tags debug ./...
```

Linux/macOS debug integration tests load the real shared library into CPA v8 through a temporary Go source overlay. Local mock servers verify selected-key quota, 402/429 failover, targeted cooldown reset, static credential refresh, cancellation and unload. SDK module-cache sources are not modified; download all host dependencies before running the offline overlay tests.

Default tests use no real DeepSeek key and make no paid calls. Live inference and account access require separate verification with your own credentials.

## Official documentation

[First API call](https://api-docs.deepseek.com/) · [Models](https://api-docs.deepseek.com/api/list-models/) · [Balance](https://api-docs.deepseek.com/api/get-user-balance/) · [Thinking](https://api-docs.deepseek.com/guides/thinking_mode/) · [Responses](https://api-docs.deepseek.com/guides/responses_api/) · [Anthropic](https://api-docs.deepseek.com/guides/anthropic_api/) · [Rate limits](https://api-docs.deepseek.com/quick_start/rate_limit/) · [Errors](https://api-docs.deepseek.com/quick_start/error_codes/)

## License and icon source

Plugin code: [MIT](LICENSE). The DeepSeek icon places the [official API documentation favicon](https://api-docs.deepseek.com/img/favicon.svg), with its shape and color unchanged, on a square white canvas. It is available as [SVG](assets/logo.svg) and [PNG](assets/logo.png); CPA's plugin metadata uses the PNG. See [asset notes](assets/README.md) for provenance and rights information. This is an independent integration and does not imply DeepSeek endorsement.
