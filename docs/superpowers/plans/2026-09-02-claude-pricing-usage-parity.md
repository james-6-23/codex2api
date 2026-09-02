# Claude 定价与账号额度同步修复计划

## 目标

让 Claude/ClaudeCode 的模型价格、缓存读写价格、官方来源链接和账号级 Fable 配额与 Anthropic 官方数据一致，并保持现有旧账号和旧接口兼容。

## 实施步骤

1. **建立失败测试与影响边界**
   - 为 Anthropic 官方 HTML 价格解析、Fable 5/5.1 缓存读写价格、OAuth `limits[]` 额度解析和 API 响应字段补充测试。
   - 对将修改的同步函数、定价模型和账号响应构建函数执行 GitNexus upstream impact，记录风险和直接调用方。

2. **修复官方 Anthropic 定价同步**
   - 从 Anthropic 官方 API 定价页抓取并解析 Input、5m/1h Cache write、Cache read、Output。
   - 正确映射 Fable 5.1、Fable 5 以及现有 Claude 家族模型；缓存读取用于实际计费，缓存写入单独保存并展示。
   - 返回并展示 Anthropic 官方价格链接，明确“缓存读取”与“缓存写入”标签。

3. **修复 Claude 账号额度同步**
   - 使用 Claude OAuth usage 端点读取 5h、7d 及 `limits[]` 模型族额度。
   - 持久化 `7d_fable` 共用额度，账号详情/API/前端展示 Fable 5.x 进度和重置时间；失败时回退现有 Messages 探测。

4. **验证与回归**
   - 运行 Go 定价、账号、代理测试及前端相关测试/构建。
   - 运行 GitNexus detect_changes，确认变更仅影响预期流程；必要时修正兼容性问题。

5. **交付**
   - 汇总官方价格字段、缓存计费语义、Fable 配额来源和测试结果，等待用户确认后再提交/部署。
