# Model Output Guard TDD — 中控进度台账

> 主代理（中控）状态文件。每次派发/回调后更新。子代理只读不写（除非另有说明）。

- 分支：`fix/dsh-stream-runaway-guard`
- 基线 commit（代码工作起点）：`39c2cbd2`（docs: add model output runaway guard design specification）
- 基线验证：`GOCACHE=/tmp/go-build GOPATH=/tmp/go go build ./...` = GREEN
- 模块：`github.com/codex2api`，Go 1.26；所有子代理 go 命令统一前缀 `GOCACHE=/tmp/go-build GOPATH=/tmp/go`

## 用户变更（2026-10-31）

- **默认值翻倍**：工具参数字节上限 128 KiB → **256 KiB**（131072→262144，缓解大文件 write 误拦）；请求级事件预算 65536 → **131072**（细 delta 长推理 + 多工具并行下避免事件预算先于字节预算耗尽）。
- 主代理只负责主控，文档（spec+计划文件）修改也全部派子代理（工作区内，无 AGENTS.md 限制）。
- 不受影响的同值参数：通道字节下限 MinChannelBytes=131072、deltas 校验上限 65536（256..65536）、任意测试数据（delta1 65536 数组、toolBytes 65536、variantParam 下标）。

## 计划文件 → 任务映射

