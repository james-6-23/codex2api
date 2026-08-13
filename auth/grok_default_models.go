package auth

// Grok 账号未声明 models 白名单、且尚未同步到账号目录时的保守默认模型集。
// 这是全仓库唯一权威来源：proxy 的注册/调度面(DefaultGrokModelIDsForAccount)
// 与 auth 的授权门(GrokChannelSupportsModel)都必须从这里取,避免两份清单发散后
// 出现"目录展示但调度拒绝"(或反之)的错配——PR #513 与 grok-4.6 默认集就曾因
// 双份硬编码在合并后互相打架。
//
// 两条通道的目录并不相同,必须分开取:
//   - OAuth 走 cli-chat-proxy,目录由 CLI 通道决定。实测 supergrok_heavy 与 free
//     两种套餐原先只返回 grok-4.5;grok-4.6 作为当前旗舰一并列入兜底,不含 grok-3 / grok-2。
//   - API Key 走 xAI 公开 API,目录更宽。
//
// 默认集只是探测不到时的兜底:账号导入或连通性测试跑过 FetchGrokModelIDs 后,
// 应以探到的真实目录为准。

// GrokOAuthDefaultModelIDs 返回 OAuth 凭据账号的默认可用文本模型集。
func GrokOAuthDefaultModelIDs() []string {
	return []string{"grok-4.6", "grok-4.5"}
}

// GrokAPIKeyDefaultModelIDs 返回 API Key 凭据账号的默认可用文本模型集。
func GrokAPIKeyDefaultModelIDs() []string {
	return []string{"grok-4.6", "grok-4.5", "grok-4", "grok-3-fast", "grok-3", "grok-2"}
}
