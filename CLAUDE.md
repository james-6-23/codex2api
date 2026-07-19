# codex2api

## Skill routing

When the user's request matches an available skill, invoke it via the Skill tool. When in doubt, invoke the skill.

Key routing rules:
- Product ideas/brainstorming → invoke /office-hours
- Strategy/scope → invoke /plan-ceo-review
- Architecture → invoke /plan-eng-review
- Design system/plan review → invoke /design-consultation or /plan-design-review
- Full review pipeline → invoke /autoplan
- Bugs/errors → invoke /investigate
- QA/testing site behavior → invoke /qa or /qa-only
- Code review/diff check → invoke /review
- Visual polish → invoke /design-review
- Ship/deploy/PR → invoke /ship or /land-and-deploy
- Save progress → invoke /context-save
- Resume context → invoke /context-restore
- Author a backlog-ready spec/issue → invoke /spec

## 本地实测

改完代码需要对真实上游实测时:

```
docker compose -f docker-compose.pgredis2004.yml up -d --build
```

一条命令完成重建+重启,服务在 http://127.0.0.1:2004。管理 API 直接加请求头 `X-Admin-Key: 123456`(前缀 `/api/admin`);测试用下游 API key 查 DB `api_keys` 表。改过全局设置(如 `payload_rules`)测完必须恢复原值——该部署有真实外部流量在用。

## 发版流程

版本发布按以下步骤走(用户说"发版"/"打 tag 发布"时执行全程):

1. **更新 CHANGELOG.md**:顶部插入新版本段(`## vX.Y.Z - YYYY-MM-DD`),按既有风格写英文条目(Features/Fixes,加粗导语,带 issue 引用),提交 `docs(changelog): vX.Y.Z` 并推送。
2. **打 tag 并推送**:`git tag vX.Y.Z && git push origin vX.Y.Z`。tag 推送触发 `.github/workflows/release.yml` 自动构建多平台产物并创建 GitHub Release(notes 是自动生成的占位内容)。
3. **等工作流跑完**:`gh run watch` 或轮询 `gh run list --workflow=release.yml`,确认 release 产物(darwin/linux/windows 压缩包 + SHA256SUMS)已挂上。
4. **替换 release notes**:`gh release edit vX.Y.Z --notes-file <file>`。notes 用**中文**,参考上一版本(`gh release view v<prev>`)的固定结构:
   - 开头一段 `>` 引用:一句话定位本版聚焦点,加粗最重要的修复/功能;
   - `## Features` / `## Fixes`:逐条加粗标题(含 issue 号与贡献者),正文说清背景→根因→方案→验证;
   - `## Upgrade Notes`:数据库迁移、默认行为变化、是否建议升级、逃生阀;
   - `## 致谢`:列出 issue 报告者/贡献者(@handle — 贡献内容);
   - 结尾 `**Full Changelog**: https://github.com/james-6-23/codex2api/compare/v<prev>...vX.Y.Z`。