| 计划文件 | 任务 | 主要文件 | 依赖 |
|---|---|---|---|
| 01-core.md | T1–T3 | security/outputguard/{repetition,channel,guard}*.go | — |
| 02a-database.md | T4 | database/output_guard_settings*.go + sqlite.go + postgres.go | — |
| 02b-runtime.md | T5 | proxy/output_guard_settings*.go + main.go | T4 |
| 02c-admin-ui.md | T6 | admin/output_guard_settings*.go + admin/handler.go + frontend/** | T4+T5 |
| 03-protocol.md | T7 | proxy/outputguard_projection*.go | T3 |
| 03-protocol.md | T8 | proxy/sse_line.go + sse_line_test.go + executor.go | — |
| 04a-termination.md | T9a+T9b | proxy/handler.go(streamOutcome) + stream_flush_writer.go + outputguard_cancel/downstream | T3+T7 |
| 04b-responses-chat.md | T10A | proxy/handler.go + outputguard_http_chat_test.go | T5/T7/T9 |
| 04c-native-http.md | T10B | proxy/outputguard_native_adapters.go + handler_anthropic.go + handler_gemini.go + handler.go(Grok) | T10A（handler.go 互斥） |
| 05a-detection-config.md | T11.2+T11.4 | S 验收测试(1-11) + P 验收测试(21-23) | 11.2←T3；11.4←T4+T5+T8 |
| 05b-lifecycle-acceptance.md | T11.3 | P 验收测试(12-20) | T10A+T10B |
| 05c-performance.md | T11.1+11.5+11.6 | proxy/perfstat/ + acceptance_benchmark_test.go + scripts/run_acceptance_suite.sh | T10B |

## 派发规则

1. 文件所有权硬规则：同文件编辑=硬冲突（串行）；同包不相交新文件=软冲突（60s 重试协议）
2. 审查代理 glm-coding/glm-5.3-flash high，并发≤1，只读+运行测试
3. 单任务原则：子代理完成即归档，修复须新建
4. 全局同时运行 ≤5；禁止轮询，等回调
5. 计划/spec 文件修改也派子代理（工作区内）；代码 commit 只含代码文件

## 已完成

| 任务 | 子代理 | commits | 状态 |
|---|---|---|---|
| T1-T3 核心 | aebcc6a8 (W1) | 2d388cca, 9cdf21d4, 0087d69c | GREEN（24/24 -race；注：Task1 commit 因 index 预暂存带入了 21 个 docs 文件；01-core.md 复选框已勾选未提交） |
| T4 数据库 | fc07f2f5 (W2) | ecc4385e | GREEN（8 顶层测试 PASS，postgres 无 DSN 跳过；偏差：migrate 函数名/DDL 插入点/尾逗号） |
| T8 framing | 26221eba (W3) | fcced5b3 | GREEN（7 用例 + 回归 11 用例 + build；03-protocol.md T8 复选框已勾选未提交） |

## 运行中（Batch 2，2026-10-31 07:19 派发，5/5 满员）

| 代号 | subagent id | 任务 | 模型 | 状态 |
|---|---|---|---|---|
| W4 | 8d72a670-e873-454b-ae64-839e322f7926 | T5 运行时（02b）+ 02b stale 值上抬 524288 | deepseek-official/deepseek-flash | **GREEN**（44647814；proxy 6/6 + database 489 PASS；02b 5 步勾选未提交） |
| W5 | c2684a24-fedc-4c62-a8c2-d5113de1ce00 | T7 投影层（03-protocol Task 7 节） | deepseek-official/deepseek-flash | **GREEN**（f61d2690；899+548 行；16/16 PASS 含 131073 事件回归；03-protocol Task 7 勾选未提交） |
| W8 | f6a5903a-3ffd-4c72-b35f-98e9fbee17e4 | T9 终止协调器（04a Task 9a+9b） | deepseek-official/deepseek-flash | **GREEN**（b2166060；6 文件；11/11 + 额外回归 PASS；04a 10 步勾选未提交） |
| W9 | 966fe3dc-60d3-4d5c-8f8c-40d18414c699 | T10A Responses/Chat 接线（04b Task 10A；proxy/handler.go + 新测试） | deepseek-official/deepseek-flash | **GREEN**（f1a93458；handler.go +113/-3 + 427 行测试；RED 5 FAIL→8 RUN PASS + 48 聚焦回归 + 整包 165s PASS；04b 5 步勾选未提交） |
| W11 | 443de2f3-0495-4599-b0c4-66417ff78985 | T11.4 交叉验收（05a L1060-1683，gen2Bytes=524288 已修正） | deepseek-official/deepseek-flash | **GREEN**（fe8161bb；database/acceptance_config_test.go 306 行 + proxy/acceptance_config_test.go 301 行；场景 21-23 PASS；05a 3 处勾选未提交；db.Close 非幂等已用 t.Cleanup 修复） |
| W7 | d1772fc6-223a-4969-9706-b903edf6a153 | T6 Admin API+前端（02c Task 6；admin/ + frontend/src/） | deepseek-official/deepseek-flash | **GREEN**（c4a766c4；9 文件；admin 1021 + database 490 + 前端 7/7 + tsc 0 错；RED 先验证；02c 以 Progress 清单代替复选框） |
| W6 | 7477f1cc-f985-444e-956f-af802bafd629 | T11.2 检测验收（05a L190-1059）+ 05a:1506 stale 修正 | deepseek-official/deepseek-flash | **GREEN**（21383575；36 顶层+32 子测试 PASS -race；05a 11.2 复选框已勾，11.4 gen2Bytes→524288 含 2 处依赖断言同步） |
| WF-docs | 60b8b6e1-bed5-4dff-bc0f-c624c7bec32a | spec:140 64→16 字节口径 + 主计划/candidate ~L2165/~L2214 stale 值 →524288 | deepseek-official/deepseek-flash | **GREEN**（spec:140 修正；主计划 L2172-2261 与 candidate 对应区域 stale/gen2/依赖断言 →524288，LKG 默认断言保留 262144；与 02b 语义一致；未提交） |
| FW1 | f5455cb3-efb3-4e8a-b96b-0db49a6715ca | framing 修复 3 findings（executor.go/sse_line.go + 回归测试） | deepseek-official/deepseek-flash | **GREEN**（4522ee05；TDD 证据完整；13 用例 PASS；曾遇 W4 并发噪声，60s 重试自愈） |
| R2 | 04144a0c-bd38-4f28-b362-accda88fba4e | 审查 W1 核心（2d388cca..0087d69c，/tmp worktree 隔离验证） | glm-coding/glm-5.3-flash high | **APPROVED，零 findings**（24/24 -race、vet/gofmt 干净、与 W1 时点计划逐字一致；仅测试函数命名偏差 LOW 可接受；2 个非缺陷观察已记录） |
| R3 | f79fadbe-e6fb-447f-868a-c9d41aba2c25 | 审查 W2 数据库（ecc4385e + WD2 b013ce1a，worktree @b013ce1a 隔离） | glm-coding/glm-5.3-flash high | **APPROVED**（667 PASS -race；2 个 LOW 不阻塞，见下） |
| R4 | e7cf6852-a5dd-4ca9-81c5-27c2847d42a3 | 审查翻倍专项（a9af1bdb+b013ce1a 代码 + 11 文档未提交改动 + 524288 判断 + spec:140 口径） | glm-coding/glm-5.3-flash high | **APPROVED，零 findings**（代码/文档 grep 矩阵全部分类合法；算术一致：16 字节、256 GiB、131073 十处、||256、262145；524288 四处语义一致；后续 commit 无回退） |
| R5 | d6d3d11d-7126-4d6b-a499-87c734971ad8 | 复审 FW1 framing 修复（4522ee05，3 findings + 回归测试 + HEAD 回退检查） | glm-coding/glm-5.3-flash high | **APPROVED**（3 修复全验证、13 用例隔离 PASS、HEAD 无回退；1 个 pre-existing LOW 入 backlog：多行事件 join 分隔符未计入事件 cap，极端 payload 2×，内存仍有界） |
| W13a | ea800da1-93e5-4f63-8c4a-f585bb9b23a1 | T11.1 perfstat 统计工具包（05c L45-516，新建 proxy/perfstat/ 4 文件） | deepseek-official/deepseek-flash | **GREEN**（25c065e3；4 文件 397 行；6 测试 + Linux RUSAGE 真计时 PASS；05c 5 步勾选未提交） |
| R6 | bda8b22d-c995-4e59-987b-45a2db9ec691 | 审查 W4(44647814 T5 运行时) + W5(f61d2690 T7 投影层)，worktree 隔离 | glm-coding/glm-5.3-flash high | **APPROVED**（22/22 隔离 PASS；1 个 LOW：ResponsesProjector/ChatProjector 的 aliasMap 声明+写入但从未读取（计划自带死代码），最终整理时确认/删除） |
| R7 | 9fa17b38-d2ed-4b1b-b485-3f497427d8e8 | 审查 W6+W11 验收测试批（worktree @fe8161bb） | glm-coding/glm-5.3-flash high | **APPROVED**（68+场景21-23 全 PASS；2 项受控突变反证；4 LOW 入整理清单⑤） |
| R8 | 0235bfaf-c2e9-4485-93b7-8c39805ee4a9 | 审查 W8 终止协调器（b2166060，worktree 隔离） | glm-coding/glm-5.3-flash high | **CHANGES_REQUESTED**（1 MED：DiscardBuffered 与并发 Flush 无同步、保活 goroutine 真实交错最坏 panic；3 LOW：OverlayGuardOutcome 4 残留字段未清零、WS 并发写契约注释缺失、DeadlineSuccess 未验 5s 真实超时）→ 派 FW2 修复（LOW4 入 backlog） |
| FW2 | fe85320c-252d-47f0-b5af-49bd3fa6f3e5 | R8 修复：stream_flush_writer 并发自包含方案（atomic.Bool+mutex）+ OverlayGuardOutcome 清零 + WS 契约注释 | deepseek-official/deepseek-flash | **GREEN**（3ea512c5；atomic.Bool CAS 先置位 + mu 统一保护 buffer/scanCarry/outputScanner/lastFlush，锁内快照清空锁外写下游 + 写前 aborted 二次检查，API 零变更；新增 50×200 并发 -race 用例 + 慢下游不阻塞用例；18/18 聚焦 + 整包 PASS（3 个无关抖动已基线验证）） |
| R9 | a28edabf-a312-4434-8e23-18eb213df439 | 审查 W7(c4a766c4 admin+前端) + W13a(25c065e3 perfstat) | glm-coding/glm-5.3-flash high | **APPROVED**（admin 2 测试 + perfstat 7 + 前端 417/417 + tsc 0 错；2 LOW 入整理清单：perfstat ErrInvalidBaseline 无消费者、Fill() 低位字节 LCG 周期 256——已通知 W13b 重复检测 fixture 改用 GenerateASCII/高位） |
| W13b | 9b726761-64ef-4362-b689-ec301c58bac9 | T11.5+11.6 性能基准门禁 + 调度脚本（05c L517-1205，新建 2 文件） | deepseek-official/deepseek-flash | **GREEN**（e98def4f；673 行测试 + 28 行脚本（scripts/ 被 gitignore，add -f）；5 场景 + 整包 207s + runner smoke 全 PASS；05c 11 步勾选未提交） |
| W10 | 4ebd7c3f-0495-4c10-91f1-c3de9fa89c73 | T10B Native HTTP 接线（04c Task 10B；native adapters + handler_anthropic/gemini + handler.go） | deepseek-official/deepseek-flash | **GREEN**（58a30bab；5 文件；6 新用例 + 整包 171.9s PASS；偏差 8 条均合理（条件门禁复用 W9 约定、Grok/Claude 先提交 200 走已提交分支 envelope、Gemini 裸 JSON 502）；04c 无复选框同 02c 惯例，未改；FW2 未提交改动已选择性排除） |
| W14 | a25e121f-a371-49f2-8a76-86cc345bec8b | OI-W9-1：Responses relay-style 分支接线（handler.go；与 W10 硬冲突，串行） | deepseek-official/deepseek-flash | **GREEN**（e8c5ed63；handler.go +61/-14 五处接线 + 175 行测试 3 用例；3/3 聚焦 PASS；调研确认 relay 分支独立于标准循环与 Grok 原生透传；非流式走 snapshot 路径；BaseURL 直连（relay 不走 Resin）；整包仅既有 image-pipeline 抖动） |
| OPT1+2 | 89a6e74c / a233a26f | CPU 门禁优化（repetition.go 早退+零分配+分段批量；场景 24 恢复 1MiB） | codex-proxy/gemini-3.8-flash-high high | **GREEN**（4e52485d；1MiB 增量 62.59%→5.41% 中位数；全闸门 PASS；OPT1 未留报告异常由主代理核验 diff 后由 OPT2 收尾提交） |
| W12 | 89e0def1-fd74-4fa2-88f6-49792c365bb4 | T11.3 生命周期/全协议终止验收（05b 场景 12-20，新建 acceptance_lifecycle_test.go） | codex-proxy/gemini-3.8-flash-high high | **GREEN**（e332d4d0；1372 行 18 用例 PASS；场景 12-20 覆盖矩阵完整；3 个 WS 入站 skip 按 spec 标注（生产未接线→W16 补齐）；整包 PASS） |
| R10c | 40a23d28-699f-4ac1-bd3d-59de58db50d6 | 审查 W12 生命周期验收（e332d4d0 vs 05b Task 11.3 + spec §9.2） | glm-coding/glm-5.3-flash high | **CHANGES_REQUESTED**（1 HIGH：场景 19 恒真伪断言（自构 penalize:false → reportStreamOutcomeFailure 永不调用、HealthTier/DB usage 断言空转）违反 05b 全局约束；1 MED：场景 17.4 WS_Terminal_And_Lease_Destroyed 名不符实+弱断言+与既有原语测试重复；3 LOW：5s deadline 未断言数值、场景 12 多处 or 弱化、场景 13 err 被丢弃。修复配 FW3（acceptance_lifecycle_test.go 与 W16 硬冲突 → 排 W16 后）） |
| W16 | a70ec105-1d38-4def-a574-d70b98875dbc | WS 入站 guard 接线（responses_ws/realtime_ws 补 ObserveEvent+终止，解锁 3 个 skip 用例） | codex-proxy/gemini-3.8-flash-high high | **GREEN**（3b039d5e；responses_ws.go +33、realtime_ws.go +6、acceptance_lifecycle_test.go +34；18 用例全绿 0 skip、整包 167s PASS；中途曾被外部中止后由用户唤醒续跑完成） |
| FW3 | 30e2ca02-5d4e-4c14-86cf-3c6bed9a6ea3 | 修复 R10c：场景 19 恒真伪断言（HIGH）+ 17.4 更名收紧（MED）+ 3 LOW | codex-proxy/gemini-3.8-flash-high high | **GREEN**（9317b6ad；acceptance_lifecycle_test.go +173/-44；18/18 lifecycle PASS、整包 PASS、gofmt/build 干净；F1 突变自证（注入 ReportRequestFailure → FailureStreak==0 got 1 FAIL）） |
| R12 | 3ed8d343-cf8b-4109-afe4-05ed6214423b | 终审 W16(3b039d5e WS 接线) + FW3(9317b6ad 断言修复) | glm-coding/glm-5.3-flash high | **CHANGES_REQUESTED**（1 HIGH：responses_ws.go:1281 guard 命中 SendDownstreamGuardTerminalWS 未取 responsesWSWriters per-conn 锁→多 lane 并发写违反 gorilla 契约可致帧损坏/panic；1 MED：close code 漂移（真实 1008 vs 17.4 断言 1013 vs 场景 12 接受 1000/1008/1011/EOF）→ 决策统一 1013；2 LOW：场景 19 前置断言或化、reason 形参冗余。F1 突变实证成立、29 子测试 0 skip）→ FW4 |
| FW4 | 3d522af0-a036-4eba-9e64-aad1f2874210 | R12 修复：WS 终态写 per-conn 锁 + close code 统一 1013 + 场景 12/19 收紧 + 同 conn 双 lane 并发回归 | deepseek-official/deepseek-flash high | **GREEN**（68dd0cb7；helper sendResponsesWSGuardTerminal 持 per-conn 锁、外层 close 改 1013、assertWSGuardTerminalThen1013 精确断言、双 lane 并发用例；反证：回退 helper → 稳定 FAIL+gorilla concurrent write panic；targeted 49 用例 PASS；整包仅既有环境性失败 TestChatCompletionsStreamKeepsDownstreamAliveDuringUpstreamSilence（HEAD 亦复现）） |
| R13 | 1b877f38-cf64-42b0-b7bc-e8b7012dd6ca | 复核 FW4（68dd0cb7 锁修复 + close code + 双 lane 用例） | glm-coding/glm-5.3-flash high | **APPROVED**（85 用例 0 FAIL、双 lane -count=3 PASS、回退反证 panic 实证；唯一 MED=工作区回退残留（非 commit 缺陷）→ 主代理已 `git checkout` 恢复并复跑双 lane 用例 PASS） |
| OPT5 | c8a97fae-8473-4b19-a0b6-c80087d446bf | 40 并发空闲机干净复测（deepseek-flash 路由；guard on/off 交替 5 轮 + 1MiB 变体 + loadavg 记录） | deepseek-official/deepseek-flash high | **完成**（64KiB：on 中位 619.77ms / off 619.29ms → 净增 **+0.48ms/轮 (+0.08%)**、+0.012ms/请求；1MiB：净增 -36.86ms (-1.26%)；两档净增量均远小于轮间抖动 151-363ms → **40 并发 guard 净 CPU 增量已落入噪声**；320/320 请求 200 零误杀；命中用例 31 delta 精确终止；机器空闲 loadavg 2.4-2.7） |
| 长跑26 | bash-1e02ac9b718541ff | 场景 26 资源泄露 10min 长跑（RUN_OUTPUTGUARD_LONG_RUN=1） | 主代理后台 | **PASS**（600s soak：GoroutineDelta=0、HeapGrowth=59536B、FDDelta=0；单 channel 状态 33936B ≤ 64KiB） |
| R10a | 9f1fb07c-846e-44de-a4ab-076e766c727d | 审查接线三连（W9+W10+W14）+ repetition 早退数学等价性（4e52485d） | glm-coding/glm-5.3-flash high | **APPROVED**（guard 16 用例 -race PASS；早退数学独立推导成立、checkpoint 序列严格一致；4 findings 均范围外缺口：①MED WS ingress/realtime 未接 guard（backlog）；②LOW compact 透传 2 写点未观察；③LOW live sideband 盲代理；④LOW 死代码 handleStreamResponse/handleCompactResponse 待删） |
| R10b | e0dad2b2-aadf-4449-b80c-8257948f1308 | 审查 FW2(3ea512c5)+W13b(e98def4f)+OPT(4e52485d 实现质量) | glm-coding/glm-5.3-flash high | **APPROVED**（16 用例 -race PASS；场景 24 复跑 5.03%；FW2 并发安全完备、OverlayGuardOutcome 清零正确、基准质量与 05c 参考一致、OPT writeChunk 环形/checkpoint 推进正确；5 LOW 入整理清单） |
| OPT3 | d1448c03-91fe-46cc-ab49-fb7f37ba2149 | 40 并发 CPU 消耗评估（隔离 worktree @4e52485d，纯测量不提交） | codex-proxy/gemini-3.8-flash-high high | **完成**（40 并发 guard on 64KiB：CPU 151.44ms/轮、3.79ms/请求、利用率 53.69%；guard 纯增量 +2.88%；扩展性 34.97×/40× 亚线性；触发路径 136.71ms；-race 完成率 100% 无报警；测量期间 W12 并行致 headline 负增量噪声） |
| OPT4 | dd8db781-615e-4ad2-980d-87700ff7507d | 第二轮剖析驱动优化（40 并发基线复测 + pprof 差量 + per-event 热点优化 + 全闸门） | codex-proxy/gemini-3.8-flash-high high | **GREEN**（54f1093c；pprof：ObserveEvent 7.73% cum 为最大 guard 热点、Push 仅 0.52%；优化 gjson.GetManyBytes 单遍历 + 零拷贝 extractJSONStringPayload + RequestGuard 通道解析缓存；单流场景 24 增量 5.41%→**1.23%**；40 并发净增量中位数 -1.41%（loadavg 4.53，噪声带内）；场景 25 线性度 PASS；全闸门 PASS） |
| W15 | b895babd-630d-4965-aa71-1f611c2ba427 | 默认关闭改造（enabled 默认 true→false：database 常量/DDL/COALESCE、proxy fallback、admin 默认、frontend 默认与测试、spec+02a/02b/02c 文档同步） | codex-proxy/gemini-3.8-flash-high high | **GREEN**（82049878；7 代码 + 6 测试文件；database/admin/-race + frontend 417/417 + tsc 0 错 + build/gofmt PASS；docs 4 文件未提交） |
| R11 | b5a78c91-ce28-4069-9d79-5362312b907c | 审查 W15 默认关闭改造（82049878 代码闭环性 + 遗漏扫描 + 文档口径） | glm-coding/glm-5.3-flash high | **APPROVED**（默认值来源全闭环 false、三态/门禁语义保持、真实覆盖；2 LOW：acceptance_config_test.go:15/:43 注释仍写默认开启；另发现 spec:132/§173-189 tool bytes 128KiB 漏翻倍、candidate:1707/:2805 默认关闭未同步 → 入 W17） |
| W17 | 5b1f5cfd-7d21-4321-9721-3d64ad5f3ecb | 文档最终同步（spec tool bytes 128→256 KiB 全链派生、candidate 默认关闭、R11 2 注释、02c 128*KIB、03-protocol Task 8 同步 FW1、04c Progress 清单） | codex-proxy/gemini-3.8-flash-high high | **GREEN**（7 文件未提交；grep 复核：spec 仅余 L291 通道下限 128 KiB 合法、candidate DEFAULT TRUE 归零、02c 仅余 MIN 通道下限、03-protocol Task 8 已对齐 max(len(data),1)+clamp） |
| Tidy-A | 15c470cb-f795-4ace-acf8-1a64e7dae837 | 整理清单 A（security/outputguard + scripts + handler.go 死代码）：场景 1 下界断言、nextNeeded 去重、ErrInvalidBaseline、脚本强断言、边界接受方向测试、死代码删除 | codex-proxy/gemini-3.8-flash-high high | **GREEN**（2c14c49c；子代理未留报告未提交，主代理验证：outputguard 1.467s/perfstat 1.012s -race PASS、gofmt 干净、bash -n OK、handler.go 仅删 69 行死函数、go build ./proxy PASS → 代为提交 6 文件 +126/-95） |
| Tidy-B | 5f57c8af-5a44-48b4-b072-6040440c9694 | 整理清单 B（proxy stream_flush_writer + database）：三处 aborted 二次检查、跨 goroutine 契约注释、scanner 并发变体、SQL 字面量绑定、PG skip 重构、candidates 去重、rc 终值断言 | codex-proxy/gemini-3.8-flash-high high | **GREEN**（3755589e；4 文件；database 121.9s -race PASS、scanner 并发变体 PASS、PG SKIP 在 -v 可见；B4 采用 fmt.Sprintf 注入常量强绑定） |

### Batch 1.5 完成记录（07:15 前后回调）

| WD1 文档翻倍 | 62fcf76d | spec 27 处 + 10 计划文件共 314 处 | GREEN（残留旧值全部属"不得改"类；git diff --check 通过） |
| R1 framing 审查 | 63c1f3a2 | verdict=CHANGES_REQUESTED（1 MED + 2 LOW） | 完成 → FW1 修复中 |

### R1 findings（FW1 修复中）
1. [MED] executor.go:1647-1659 空 `data:` 行 `currentEventBytes += 0` 绕过事件累计硬 cap（dataLines 无界；"data:\n"×12M 实测 12MB payload 未拒）→ 改 `+= max(len(data),1)` + 回归测试
2. [LOW] sse_line.go:55 ReadBoundedResponseBody limit=MaxInt64 时 limit+1 溢出 → clamp
3. [LOW] executor.go:1687-1689/1727-1729 两处 errSSELineTooLong 检查数学不可达 → 加深防御注释
（test_evidence：7/7 + 4/4 回归 PASS，build/gofmt 通过）

### WD1 open_issues 分派
- 02b:225/229/237/259/272 stale/gen2 = 262144 与新默认相同 → **W4** 处理（上抬 524288）
- 05a:1506 同类 → **W6** 处理
- spec:140 "平均每个事件 64 字节"应为 16（2MiB÷131072）+ 主计划 ~L2165 + candidate ~L2214 stale 值 → **待派 WF-docs**（等空位，当前 5/5 满员）
- 03-protocol Task 8 参考代码与 FW1 修复的同步 → 最终计划同步时处理

### WD2 代码翻倍已完成（07:05 回调）

- commits：`a9af1bdb`（outputguard）+ `b013ce1a`（database）
- 验证：core 24/24 PASS(-race)；database 477/477 PASS(-race)；go build ./... PASS
- 偏差（合理）：sqlite.go 实际 4 处该列 DEFAULT 131072（含增量列迁移列表）全部改为 262144；guard_test.go 同步 2 处 128KiB 注释；database 测试 L231 `int64(131072+(idx+1)*1024)` 属任意更新值未动
- 已解锁并派发（Batch 2）：W4/W5/W6 + FW1 + R2

## 审查队列

- [x] R1：W3 framing（运行中）
- [x] R2：W1 核心 — **APPROVED 零 findings**（唯一 LOW：并发测试函数命名与计划不同，内容相同，不改）
- [x] R3：W2 数据库 — **APPROVED**（2 个 LOW 记入最终整理清单：① SQL 字面量 262144 与 Go 常量无编译期绑定，建议 fmt.Sprintf 注入或同步注释；② 缺边界端点"接受"方向测试）
- [x] R4：翻倍专项 — **APPROVED 零 findings**
- [x] R5：FW1 framing 修复复审 — **APPROVED**（1 个 pre-existing LOW 入 backlog）
- [x] R6：W4+W5（T5 运行时 + T7 投影层）— **APPROVED**（22/22 隔离 PASS；1 LOW：aliasMap 死代码）
- [x] R7：W6+W11（T11.2 + T11.4 验收测试批）— **APPROVED**（outputguard 68 + 场景 21-23 全 PASS；2 项受控突变反证证明断言非恒真；4 LOW 见下）
- [ ] R7：W6+W11（T11.2 + T11.4 验收测试批）— 待 R6
- [x] R8：W8（T9 终止协调器）— **CHANGES_REQUESTED**（1 MED + 3 LOW；11/11 隔离 PASS）→ FW2 运行中 → 已闭环（3ea512c5）
- [x] R9：W7+W13a — **APPROVED**；R10a：接线三连+早退数学 — **APPROVED**（范围外缺口：WS ingress MED 入 backlog→W16、死代码入整理清单）；R10b：FW2+W13b+OPT — **APPROVED**（5 LOW 入整理清单）；R10c：W12 生命周期验收 — **CHANGES_REQUESTED**（1 HIGH + 1 MED + 3 LOW → FW3，排 W16 后）
- [ ] R9：W7（T6 Admin+前端）∥ W13a（T11.1）— 待 R8
- [ ] R10：W9/W10/W12/W13b 批次 — 待 R9
- [ ] 后续各批编写完成后按序入队

## 环境约定（03:45 更新）

- 所有 go 命令统一前缀：`GOCACHE=/tmp/go-build GOPATH=/tmp/go GOTOOLCHAIN=go1.26.10 GOPROXY=https://goproxy.cn,direct`（用户 03:45 指示固定使用 goproxy.cn 代理；/tmp 缓存可能被清理导致每次重新下载工具链/模块，GOPROXY 保证下载稳定）。
- /tmp worktree 创建→验证→清理需在同一 bash 调用内完成（/tmp 可能跨调用不持久）。

## 模型路由变更（21:29 / 03:31）

- 21:29 起 writers = **codex-proxy/gemini-3.8-flash-high** @ high（已执行至 FW3 之前所有 gemini 任务）。
- **03:31 起 writers 改为 deepseek-official/deepseek-flash @ high**（正在运行的 FW3 为已派发任务，保持原路由跑完；其后新派发的编写子代理一律用新路由）。审查员保持 glm-coding/glm-5.3-flash @ high。

## CPU 优化任务（21:35 用户指示"先优化"）— ✅ 完成

用户选择 OI-W13b-1 处置方案 = **优化实现**。最终 **commit 4e52485d**（OPT1+OPT2 合并提交，2 文件 +97/-44）：
- **结果**：场景 24 @1MiB CPU 增量 **62.59% → 5.41%**（5 次独立采样中位数：3.73/3.13/10.48/7.54/5.41%），远低于 Spec §9.3 20% 门禁与 16% 内部目标；场景 24 恢复计划 1MiB 口径（256×4KiB）。
- **技术**：① checkCurrentWindow——linear/pi 复用字段零堆分配（pi [8192]uint16，struct +24KB 仍过 64KiB 体积闸门）、ring 两段 copy 展开、KMP 前缀遗传性早退（pi[i] < i+1-256 即判非重复，正常数据 ~300 迭代中止）；② Push/PushWithHash——checkpoint 对齐分段批量推进（writeChunk 双段 copy、局部紧循环 FNV、段尾统一更新游标），消除 -race 下每字节 6-8 次 struct 字段插桩访问。
- **语义零变化**：checkpoint 触发时机字节级等价；outputguard 全包 + 场景 1/2/3/8/11 等价闸门 + 场景 24×5 + 场景 26 + proxy 整包全 PASS；gofmt/vet 干净。
- 遗留：pprof 证据在 /tmp/scenario24.pprof（未入库）；4e52485d 纳入 R10 审查批（repetition 语义敏感，建议 R10 重点核早退数学等价性）。

## 40 并发 CPU 评估（23:25 OPT3）

32 核 / GOMAXPROCS=32 / 7.8GiB。隔离 worktree @4e52485d，in-process httptest 真实 Responses 管线，每点 5 轮中位数：
- **扩展性（guard on, 64KiB/请求）**：并发 1/5/10/20/40 → 总 CPU 4.33/28.80/33.30/69.28/151.44ms，CPU/请求 4.33/5.76/3.33/3.46/3.79ms，利用率 6.02/19.05/30.39/46.05/53.69%；40 并发 CPU 为单请求的 **34.97×**（<40×，亚线性）。
- **guard 纯增量**：交替多轮 +72.61ms（+2.88%）；40×64KiB 载荷下约 +1.8ms/请求。headline 单次对照出现 -34.35% 负增量，系测量时 W12 整包测试并行占用 32 核所致（噪声）。
- **1MiB/请求 40 并发**：总 CPU 132.00ms、CPU/请求 3.30ms——固定每请求开销（配置快照/路由/鉴权）主导，载荷大小非主导项。
- **触发最坏路径 40 并发**：8.1KiB 处准确拦截，全程 CPU 136.71ms（低于正常完整流），cancel/终止/envelope 链路无额外尖峰。
- **-race 40 并发**：ON 4918.65ms vs OFF 3043.04ms（+62%，竞争检测运行时放大，非生产工况），完成率 100%、零 data race 报警、零死锁。
- 资源：goroutine 峰值 306（触发阶段短暂 551）、堆稳态 ~100.16MB、全部场景完成率 100%、正常流零误杀。
- **待办**：空闲机（W12 完成后）复测一次干净基线，确认 guard 纯增量。

## 默认关闭改造（23:52 用户指示）

用户要求 guard **默认关闭**（enabled 默认 true→false）。已定位影响面：`database/output_guard_settings.go:12 DefaultOutputGuardEnabled=true`、DDL enabled 列默认值、COALESCE 默认（:55/:119）、proxy fallback 构造、`admin` GET 默认断言、`frontend/src/lib/outputGuardSettings.ts:21 output_guard_enabled: true`（DEFAULT_OUTPUT_GUARD_SETTINGS）、Settings.tsx 警告逻辑与测试、文档：spec:131/:293/:347、02a:20/:141、02c:198。派 **W15** 实施（代码+测试提交、docs 保持未提交）。冷启动就绪门禁语义不变（与 enabled 无关）。

## 40 并发 CPU 评估（23:25 OPT3）+ 二次优化（00:43 OPT4 54f1093c）

OPT3 初步数字见上（受 W12 测试干扰）。OPT4 以 pprof 实证重构后：
- **guard-on 独有热点**：ObserveEvent 7.73% cum（Project 6.02%，gjson 重复解析/string 构造为主）> RequestGuard.resolveChannelLocked 0.45% > RepetitionDetector.Push 0.52%（扫描已非瓶颈）。
- **优化**：gjson.GetManyBytes 单遍历、零拷贝 extractJSONStringPayload、RequestGuard 通道解析缓存。
- **结果**：单流场景 24（-race）CPU 增量 **1.23%**（5.41%→1.23%），TTFT 0 退化；40 并发净增量中位数 **-1.41%**（5 轮 ±10% 噪声带内，loadavg 4.53——噪声已大于信号）；场景 25 线性度 1/2/4MiB = 2.13×/1.95× PASS；outputguard 全包 + proxy 整包 + build/vet/gofmt 全 PASS。
- **结论**：40 并发下 guard CPU 开销已落入测量噪声（<±1% 量级真实增量），进一步优化收益低于噪声；如需更精确结论需在完全空闲机做高精度计数（如 perf stat 指令数）。
- 待办：最终整合时在空闲机复测一次 40 并发净增量（当前 3 个子代理并行，loadavg 4.5+）。

## 最终验证（05:04）

- `go build ./...` **PASS**；gofmt 改动文件零输出。
- 全包 `go test -race -count=1 ./...`：**21 个包 PASS**（含 database 157s、admin 124s、outputguard 1.6s、perfstat、wsrelay、proxy/…），proxy 包仅 1 个失败：`TestChatCompletionsStreamKeepsDownstreamAliveDuringUpstreamSilence`。
- **该失败为既有环境性失败，非本分支回归**：在分支基点 `dfab070b`（v3.1.2，全部 guard 改动之前）worktree 复跑同一用例同样 FAIL。机制：请求级 keepalive 5ms 间隔 + 本机 Resin/代理延迟使"首字前窗口"超过 5ms，keepalive 落在首字前；与 guard 无关（该测试期望首字前零字节）。
- 长跑场景 26（10min soak）PASS：GoroutineDelta=0、HeapGrowth 59536B、FDDelta=0。
- 40 并发干净复测（OPT5）：guard 净增量 +0.48ms/轮（+0.08%，64KiB）/ -36.86ms（1MiB），均落入 151-363ms 轮间噪声；320/320 请求 200、零误杀。
- CPU 优化终值：单流场景 24（-race）增量 62.59% → **1.23%**。

## 已知限制与 backlog（不在本次范围）

1. R5 既有项：多行事件 join 分隔符未计入事件 cap（pre-existing，内存仍有界，计划参考口径一致）。
2. R9 既有项：perfstat.Fill() 低位字节流周期 256（重复检测 fixture 已改用 GenerateASCII 规避）。
3. R10c/R8 观察项：SendDownstreamGuardTerminalWS 的 reason 形参冗余（脱敏正确）；
4. 场景 18 DeadlineSuccess 未构造慢消费下游验证 5s 真实超时（集成测试级，既有无变化）。
5. 既有图像管线/keepalive 时序测试在部分机器上偶发（非本分支引入，已按基线排除）。
6. WS 命名 lane 的 guard 命中沿用既有 multiplex 语义：仅终止所属 lane（error 帧 + 该 lane 收尾），1013 整连接关闭仅用于默认 lane 与 realtime 连接。

## 暂停/恢复记录（2026-10-31）

13:01 用户指示恢复：OI-W9-1 单开 W14 补齐（relay-style 接线）。**14:10 用户再次指示暂停派发**：不再派发新子代理，运行中的 W13b（T11.5+11.6）与 W14（relay-style）继续跑完，回调照常记录，解锁链挂起等"恢复"指示。
14:18/14:33 W13b（e98def4f）与 W14（e8c5ed63）先后 GREEN 并记录。21:35 用户指示优化 CPU 门禁（恢复 OPT1/OPT2 派发）；22:36 OPT2 完成 **4e52485d**（1MiB 增量 5.41%，达标）。22:55 用户指示继续执行：派发 W12（T11.3，gemini-3.8-flash-high 首个正式任务）∥ R10a（接线三连 + repetition 早退数学）。后续：W12/R10a 完成 → R10b（FW2+W13b+OPT 审查）∥ 长跑场景 26。

### W13b 重要发现（已处置）
- ~~**OI-W13b-1（CPU 门禁超标）**~~ → **已优化解决**（4e52485d，见"CPU 优化任务"节：1MiB 增量 5.41%，场景 24 已恢复 1MiB 计划口径）。
- ~~**OI-W13b-2（独立佐证 W14）**~~ → W14 已接线 relay 分支（e8c5ed63），佐证闭环。
- 其他：场景 25 用 encoding/json 构造 payload（PRNG ASCII 引号破坏 JSON）；场景 26 每 worker 独立 PRNG（计划参考共享 PRNG 会 race）；长跑 10min 需 RUN_OUTPUTGUARD_LONG_RUN=1（**待用户决策是否执行**）。

### W9 遗留 open issues（待决策）
- **OI-W9-1**：Responses 的 relay-style 分支（account.IsRelayStyle：OpenAI Responses 中转/Antigravity Responses 等）未按 04b 锚点接线，本次仅覆盖标准 Codex 主分支 + Chat 主分支。建议：并入 W10 范围或单开 W14 补齐。
- **OI-W9-2**（设计注记，暂不改）：冷启动 Load 的 parent ctx 在写 lastSyncError 前取消时，门禁退化为默认配置而非 503，直到轮询恢复（与既有测试兼容性设计相关）。
- W9 偏差记录：冷启动门禁改为 outputGuardHTTPGateArmed()（仅 ready 或 lastSyncError 非空时生效，避免既有全量测试 503）；测试阈值用 DB 合法最小值（计划 100/200 字节配置会被校验拒绝）；Chat 流式无 downstreamMu 传 nil mutex；gotTerminal 防 upstream_stream_break 合成。

## 待办（按解锁顺序）

- [x] W9 完成 → W10：T10B native HTTP（04c）— **运行中**
- [ ] W10 完成 → W14：OI-W9-1 relay-style 接线（handler.go 串行）→ W12：T11.3 生命周期/全协议验收（05b，场景 12-20）∥ W13b：T11.5+11.6 性能基准+调度脚本（05c，依赖 T11.1+W10）
- [ ] W10 完成 → W12：T11.3 生命周期/全协议验收（05b，场景 12-20）∥ W13b：T11.5+11.6 性能基准+调度脚本（05c，依赖 T11.1+W10）
- [ ] 全部 GREEN → 终验（go build + 全量测试 + pnpm typecheck）
- [ ] 最终整理清单：① R3-LOW-1 SQL 字面量 262144 与 Go 常量无编译期绑定（fmt.Sprintf 注入或同步注释）；② R3-LOW-2 补边界端点"接受"方向测试；③ R5-backlog 多行事件 join 分隔符未计入事件 cap（极端 payload 2×）；④ R6-LOW aliasMap 死代码确认/删除；⑤ R7-LOW×4：场景 1 触发区间补 ≥8192 下界断言、database PG Skip 死分支改始终注册+skip、candidates map/slice 冗余合并、并发 rc 组补终值断言；⑥ R9-LOW×2：perfstat ErrInvalidBaseline 无消费者（删或留 T11.5）、perfstat Fill() 低位字节 LCG 短周期 256（重复检测 fixture 改用 GenerateASCII/高位）；⑦ R8-LOW-4 DeadlineSuccess 未验 5s 真实超时（集成测试）；⑧ 计划文件勾选/同步提交（含 03-protocol Task 8 参考代码同步 FW1 修复、02c Step 5 的 128*KIB 字面量 → 256*KIB、W7 的 Progress 清单、04c 的 Progress 清单）；⑨ R10b-LOW×5：stream_flush_writer 三处锁外直写补 aborted 二次检查（一行）、Flush/WriteBytes 跨 goroutine 契约注释、并发测试补 NewOutputScanner 变体、repetition.go Push/PushWithHash 推进逻辑提取 helper、run_acceptance_suite.sh 第 3 步改强断言或改名；⑩ 死代码 handleStreamResponse/handleCompactResponse 删除（R10a-LOW-4）；⑪ 汇总报告
