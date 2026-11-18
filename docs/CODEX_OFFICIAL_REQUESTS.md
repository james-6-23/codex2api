# Codex 官方请求格式兼容

本功能针对 Codex CLI `rust-v0.160.0` 的 HTTP、原生 WebSocket 与压缩 v2 请求格式，提供可选的账号设备标识固定与请求字段保留。实现直接包含在 Go 源码中，使用仓库现有 Dockerfile 或 Release 工作流正常编译即可。

三个环境变量均为可选配置。未配置时保留原有行为；修改后重启应用。环境变量只用于网关内部，不会作为新增字段发给上游。

## 固定账号安装 ID

在管理后台开启账号指纹模式，推荐 `single_machine_multi_window`。然后在 `.env` 中按账号的数据库 ID 配置：

```dotenv
CODEX_ACCOUNT_INSTALLATION_IDS={"1":"11111111-1111-4111-8111-111111111111","2":"11111111-1111-4111-8111-111111111111","3":"22222222-2222-4222-8222-222222222222"}
```

示例中的 UUID 仅供测试。实际部署请用 `python -c "import uuid; print(uuid.uuid4())"` 生成自己的值，并持久保存配置。

- 键是网关账号记录的数据库 ID，不是邮箱或上游 workspace ID。
- 同一上游账号的多个工作区记录可配置同一个 UUID；不同账号使用不同 UUID。
- 显式配置优先于旧的账号自定义安装 ID 头和自动派生值，下游安装 ID 改变不会改变配置值。
- UUID 必须为小写、带连字符的标准 UUIDv4。错误配置会在启动时被拒绝，错误信息不输出配置内容。
- 指纹模式 `off` 时不会改写请求体中的账号身份。未配置的账号继续使用原有身份规则。
- 会话与线程继续按原有规则隔离；固定设备不表示将不同用户的对话合并。
- 已有 `client_metadata` 中的安装 ID 会被改写。没有相应 metadata 的请求不会凭空增加这些字段。

对配置了安装 ID 的账号，最终出站装配会移除独立的 `X-Codex-Installation-Id` 请求头。安装 ID 位于请求体 metadata 内；`x-codex-turn-metadata` 头中的 JSON 投影保留安装 ID，但排除体积较大的 `tool_namespaces_info`，完整信息仍在请求体中。WebSocket 的 turn state 留在每个 `response.create` 帧中，不放进握手头。

## 关闭自动生成的证明

```dotenv
CODEX_DISABLE_GENERATED_ATTESTATION=true
```

开启后，统一 attestation 装配不发送 `X-OAI-Attestation`，包括冻结快照中的值。默认关闭此开关，继续执行原有证明策略。

## 保留官方格式请求字段

```dotenv
CODEX_PRESERVE_OFFICIAL_REQUEST_FIELDS=true
```

仅当 Responses 请求同时具备以下 metadata 时生效：

1. `client_metadata.x-codex-installation-id` 非空。
2. `client_metadata.x-codex-turn-metadata` 是 JSON 对象的字符串表示。
3. 嵌入对象的 `installation_id` 与前述安装 ID 相同，且 `session_id`、`thread_id`、`request_kind` 均非空。

这只识别请求格式，不认证客户端来源；下游仍需正常鉴权。

生效后保留 `input[].id`、`input[].internal_chat_message_metadata_passthrough`；HTTP `/responses` 与 WebSocket 帧不补缺失的 `instructions`，也不自动注入默认图片工具和桥接提示。已有 instructions 和显式工具继续接受正常处理。账号身份映射、缓存隔离、工具配对及其他格式修复继续生效。

压缩 v2 走 `/responses`，同样适用。旧 `/responses/compact` 端点仍保留原有 instructions 补齐行为。普通请求或关闭开关时，仍采用原有字段清理与兼容默认值。

HTTP zstd 是现有管理后台的 `codex_request_compression` 设置，与这些环境变量独立。

## 从源码构建与验证

```sh
docker build --build-arg BUILD_VERSION=dev-official-wire -t codex2api:dev-official-wire .
```

也可以按贡献指南构建前端后执行 `go build .`，或使用 `docker-compose.local.yml` 从源码部署。仓库的 Docker/Release 工作流同样从源码构建，无需在官方预构建镜像上替换二进制。

针对性回归命令：

```sh
go test ./proxy ./proxy/wsrelay -run 'TestOfficialFields|TestOfficialWire' -count=1
```

测试覆盖开关启闭、无配置兼容、无会话 metadata、多个账号与工作区、HTTP 压缩、请求头投影、WebSocket 帧与冻结握手的一致性。所有测试使用模拟 token、本地上游及虚构 UUID。

上述兼容仅涉及请求头与请求体；TLS 握手、HTTP/2 设置和 WebSocket 压缩协商不因此等同于官方客户端。
