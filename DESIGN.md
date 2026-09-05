# DESIGN.md — 前端 UI 约束

本文件是 `frontend/` 的组件与布局约束。所有前端改动（含 AI 代理生成的代码）必须遵守；
`frontend/src/lib/uiConventions.test.mjs` 与 `claudeParity.test.mjs` 会在 CI 中强制其中可机检的部分。

## 1. 表单控件：只用共享组件，不手写

| 需求 | 必须使用 | 禁止 |
|---|---|---|
| 下拉选择 | `components/ui/select.tsx` 的 `Select`（`options` 数组，`value` / `onValueChange`） | 原生 `<select>` / 自定义 className 字符串（如 `selectCls`） |
| 开关 | `components/ui/switch` 的 `Switch` | `<input type="checkbox">` |
| 数字输入 | `components/ui/draft-number-input` 的 `DraftNumberInput`（带 `min` / `max`） | `<Input type="number">`（仅历史遗留允许） |
| 文本输入 | `components/ui/input` 的 `Input` | 原生 `<input>` |
| 少量互斥选项 | `Settings.tsx` 的 `SegmentedPillGroup` | 手写按钮组 |
| 按钮 | `components/ui/button` 的 `Button`，图标用 lucide，加载态用 `RefreshCw` + `animate-spin` | 原生 `<button>` |

需要新的表单控件时，先在 `components/ui/` 新增共享组件，再在页面使用；不在页面内部就地实现。

## 2. 设置页布局

- 系统设置页按 Tab 拆分：`codex` / `claude` / `antigravity` / `grok` / `appearance` / `general`，由 `?tab=` 查询参数驱动（`SettingsTabKey`），每次只渲染当前 Tab 的内容；旧的 `#settings-xxx` 锚点通过 `LEGACY_SECTION_TABS` 映射到 Tab。新增配置块先判断归属：某个上游渠道专属的进对应渠道 Tab，跨渠道的进 `general`，站点品牌/背景进 `appearance`。
- Tab 内用 `SettingsSection`（`id` / `title` / `description` / `icon`）分组，每个配置模块用 `SettingsCard`（`title` / `description` / `icon` / `footer`）。新增或删除 `SettingsSection` 时同步维护 `SETTINGS_TAB_SECTION_INDEX`（Tab → 分区目录），多于一个分区的 Tab 会据此在 Tab 栏下方渲染居中的磨砂玻璃分区胶囊条（与 Tab 栏同一粘性容器）并做滚动高亮；`settingsTabs.test.mjs` 会校验目录里的 id 都对应真实渲染的 section。
- Tab 栏是页面流内的 `sticky` 元素，不用 `fixed` 悬浮；分区标题（`SettingsSection`）是文字级标题，不带主色图标芯片；`SettingsCard` 的图标芯片用中性色，主色只留给 `tone="danger"` 与交互高亮，避免整页都是强调色。
- 保存心智：开关/下拉类字段走 `autoSaveSettingsPatch` 即时生效；文本/数值类字段进 `settingsForm` 后由 `persistedSettings` 快照做脏检查，页头 `SaveStatusPill` 显示状态，底部操作条只在 `dirtyCount > 0` 时出现。任何把服务端确认值写回表单的新路径都要同步 `setPersistedSettings` / `markPersisted`，否则会出现假的"未保存"计数。
- 单个配置项用 `SettingField`（`label` / `description` / `layout="switch"` 可选），说明性提示用 `SettingHelp`。
- 放在 `general` Tab 里的跨渠道卡片必须通过 `channels` 属性声明适用渠道（`SettingsCard` / `SettingField` 均支持，渲染 `components/ChannelScopeBadges.tsx`）：全渠道用 `ALL_UPSTREAM_CHANNELS`，更窄的用 Settings.tsx 顶部的 `CHANNELS_*` 常量；范围要按后端消费点核对，不凭文案猜。渠道 Tab 内的卡片默认视为该渠道专属，只有同时作用于其他渠道时才加标记（如 Codex 的全局自动暂停也作用于 Claude 的 5h/7d 窗口）。
- 栅格只用 `SETTINGS_FIELD_GRID` / `SETTINGS_FIELD_GRID_3` / `SETTINGS_SWITCH_GRID` 常量，不手写 `grid-cols-*`；卡片级双列排布用 `SETTINGS_CARD_GRID_2`（自带 `lg:items-start`，卡片高度不一时不拉伸）。
- 卡片里只有一个开关时用 `SETTINGS_SWITCH_ROW`（整行），不要放进 `SETTINGS_SWITCH_GRID`（会挤成半宽、标签折行）。一组只含开关的相关设置不要拆成多张窄卡，合并成一张卡用 `SETTINGS_ROW_LIST` + `SettingField layout="row"`（`description` 外显、`help` 进 tooltip）逐行排列，参照 Codex Tab 的「压缩与兼容开关」卡。
- "开关 + 数值"成对的行（例如自动同步 + 间隔）沿用 Codex "客户端与指纹" 区块的两列边框布局；新增同类区块直接复制该结构。
- 版本号、ID 等等宽内容用 `font-mono text-xs text-muted-foreground`。
- 供应商可见性：仪表盘/账号管理/用量渠道过滤器里任何枚举上游渠道的地方都要经过 `useVisibleChannels()`（`src/visibleChannels.tsx`，由 `/settings/visible-channels` 驱动，Codex 兜底始终可见）过滤，不要直接写死四渠道列表；新增消费点在 `lib/visibleChannels.test.mjs` 加断言。

## 3. 文案

- 所有可见文案走 `t('namespace.key')`；新增 key 必须同时写入 `locales/zh.json`、`en.json`、`zh-TW.json` 三个文件的同一位置。
- 占位符用 `{{name}}`，不用字符串拼接。

## 4. 守卫测试

- 新增或改动设置区块时，在 `frontend/src/lib/claudeParity.test.mjs`（Claude 相关）或对应的源码守卫测试里加断言，覆盖：使用了哪个共享组件、调用了哪个 API 方法、i18n key 存在。
- `uiConventions.test.mjs` 会扫描 `pages/` 与 `components/`（排除 `components/ui/select.tsx`）拒绝任何原生 `<select>`。

## 5. 参照实现

- 共享下拉：`frontend/src/pages/Settings.tsx` ClaudeCode 卡片的时区 / 指纹模式 / 平台 / 版本策略字段。
- 同步按钮 + 自动同步开关 + 间隔：Settings.tsx 中 Codex "客户端与指纹" 与 ClaudeCode "CLI 版本同步" 区块。
