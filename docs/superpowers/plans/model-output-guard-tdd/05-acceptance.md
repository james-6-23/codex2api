# 05-Acceptance: 综合验收测试套件、性能指标工具与 Spec §9 完整覆盖矩阵 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建 Codex2API 全局模型输出失控防护体系（Model Output Guard）的端到端综合验收测试套件、性能指标统计工具、长期运行资源泄露门禁与全自动化调度脚本，对 Spec §9 场景 1~28 进行严格真实的规格验收与覆盖率核查，消除历史伪测试并建立严谨的质量防线。

**Architecture:** 
1. 验收测试基础设施与统计学分析库（`test/outputguard`）：跨平台真进程 CPU 计时器（Linux/Unix 采集 `syscall.Getrusage` RUSAGE_SELF 微秒级精确进程 CPU 时间；Non-Linux/Windows 提供独立 build tags 回退），高周期防重 PRNG（LCG 周期 $\ge 2^{63}-1$，无短周期循环），样本校验器，P95 / Median 统计学分析函数及机器判定阈值校验器；
2. 检测层规格验收套件（`security/outputguard`，场景 1~11）：在 `outputguard` 内部测试包中直接对 `RequestGuard`、`ChannelGuard` 与 `RepetitionDetector` 进行黑盒与灰盒边界断言，涵盖事故短周期快速触发、分块不变性（1B/小/大/随机边界）、单大 Delta 嵌入 9215B 周期片段与 8192B 窗口对齐/非对齐即时尾检、多 Channel 状态完全隔离与 ID 别名防重置、硬预算拦截优先级、边界值与多语言 UTF-8、完整快照防逃逸、终态链 8a/8b/8c 成本硬上界（扫描 $\le 2\times\text{limit}$，总校验 $\le 5\times\text{limit}$）、7 类正常样本绝不误报、有意大段重复按规拦截，以及即时尾检放行规则；
3. 生命周期、全协议终止与恢复阻断验收套件（`proxy`，场景 12~20）：全面集成真实路由 Handler 与 Mock Upstream 传输，涵盖全协议 Handler 矩阵（Responses HTTP/WS、ChatCompletions、Anthropic Messages、Gemini Native 流式与非流式及 Realtime 文本子集）、compaction provenance / collector / replay 物理存储前置拦截、立即 Cancel 上游且绝无 5 秒 usage drain、保活注释并发无旧缓冲泄露与零 data race、重试/fallback/续想全链路阻断且上游调用次数严格恒为 1（`calls == 1`）、多协议标准化错误 Envelope 与 Gemini 兼容格式（绝不带 `[DONE]`）、持锁写 Deadline 与 `ErrNotSupported` 安全降级退出、0 账号降分/惩罚且无虚假成功缓存与虚构 usage、以及请求级与 WebSocket Lease 资源强隔离（异常 Lease 立即废弃，正常请求不受影响）；
4. 持久化配置、修订轮询与传输 Framing 门禁验收套件（`proxy`，场景 21~23）：基于真实 SQLite 数据库（`database.New("sqlite", ...)`）验证三态布尔指针存储（`*bool`）、并发更新安全与恢复默认配置；验证 5 秒后台 Revision 轮询、在途请求旧快照隔离与冷启动 DB 故障强阻断（返回 503 `service_unavailable`）；验证语义 Guard 全局关闭（`Enabled = false`）时，传输 Framing 有界读取器依然对超过 8 MiB 的超大无换行数据行与多行 Event 进行硬上限拦截；
5. 性能基准、线性度分析与长跑资源泄露门禁套件（`test/outputguard`，场景 24~28）：在真实端到端完整处理管线中对比 Guard 开启与关闭的 TTFT P95 增量（$\le 10\text{ ms}$）与 CPU 增量（$\le 20\%$），非正基线立即判为无效实验；测试 1/2/4 MiB 纯 CPU 处理线性度（每翻倍中位数增长比值 $\le 2.5$），分别对 Envelope 投影、线性扫描与快照比对进行分项测算；执行持续并发退出与极端细 Delta（4096 细碎 Delta）深度压测，机器严格判定堆内存、Goroutine 与文件描述符（FD）增长趋势；验证敏感内容与凭据在脱敏审计日志与 Proxy 边界外绝不外泄；
6. 自动化测试调度脚本与 Spec §9 场景 1~28 覆盖映射矩阵（`scripts/run_acceptance_suite.sh`）：提供一键调度全量测试套件的可执行脚本，强制开启 `-race`、超时设为 25 分钟、支持长跑环境变量门禁，输出机器与人工均可判定的终态收尾报告。

