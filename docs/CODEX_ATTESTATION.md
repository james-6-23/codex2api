# Codex 出站 attestation

Codex 账号的 HTTP、Responses WebSocket、Live 建连和 sideband 共用一套
`X-Oai-Attestation` 规则，适用于 Desktop、TUI、IDE、Exec、SDK 等客户端身份。
平台取自最终出站 User-Agent；服务器的操作系统只决定能否实际调用原生 DeviceCheck。

OAuth 换码、令牌刷新和普通维护请求不调用对话的 attestation provider，也不复用对话证明。
这与 Codex `rust-v0.166.0` 的登录客户端和默认 HTTP 头构建保持一致；令牌刷新只复用
所选客户端的 UA、Originator，授权链接携带 Originator，换码使用独立客户端。

## 平台与失败状态

| 出站身份或原生结果 | 自动行为 |
| --- | --- |
| Windows | 内层 `error_code: 1`，携带账号设备画像 |
| Linux，包括各发行版 | 不发送证明，同时清除外部提供的该请求头 |
| macOS x64 | 内层 `error_code: 2`，表示架构不支持 |
| macOS arm64，原生生成成功 | 内层携带真实 `token` |
| macOS arm64，原生报告设备不支持 | 内层 `error_code: 3` |
| macOS arm64，非兼容 Mac 宿主、缺组件、调用异常、超时或没有 token | 内层 `error_code: 4` |

上述自动生成结果的外层都是 `{"v":1,"s":0,"t":"v1.…"}`。
`t` 是 Base64URL 编码的 CBOR；其中包含 `bundle_id: "com.openai.codex"`、
设备画像 `f`、真实 token 或错误码，以及原生返回时可选的浮点耗时 `t`。
外层 `s:0` 表示成功构造结果，并不表示 DeviceCheck 校验通过。

这些状态来自官方桌面包的生成策略：Windows/macOS `26.1021.31345`，
Linux `26.1021.22027`。三平台包中的相关生成函数一致。Linux 默认不启用
attestation，因此不主动发送其“平台不支持”的备用 token。

## 身份、画像与证明来源

证明仅使用本项目服务按最终出站身份生成的结果。下游请求和账号自定义请求头中的
`X-Oai-Attestation` 均被清除，不能覆盖生成结果；Linux 或未启用证明的平台不发送该头。
Live sideband 只恢复服务端保存的建连结果，包括明确的“不发送”，不会接收客户端证明。

所有自动生成的画像使用账号绑定时区派生语言和日期，按账号固定选择屏幕档位，
会话 UUID 按该时区的自然日轮换。保留原 Windows 实现的种子和档位映射，
升级不会改变已有 Windows 账号的生成结果。

例如，Linux/Docker 上模拟 Windows 出站身份时仍生成 Windows 降级状态；
模拟 macOS arm64 时因无法调用原生组件而生成错误码 4。Mac 主机使用 Linux
出站身份时不自动添加证明。

## Mac 原生依赖

原生生成需要 Apple Silicon Mac，以及安装在系统或用户 `Applications` 目录中的
官方 `ChatGPT.app` 或旧名称 `Codex.app`，应用标识须为 `com.openai.codex`。
复用应用内的 `cua_node/bin/node` 和 `native/devicecheck.node`，无需新增 Go 原生依赖。

原生调用在独立子进程中进行，执行上限为 5 秒并遵从请求取消。设备不支持、组件缺失
或进程失败时返回上述内层状态，不因生成失败在本地阻断普通请求或 Live。

CBOR 和设备画像可以在软件中构造，真实 DeviceCheck token 仍由 Apple 原生框架生成。
不能把带错误码的结果视为硬件证明；实际可用性和权限由上游决定。

公开资料检索未找到可验证的 Windows/Linux 纯软件生成有效 Mac DeviceCheck token 的实现。
Apple 的 [DCDevice 文档](https://developer.apple.com/documentation/devicecheck/dcdevice)
要求先检查设备支持情况，生成 token 后交由服务端处理，并要求注册 App ID。
[oh-my-pi 的原生调用问题记录](https://github.com/can1357/oh-my-pi/issues/8353)
也表明直接调用仍依赖 Mac 的 DeviceCheck 框架，且无图形登录会话时可能使进程异常退出；
因此本项目将该调用隔离在子进程中。这些资料不能证明跨平台模拟绝无可能。

本次在 Linux 验证封装字节、平台分支、失败状态、Live 复用与跨平台编译，未验证 Mac
真机生成成功或真实上游接受 Live 请求。

## Live 会话

Live 在选定账号后装配身份和证明；切换账号重试时重新解析。成功建连后，保存
最终出站 UA、Version、Originator，并将最终证明加密保存到会话映射。
未发送证明也加密保存为空字符串，区别于缺失或损坏的密文。

sideband 使用建连账号、冻结身份和解密后的证明，不重新生成，也不受随后修改的
身份或自定义请求头影响。已有会话缺少身份快照时沿用原身份解析；原有证明密文格式兼容。
API Key 的“自动”和 Codex 渠道均可设置“允许 ChatGPT Live”，新建 Key 在这两个渠道下
默认开启，也可手动关闭。创建接口省略 `limits.allow_live` 时采用相同默认值，显式
`false` 仍关闭；已有 Key 保留已保存的选择。长会话并发与租约沿用现有规则。

上游建连使用 `https://chatgpt.com/backend-api/codex/realtime/calls`，v3 sideband 使用
`wss://api.openai.com/v1/live/<call_id>`。两步均使用池中同一个 Codex 账号的 OAuth
access token 和 account ID；sideband 域名不同不代表切换为平台 API Key。
此行为对应 [Codex 0.164.0 的鉴权说明](https://github.com/openai/codex/blob/rust-v0.164.0/codex-rs/core/src/client.rs#L525)
及 [ChatGPT provider 的端点测试](https://github.com/openai/codex/blob/rust-v0.164.0/codex-rs/codex-api/src/endpoint/realtime_websocket/methods.rs#L2497)。
拨号失败时服务端记录 HTTP 握手状态、上游 request ID 与调用映射哈希；不记录证明或鉴权头。
