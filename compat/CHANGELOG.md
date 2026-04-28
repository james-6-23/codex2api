# 兼容层更新日志

本文档记录 codex2api 兼容层的所有重要变更。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.0.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [未发布]

### 新增
- 2K/4K 本地图片放大功能（Catmull-Rom 插值算法）
- 进程内 LRU 缓存（默认 512MB）
- 并发控制机制（避免 CPU 打满）
- JPEG 缩略图生成（多档降级策略）
- HMAC 防盗链代理（HMAC-SHA256 签名）
- 配置管理系统
- 完整的单元测试
- 集成文档和示例代码

### 技术细节
- **算法**: Catmull-Rom (biquintic) 插值
- **支持格式**: PNG/JPEG/GIF/WEBP 输入，PNG 输出（放大）
- **性能**: 首次放大 0.5-2s，缓存命中 < 10ms
- **内存**: 512MB 缓存 + 临时解码内存
- **依赖**: `golang.org/x/image v0.39.0`

### 文件清单
```
compat/
├── README.md                    # 兼容层总览
├── INTEGRATION.md               # 集成指南
├── CHANGELOG.md                 # 本文件
├── config.example.yaml          # 配置示例
└── image/
    ├── upscale.go              # 核心放大算法（218 行）
    ├── thumb.go                # 缩略图生成（167 行）
    ├── config.go               # 配置管理（50 行）
    └── upscale_test.go         # 单元测试（100 行）
```

## [计划中]

### 阶段 2：HMAC 防盗链代理 ✅ 已完成
- [x] 签名 URL 生成
- [x] 签名验证中间件
- [x] 图片代理下载
- [x] 集成到现有路由

### 阶段 3：SSE 直出优化 ✅ 已完成
- [x] SSE 流读取器实现
- [x] SSE 流写入器实现
- [x] 图片进度事件解析
- [x] 带缓冲的流写入器
- [x] 完整的单元测试

### 阶段 4：RT/ST 双路径刷新 ✅ 已完成
- [x] RefreshToken 刷新逻辑
- [x] SessionToken 回退机制
- [x] Web 作用域校验
- [x] 自动刷新调度器

## 版本历史

### [0.1.0] - 2026-04-28

#### 新增
- 初始版本
- 2K/4K 本地放大功能
- 缩略图生成功能
- 基础文档

#### 移植来源
- 源项目：[gpt2api](https://github.com/432539/gpt2api)
- 移植文件：
  - `internal/image/upscale.go` → `compat/image/upscale.go`
  - `internal/image/thumb.go` → `compat/image/thumb.go`
- 修改内容：
  - 简化 LRU 缓存实现（从 list 改为 map + 访问计数）
  - 添加全局缓存管理
  - 添加配置系统

---

## 贡献指南

### 如何添加新功能

1. 在 `compat/` 下创建对应的子目录
2. 编写核心代码和测试
3. 更新 `README.md` 和 `INTEGRATION.md`
4. 在本文件中记录变更
5. 提交 PR

### 变更日志格式

每个版本包含以下部分（如适用）：
- **新增**: 新功能
- **变更**: 现有功能的变更
- **弃用**: 即将移除的功能
- **移除**: 已移除的功能
- **修复**: 错误修复
- **安全**: 安全相关的修复

### 版本号规则

- **主版本号**: 不兼容的 API 变更
- **次版本号**: 向后兼容的功能新增
- **修订号**: 向后兼容的问题修复

---

## 许可证

本兼容层代码移植自 [gpt2api](https://github.com/432539/gpt2api)，遵循原项目许可证。