**Tech Stack:** Go 1.26+, `github.com/codex2api/api`, `github.com/codex2api/auth`, `github.com/codex2api/config`, `github.com/codex2api/database`, `github.com/codex2api/proxy`, `github.com/codex2api/security/outputguard`, `github.com/gin-gonic/gin`, `github.com/gorilla/websocket`, `github.com/tidwall/gjson`, `modernc.org/sqlite`.

**Spec:** `docs/superpowers/specs/2026-10-28-model-output-guard-design.md`

## Global Constraints

- **范围限制**：本项目仅实施端到端综合验收测试套件与相关脚本（涵盖 Task 11.1 ~ Task 11.6）；只交付实施计划 `docs/superpowers/plans/model-output-guard-tdd/05-acceptance.md` 与测试代码；不修改 Spec 规格设计文档、旧 Plan、Candidate、其他模块或业务生产源码。
- **不可妥协的设计取舍与首要原则**：
  - **废除伪测试**：旧“28 全覆盖”表格存在伪造断言（如仅断言 `rec.Code != 0`、局部布尔变量、自构结构体、调用次数容忍 0 等），本模块彻底重写，所有测试必须真实观察底层状态与 side-effects。
  - **真实 Handler 链路**：场景 12/13/16/19/20 必须挂接真实 Handler 并配合 Mock Upstream、物理 Sink、Cache、Health、Usage 及 WS Lease 进行端到端观测；若 04-lifecycle 尚未完成，必须并列提供包含全部真实 Account 字段（`DBID`, `Status: auth.StatusReady`, `UpstreamType`, `BaseURL` 等）的独立测试辅助 Setup 函数，URL 注入必须对应真实路由。
  - **全协议覆盖**：场景 12 必须覆盖 Spec §4.1 全协议组合（Responses HTTP/WS、Realtime WebSocket 文本子集、ChatCompletions、Messages、Gemini Native 流式与非流式）。
  - **重试阻断确定性**：场景 16 中，所有普通重试、Fallback、上下文修复、Compaction 及自动续想分支，上游调用次数必须严格断言为 1（`calls == 1`），严禁大于 1 或容忍 0。
  - **审计与边界安全**：场景 13 必须观察真实存储/Replay Sink 未被写入；场景 19 验证 0 账号惩罚且无虚假成功缓存与 usage 虚构；场景 20 验证异常 Lease 彻底销毁且并发正常请求成功。
  - **性能与基准可信度**：场景 24 必须包含 HTTP 客户端真实接收有效 Delta，若 Baseline CPU 非正则实验无效失败并报错；场景 25 必须验证 1/2/4 MiB 且每倍增长比值 $\le 2.5$，分别核验 Envelope、Scan、Snapshot 分项且检查所有错误；场景 26 验证每 Channel 状态 $\le 64\text{ KiB}$，10 分钟长跑门禁机器自动判定堆/Goroutine/FD 趋势；非 Linux 系统提供专用 Build Tag 回退。
  - **Tests-only 增强验收要求**：在未有实现前运行新验收测试必须呈现可预期的编译失败或 Red 失败；针对前序已实现的包，必须先对 Baseline 进行验证或通过受控突变体（Controlled Mutant）反证，严禁无声明直接变绿。

---
