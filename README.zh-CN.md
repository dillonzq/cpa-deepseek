# cpa-deepseek

<p align="center">
  <img src="assets/logo.svg" alt="DeepSeek" width="180">
</p>

<p align="center">
  <a href="README.md">English</a> · <strong>简体中文</strong>
</p>

<p align="center">
  <a href="#安装">安装</a> ·
  <a href="#配置">配置</a> ·
  <a href="#模型与协议">协议</a> ·
  <a href="#凭证quota-与冷却">quota</a> ·
  <a href="#构建">构建</a> ·
  <a href="https://github.com/dillonzq/cpa-deepseek/actions/workflows/build.yml">构建产物</a>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-555" alt="许可证：MIT"></a>
  <a href="https://help.router-for.me/plugin/development"><img src="https://img.shields.io/badge/CLIProxyAPI-8.0.0%2B-555" alt="CLIProxyAPI 8.0.0+"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.26.10%2B-555" alt="源码构建需要 Go 1.26.10+"></a>
</p>

---

用于 [CLIProxyAPI](https://help.router-for.me/plugin/development)（CPA）的 Go 原生动态库插件，以 **`deepseek`** provider 接入 [DeepSeek 官方 API](https://api-docs.deepseek.com/zh-cn/)。

所有模型和客户端协议共用一组凭证。插件负责模型发现、上游请求和协议转换；CPA 负责凭证选择、轮换、重试、会话亲和和冷却。

插件 ID 为 `cpa-deepseek`，provider ID、凭证类型和默认模型前缀均为 `deepseek`。

## 功能

- **官方模型发现**：从 DeepSeek `/models` 接口读取模型、上下文和输出上限、模态与推理级别，支持显式覆盖元数据。
- **四种客户端协议**：原生支持 Chat Completions、Responses、Anthropic Messages，通过 [apix](https://github.com/dillonzq/apix/tree/v0.1.0) 转换 Gemini GenerateContent；均支持普通和流式请求。
- **工具与推理**：保留原生工具定义、工具结果、`reasoning_content`、模型支持的图片输入及协议扩展字段，处理 DeepSeek thinking 控制和 CPA 模型名后缀。
- **多组命名凭证**：根据 key 生成稳定身份，严格使用选中凭证执行，各协议共用 CPA 调度状态。
- **CPA v8 原生 quota**：实时查询选中凭证的账户余额，分别显示 CNY/USD 金额与账户可用状态。
- **定向冷却恢复**：充值或临时故障恢复后，通过 CPA 原生接口解除指定凭证的冷却。
- **可取消 HTTP**：通过 CPA 宿主 HTTP operation 发出请求，客户端断开、超时和插件卸载会取消上游工作并释放流。

## 环境要求

- **CLIProxyAPI**：`v8.0.0+`，启用原生插件。
- **上游凭证**：[DeepSeek API key](https://platform.deepseek.com/)，推理需要账户余额充足。
- **仅源码构建**：使用 [go.mod](go.mod) 要求的 Go 版本，启用 CGO，并安装面向 CPA 宿主平台的 C 编译器。使用预编译动态库无需 Go。

## 安装

按 **CPA 实际运行的系统和架构** 选择动态库，容器部署时以容器环境为准。从 [GitHub Releases](https://github.com/dillonzq/cpa-deepseek/releases) 下载对应压缩包，并使用附件 `SHA256SUMS` 校验。首次发布前，可在成功运行的 [Actions 构建](https://github.com/dillonzq/cpa-deepseek/actions/workflows/build.yml)中下载可用产物，也可[本地构建](#构建)。

| CPA 宿主系统 | 架构 | 动态库 |
| --- | --- | --- |
| Linux | amd64 / arm64 | `cpa-deepseek.so` |
| macOS | amd64 / arm64 | `cpa-deepseek.dylib` |
| Windows | amd64 | `cpa-deepseek.dll` |

1. 解压产物，将匹配平台的动态库放入 CPA 插件目录，例如 Linux 下的 `plugins/cpa-deepseek.so`。分发安装包时保留 `LICENSE` 和图标来源说明。
2. 添加下方[最小配置](#最小配置)，并在 **CPA 进程环境**中设置 `DEEPSEEK_API_KEY`。
3. 重启 CPA，检查 `cpa-deepseek` 加载日志。
4. 使用 **CPA 客户端密钥**查询 `/v1/models`，再请求列表中的 `deepseek/<model-id>` 模型。

```bash
curl http://localhost:8317/v1/models \
  -H "Authorization: Bearer $CPA_CLIENT_KEY"

curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_CLIENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek/deepseek-flash","messages":[{"role":"user","content":"Hello"}]}'
```

请按部署调整地址和端口。DeepSeek key 用于上游；CPA 客户端密钥用于推理请求，CPA 管理密钥用于下方管理操作。

## 配置

设置位于 `plugins.configs.cpa-deepseek`。全局插件开关和此插件开关均需启用。未知 provider 配置会报错；环境变量仅在 `credentials[].api-key` 展开。未设置的变量展开为空 key，会导致校验失败。

### 最小配置

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

### 多凭证配置

在插件配置内，将 `credentials` 数组替换为：

```yaml
credentials:
  - name: Personal
    api-key: "${DEEPSEEK_API_KEY}"
  - name: Team
    api-key: "${DEEPSEEK_TEAM_API_KEY}"
```

每项支持 `name` 和 `api-key`，至少需要一个非空 key，重复 key 会被拒绝。**同一 DeepSeek 账户下的不同 key 共享账户余额和并发限制**；增加 key 数量不会增加该账户额度。

### 配置选项

[完整示例](config.example.yaml) 包含默认设置。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `base-url` | `https://api.deepseek.com` | API 根地址，后面追加端点路径 |
| `model-prefix.enabled` | `true` | 为公开模型 ID 添加前缀 |
| `model-prefix.value` | `deepseek` | 如 `deepseek/deepseek-flash` |
| `catalog.refresh-interval` | `15m` | 模型发现周期，至少 `1m` |
| `catalog.stale-while-unavailable` | `true` | 发现失败时保留最近成功的快照 |
| `models` | 空 | 模型声明及元数据覆盖 |
| `quota.timeout` | `15s` | 余额查询或凭证验证超时，最多 `1m` |
| `request-timeout` | `15m` | 上游请求总超时，包含流式读取 |
| `max-response-bytes` | `67108864` | 接受的普通响应大小或流累计大小，64 MiB |
| `allow-http` | `false` | 本地测试服务器需要 HTTP 时显式开启 |

基础地址使用 `https://api.deepseek.com` 这样的 API 根地址。插件直接追加 `/models`、`/user/balance` 和下文推理路径，不删除已有路径段。`max-response-bytes` 在宿主读取后由插件检查，不是 transport 层的内存限制。HTTP 请求使用 CPA 宿主 transport 和全局代理策略。

### 模型元数据覆盖

模型声明使用 CPA provider 字段名，例如在 `plugins.configs.cpa-deepseek` 下添加：

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

显式字段优先于接口返回的元数据。声明也可以补充发现结果中缺少的模型，并在发现接口不可用时继续生效。元数据仅描述能力，声明某个模型或模态不会启用上游不支持的功能；请与[官方模型信息](https://api-docs.deepseek.com/zh-cn/api/list-models/)保持一致。

## 模型与协议

模型发现使用配置凭证调用官方 [`GET /models`](https://api-docs.deepseek.com/zh-cn/api/list-models/)，某次失败时尝试下一个 key。单次尝试最多 3 秒，完整刷新最多 10 秒，不修改执行冷却。开启 stale serving 时，失败会保留最近成功快照；关闭后只保留显式模型声明。

模型通过发现接口获取，不硬编码列表。当前官方示例主要使用 `deepseek-flash` 和 `deepseek-v4-pro`，实际请求请以 CPA `/v1/models` 返回的 ID 为准。

| 客户端格式 | DeepSeek 上游路径 | 处理方式 |
| --- | --- | --- |
| OpenAI Chat Completions | `/chat/completions` | 原生 |
| OpenAI Responses | `/responses` | 原生 |
| Anthropic Messages | `/anthropic/v1/messages` | 原生 |
| Gemini GenerateContent | `/chat/completions` | 转换请求、响应和流 |

原生请求保留各 API 专有字段及扩展字段；跨协议转换遇到无法表达的能力时返回明确错误。图片支持取决于选中的模型和协议。当前未实现独立 Files、Search、Fetch 端点代理和 token 计数操作；原生推理请求中支持的服务端工具字段会保留。

### Thinking 与工具

官方 API 默认启用 thinking。插件按照 DeepSeek [推理控制](https://api-docs.deepseek.com/zh-cn/guides/thinking_mode/)处理级别：

| 请求级别 | 实际级别 |
| --- | --- |
| `none` | 关闭 thinking |
| `minimal`、`low` | `low` |
| `auto`、`medium`、`high`、`xhigh` | `high` |
| `max`、`ultra` | `max` |

可使用原生请求字段，或 `deepseek/deepseek-flash(none)`、`deepseek/deepseek-flash(max)` 这样的 CPA 模型名后缀。后缀优先于请求体。单独归一化 effort 时，插件保留原生请求中显式设置的 thinking 开关；模型名后缀或 `none` 控制仍会应用对应开关。工具调用会话需要调用方提供所需推理历史；插件保留 `reasoning_content`，不会重建缺失内容。

Chat Completions 的 thinking 模式不支持 `tool_choice: required` 或指定工具，使用时需要显式关闭 thinking。上游参数校验错误会保留真实 HTTP 状态码。

### Responses 与流式输出

DeepSeek [Responses API](https://api-docs.deepseek.com/zh-cn/guides/responses_api/) 是无状态接口，每次请求都需要完整 input 历史。非空 `previous_response_id` 或 `conversation` 会在发送前被拒绝，避免静默丢失上下文。原生 Responses 保留官方支持的 `apply_patch` 和服务端工具字段。

插件处理普通响应中的空白和 SSE keep-alive 注释，按各协议自己的终止事件结束流。流开始前的 HTTP 错误可以触发 CPA 切换凭证；开始输出后的错误终止当前流，异常中断或畸形流会报告失败。

## 凭证、quota 与冷却

每个 key 对应独立 CPA 凭证，ID 和文件名基于 key 的 SHA-256，因此改名或调整顺序不会改变身份。`name` 在凭证解析或刷新时作为显示标签。配置重载保留已有凭证存储和宿主状态，包括代理与禁用设置。

从配置移除 key 后，该 key 无法继续执行、查询 quota、刷新或发布对应凭证的模型；实体凭证文件仍由 CPA 管理，可在凭证管理中删除。执行、quota 查询和刷新均使用 **CPA 当前选中的 key**，quota 查询失败不会改用其他账户代查。

管理操作需要 **CPA 管理密钥**。先查询目标凭证的 `auth_index` 和文件名 `name`：

```bash
curl http://localhost:8317/v8/management/credentials \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY"
```

### 刷新 quota

```bash
curl -X POST http://localhost:8317/v8/management/credentials/quota/fetch \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"auth_index":"<selected-auth-index>"}'
```

也支持 `GET /v8/management/plugins/cpa-deepseek/quota?auth_index=<index>` 和 POST 到同一路由。每次都使用所选凭证实时请求 [`GET /user/balance`](https://api-docs.deepseek.com/zh-cn/api/get-user-balance/)。

| 官方字段 | CPA quota summary |
| --- | --- |
| `is_available` | `api_available`：`1` 表示账户余额充足，`0` 表示不足 |
| `total_balance` | 总可用余额，包含赠送和充值余额 |
| `granted_balance` | 未过期赠送余额 |
| `topped_up_balance` | 充值余额 |

CNY 与 USD 分开展示，不合并、不换汇。金额按十进制解析后转为 CPA 数值字段。余额接口没有总额度或周期信息，因此不生成百分比和重置时间。畸形响应会明确报错，不会伪装成零余额；接口成功且 `is_available: false` 时仍返回实际余额信息。

虽然查询按凭证选择，数据实际属于 **DeepSeek 账户**。同一账户的多个 key 可能显示相同余额，并共享[账户并发限制](https://api-docs.deepseek.com/zh-cn/quick_start/rate_limit/)。

### 定向解除冷却

充值或临时故障恢复后，只解除目标凭证的本地路由冷却：

```bash
curl -X POST http://localhost:8317/v8/management/routing/cooldown/reset \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"auth_index":"<selected-auth-index>"}'
```

quota 查询保留冷却状态，解除冷却不会改变 DeepSeek 余额或消费。新的 402 或 429 可以再次让凭证进入冷却；冷却时长与重试遵循 CPA v8 原生规则。不支持重置上游余额，quota provider 的 `supports_reset` 为 `false`。

### 验证并刷新凭证

```bash
curl -X POST http://localhost:8317/v8/management/credentials/refresh \
  -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"<credential-filename>.json","auth_index":"<selected-auth-index>"}'
```

静态 API key 没有 token 续期流程。`auth.refresh` 通过余额接口验证选中 key 和账户可用性，保留身份、存储和宿主属性；失效 key 返回 401，余额不足返回 402。余额或限流冷却恢复使用专门的冷却重置接口。

## 排查问题

| 现象 | 检查方式 |
| --- | --- |
| 插件无法加载 | 匹配 CPA 系统和架构，启用两个插件开关，查看 CPA 加载日志 |
| 配置被拒绝 | 检查必填 key、环境变量和[示例](config.example.yaml)支持的字段名 |
| 没有 DeepSeek 模型 | 检查 `/models` 网络和 key 有效性；离线发现需要显式模型声明 |
| HTTP 401 | 检查选中的 DeepSeek key；刷新不会续期或替换失效静态 key |
| HTTP 402 | 充值 DeepSeek 账户，查询新 quota，再重置目标凭证的本地冷却 |
| HTTP 429 | 调整请求节奏并检查账户并发；同账户 key 共用限制 |
| HTTP 400 / 422 | 检查参数、thinking 与工具选择限制、Responses 完整历史 |
| HTTP 500 / 503 或超时 | 检查 [DeepSeek 服务状态](https://status.deepseek.com/)，通过 CPA 策略重试 |

上游故障含义见[官方错误码](https://api-docs.deepseek.com/zh-cn/quick_start/error_codes/)。

## 构建

使用 [go.mod](go.mod) 要求的工具链，启用 CGO，并安装面向目标平台的 C 编译器：

```bash
mkdir -p dist
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/cpa-deepseek.so .
```

macOS 输出路径改为 `dist/cpa-deepseek.dylib`。Windows 在 `CGO_ENABLED=1` 且可用 C 编译器的环境中运行 `go build -trimpath -buildmode=c-shared -o dist/cpa-deepseek.dll .`。

版本元数据默认是 `0.0.0-dev`，可通过 `-ldflags '-X github.com/dillonzq/cpa-deepseek/internal/plugin.pluginVersion=0.1.0'` 注入，或运行 `bash scripts/package.sh v0.1.0`，在 `dist/` 生成当前 Go 目标平台的版本化压缩包。CI 配置了 Linux/macOS 的 amd64、arm64，以及 Windows amd64 构建。构建产物包含中英文 README、配置示例、许可证和图标来源说明。标签构建会额外准备 GitHub Release 草稿，附上五个平台的压缩包和 SHA-256 校验文件，详见[发布流程](RELEASE.md)。

## 测试

```bash
go mod download all
go test ./...
go test -race ./internal/...
go vet -tags debug ./...
go test -tags debug ./...
```

Linux/macOS debug 集成测试通过临时 Go source overlay 将真实动态库加载到 CPA v8 宿主中。本地模拟上游验证所选凭证 quota、402/429 切换、定向冷却恢复、静态凭证刷新、取消和卸载。测试不修改 SDK 模块缓存源码；运行离线 overlay 测试前需下载完整宿主依赖。

默认测试不使用真实 DeepSeek key，也不产生付费调用。真实推理和账户访问需要使用自己的凭证另行验证。

## 官方文档

[首次调用](https://api-docs.deepseek.com/zh-cn/) · [模型发现](https://api-docs.deepseek.com/zh-cn/api/list-models/) · [账户余额](https://api-docs.deepseek.com/zh-cn/api/get-user-balance/) · [Thinking](https://api-docs.deepseek.com/zh-cn/guides/thinking_mode/) · [Responses](https://api-docs.deepseek.com/zh-cn/guides/responses_api/) · [Anthropic](https://api-docs.deepseek.com/zh-cn/guides/anthropic_api/) · [并发限制](https://api-docs.deepseek.com/zh-cn/quick_start/rate_limit/) · [错误码](https://api-docs.deepseek.com/zh-cn/quick_start/error_codes/)

## 许可证与图标来源

插件代码使用 [MIT 许可证](LICENSE)。DeepSeek 图标原样取自[官方 API 文档 favicon](https://api-docs.deepseek.com/img/favicon.svg)。中英文 README 使用[本地 SVG](assets/logo.svg)，CPA 插件元数据使用官方图标 URL；完整来源和权利说明见[图标资源说明](assets/README.md)。本项目为独立集成，不代表 DeepSeek 官方背书。
