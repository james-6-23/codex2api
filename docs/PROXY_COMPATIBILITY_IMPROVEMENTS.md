# Sub2API v0.2.1 兼容实现

本次参考 `Wei-Shaw/sub2api` 的 `v0.2.0..v0.2.1`，沿用 Codex2API 的调度、响应缓存、计费和存储边界。

## 续聊恢复

`previous_response_id is not available for this user` 在 HTTP 错误、WS/SSE 错误信封中均可识别，并进入已有的单次续聊降级流程。错误分类为确定性的 400。匹配限定于错误字段，不扫描用户输入或生成文本。

## Ultrafast

`service_tier=ultrafast` 经 Responses、Chat 转换、HTTP/WS 转发以及 `x-codex-routing-hint` 保留。Codex 配置生成器支持该档位，使用日志分别显示 requested、actual、billing 档位。

Ultrafast 默认采用**本网关当前的 Fast 计费规则**：优先使用模型的 priority 价格覆盖，未覆盖时使用现有 Fast 倍率。这是网关兼容策略，不是对官方 Ultrafast 价格的声明。上游实际降到标准档时，默认计费策略允许降价；上游自行升档不会提高用户账单。

## 失效密文记忆

在共用转发入口记录上游明确拒绝的 reasoning / compaction 密文摘要。相同密文后续再次出现时，发出前删除匹配项，其他历史项和新密文保留。

- 命名空间包含下游身份、会话、上游账号及凭据版本。
- 仅存 SHA-256 摘要；内存状态不跨进程或重启共享。
- TTL 30 分钟，最多 4096 个会话、每会话 256 个摘要。
- HTTP 错误和流内错误都通过受限的旁路读取观察；不预读、不改变响应字节。
- “缺少 encrypted_content”不视为已有密文被拒绝。

## 请求追踪

`usage_logs` 新增 `request_id`、`upstream_request_id`、`upstream_proxy_id`、`upstream_proxy_name`。SQLite/PostgreSQL 均自动迁移，两个 ID 字段具有索引。

网关每个请求生成 ID，HTTP 响应通过 `X-Codex2API-Request-ID` 返回；WS 每轮具有独立 ID。重试沿用本次网关 ID，每次出站重新记录上游 ID 和代理快照。隐藏续想轮保留各自的快照。

默认读取 `X-Request-ID`、`Request-ID`、`X-Goog-Request-ID`。账号快捷配置中的“上游请求 ID 响应头”可覆盖默认名称，也可通过账号 scheduler 更新接口设置 `upstream_request_id_header`。配置不作为出站请求头发送。WebSocket 握手 ID 不写成每轮上游 ID。

使用日志中可查看 ID 和代理信息。管理查询支持 `request_id`、`upstream_request_id` 精确过滤，已有 `q` 搜索也支持两个 ID。代理标签在请求发生时保存，代理后来改名不会改写历史；日志不保存代理 URL 或口令。

## 模型能力快照

成功获取 Codex 模型清单后，将允许的能力字段保存至 `model_capability_snapshots`。每个快照属于一个账号和凭据版本。部分字段缺失时保留该账号此前有效字段；无效或不可用清单不清空快照。旧凭据和较早异步任务不能覆盖新状态。

清单回退按当前 API Key 可访问的账号合并能力：布尔能力取交集、上下文限制取较小值、支持列表取交集，未知或冲突能力保守处理。模型 instructions 等非能力内容不会存入快照。重启会恢复用于 Responses Lite 转发判断的账号能力。

## 重放缓存

相同历史项在本地响应缓存条目之间共享不可变正文；请求重建只复制序列结构。可修改的缓存读取接口仍返回独立副本。淘汰释放缓存持有关系，正在使用的重放仍可安全完成。

原有逻辑字节预算、条目上限、TTL、owner 隔离及 Redis 格式保持兼容。运维 API 的 `response_cache.shared_payload_bytes` 表示缓存持有的去重正文大小；`current_bytes` 仍是用于容量控制的逻辑大小，二者均不等于进程 RSS。

本地缓存基准：128 轮，每轮新增约 10 KiB，缓存写入后读取重放序列；为单独比较复制开销，基准将逻辑预算设为 256 MiB。Apple M5 / darwin arm64 / Go 1.26.6，`-benchtime=5x -count=3`，取各指标中位数：

| 实现 | 每组 128 轮耗时 | 累计分配 | 分配次数 |
|---|---:|---:|---:|
| 修改前 | 158.69 ms | 180,120,702 B | 17,451 |
| 共享正文与复用校验结果 | 31.59 ms | 2,239,427 B | 1,442 |

这是缓存层的本地基准，不是完整上游请求的延迟或生产 RSS 测量。回归测试约束相同场景累计分配低于 12 MiB，并验证副本隔离和淘汰后的读取。

## 验证入口

```sh
npm --prefix frontend run build
npm --prefix frontend run typecheck
npm --prefix frontend test
go test ./...
go vet ./...
go test ./proxy -run '^$' -bench '^BenchmarkResponseCacheReplay128Turns$' -benchtime=5x -count=3
```

`TestPostgresTraceAndCapabilities` 使用 `CODEX2API_TEST_POSTGRES_DSN` 指定的空白临时 PostgreSQL 数据库验证迁移、日志读写和快照恢复。真实上游模型的可用性、Ultrafast 权限及实际计费仍由部署使用的上游决定。
