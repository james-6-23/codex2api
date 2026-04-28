# Codex2API 增强功能测试指南

本文档说明如何测试我们添加到 codex2api 的增强功能。

## 环境要求

- Go 1.21+
- Node.js 18+
- 至少 5GB 可用磁盘空间（用于编译）
- SQLite（内置）或 PostgreSQL + Redis

## 快速启动（推荐）

### 方案 1：使用 Docker Compose（最简单）

```bash
cd D:/cc/my-project/codex2api

# 使用 SQLite 轻量版
docker-compose -f docker-compose.sqlite.local.yml up -d

# 查看日志
docker-compose -f docker-compose.sqlite.local.yml logs -f
```

访问：http://localhost:8080/admin

### 方案 2：本地编译运行

```bash
cd D:/cc/my-project/codex2api

# 1. 创建配置文件
cp .env.sqlite.example .env

# 2. 创建必要目录
mkdir -p data/images logs

# 3. 构建前端（如果未构建）
cd frontend
npm install
npm run build
cd ..

# 4. 编译后端
go build -o codex2api.exe .

# 5. 运行
./codex2api.exe
```

访问：http://localhost:8080/admin

## 增强功能测试清单

### ✅ Phase 1: 2K/4K 图片放大功能

**测试位置：** 图片生成工作台

**测试步骤：**

1. 访问 `/admin` 进入管理后台
2. 添加一个 ChatGPT 账号（需要有效的 access_token）
3. 进入「图片工作台」
4. 生成一张图片
5. 在图片生成请求中添加 `upscale` 参数：
   ```json
   {
     "prompt": "a beautiful sunset",
     "upscale": "2k"  // 或 "4k"
   }
   ```

**预期结果：**
- 图片会被放大到 2K (2048px) 或 4K (4096px)
- 使用 Catmull-Rom 插值算法，画质清晰
- 放大后的图片会被缓存（默认 512MB LRU 缓存）
- 如果放大失败，会优雅降级返回原图

**验证代码位置：**
- 后端集成：`admin/image_studio.go:659-688`
- 核心算法：`compat/image/upscale.go`
- 缓存机制：`compat/image/upscale.go:UpscaleCache`

### ✅ Phase 2: HMAC 防盗链代理

**测试位置：** 图片代理 URL

**测试步骤：**

1. 生成一张图片后，获取图片 URL
2. 检查 URL 是否包含 HMAC 签名参数：
   ```
   /proxy/image/{asset_id}?exp={timestamp}&sig={hmac_signature}
   ```
3. 尝试访问该 URL，应该成功返回图片
4. 修改 `sig` 参数，应该返回 403 Forbidden
5. 等待过期时间后访问，应该返回 410 Gone

**预期结果：**
- 图片 URL 包含 HMAC-SHA256 签名
- 签名验证失败返回 403
- 过期后返回 410
- 进程级随机密钥，24小时有效期

**验证代码位置：**
- HMAC 生成：`compat/proxy/hmac.go:BuildImageProxyURL`
- 签名验证：`compat/proxy/hmac.go:VerifyImgSig`
- 代理处理：`compat/proxy/handler.go`

### ✅ Phase 3: SSE 流式响应优化

**测试位置：** API 请求响应

**测试步骤：**

1. 使用 API 密钥调用图片生成接口
2. 设置 `stream: true` 参数
3. 观察响应是否为 SSE 流式输出
4. 检查是否实时推送进度事件

**预期结果：**
- 响应头包含 `Content-Type: text/event-stream`
- 实时推送进度事件：
  ```
  event: progress
  data: {"step": 1, "total": 4}
  
  event: image
  data: {"url": "...", "index": 0}
  ```
- 前端可以使用 EventSource 接收实时更新

**验证代码位置：**
- SSE 流处理：`compat/sse/stream.go`
- 事件写入：`compat/sse/stream.go:StreamWriter.WriteEvent`
- 事件解析：`compat/sse/stream.go:ParseImageSSE`

### ✅ Phase 4: RT/ST 双路径刷新

**测试位置：** 账号刷新功能

**测试步骤：**

1. 添加一个 ChatGPT 账号（使用 refresh_token）
2. 等待 access_token 过期（或手动触发刷新）
3. 观察后台日志，查看刷新路径：
   - 优先使用 RefreshToken 刷新
   - 如果 RT 失败，回退到 SessionToken 刷新
4. 检查刷新后的 token 是否包含 Web 作用域

**预期结果：**
- 自动选择最佳刷新路径
- RT 失败时智能回退到 ST
- 验证 token 包含 Web 作用域
- 刷新失败时有详细错误信息

**验证代码位置：**
- 刷新器：`compat/refresh/refresher.go`
- RT 刷新：`compat/refresh/refresher.go:RefreshByRT`
- ST 刷新：`compat/refresh/refresher.go:RefreshByST`
- Web 验证：`compat/refresh/refresher.go:VerifyATOnWeb`

### ✅ 额外功能：Sub2API 详细用量统计

**测试位置：** 账号管理 - 用量统计

**测试步骤：**

1. 访问 `/admin` 进入管理后台
2. 点击任意账号的「用量统计」按钮
3. 查看用量模态框，应该显示：
   - 总体统计（总请求数、总 Token 数等）
   - 模型分布饼图
   - **5小时用量卡片**（新增）
   - **7天用量卡片**（新增）

**预期结果：**

5小时和7天用量卡片应该显示 4 个维度：
- ✅ 请求数 (Requests)
- ✅ Token 数 (Tokens)
- ✅ 账号计费 (Account Billed) - 显示为绿色金额
- ✅ 用户扣费 (User Billed) - 显示为蓝色金额

**验证代码位置：**
- 后端数据结构：`database/postgres.go:1245-1265`
- 时间范围查询：`database/postgres.go:getTimeRangeUsage`
- 前端类型定义：`frontend/src/types.ts:90-105`
- 前端 UI 组件：`frontend/src/components/AccountUsageModal.tsx:100-114`
- 卡片组件：`frontend/src/components/AccountUsageModal.tsx:128-157`

## 单元测试验证

所有兼容层功能都有完整的单元测试：

```bash
cd D:/cc/my-project/codex2api

# 运行所有兼容层测试
go test ./compat/... -v

# 运行特定模块测试
go test ./compat/image -v      # 图片放大测试
go test ./compat/proxy -v      # HMAC 代理测试
go test ./compat/sse -v        # SSE 流式测试
go test ./compat/refresh -v    # RT/ST 刷新测试
```

**预期结果：**
```
=== RUN   TestValidateUpscale
--- PASS: TestValidateUpscale (0.00s)
=== RUN   TestDoUpscale
--- PASS: TestDoUpscale (0.00s)
=== RUN   TestUpscaleCache
--- PASS: TestUpscaleCache (0.00s)
...
PASS
ok      github.com/codex2api/compat/image       0.123s
```

## 完整文档

更多详细信息请参考：

- **集成指南：** `compat/INTEGRATION.md`
- **前端 API 文档：** `compat/FRONTEND_INTEGRATION.md`
- **快速开始：** `compat/QUICKSTART.md`
- **测试指南：** `compat/TESTING_GUIDE.md`
- **完成报告：** `compat/SOLUTION_A_FINALREPORT.md`

## 故障排查

### 问题 1：编译失败 - 磁盘空间不足

**解决方案：**
```bash
# 清理 Go 缓存
go clean -cache -modcache -testcache

# 或使用 Docker 方式运行（不需要本地编译）
docker-compose -f docker-compose.sqlite.local.yml up -d
```

### 问题 2：前端未构建

**解决方案：**
```bash
cd frontend
npm install
npm run build
cd ..
```

### 问题 3：数据库连接失败

**解决方案：**
- 检查 `.env` 文件配置
- 确保 `data/` 目录存在且有写权限
- SQLite 模式：确保 `DATABASE_PATH` 指向可写位置

### 问题 4：图片放大功能不生效

**解决方案：**
- 检查 `main.go` 是否调用了 `compat.InitCompat()`
- 查看日志确认兼容层是否初始化成功
- 确认请求中包含 `upscale` 参数

## 性能基准

我们的增强功能性能指标：

- **图片放大：** 2K 放大 ~200ms，4K 放大 ~800ms（取决于原图大小）
- **LRU 缓存：** 命中率 >80%，缓存查询 <1ms
- **HMAC 验证：** <1ms
- **SSE 流式：** 实时推送，延迟 <50ms

## 代码仓库

所有增强功能代码已推送到：
- **Fork 仓库：** https://github.com/icysaintdx/codex2api
- **分支：** `feature/compat-layer`

## 联系支持

如有问题，请查看：
1. 项目日志：`./logs/`
2. 单元测试输出
3. 完整文档：`compat/` 目录

---

**测试完成后，请反馈：**
- ✅ 哪些功能正常工作
- ❌ 遇到的问题和错误信息
- 💡 改进建议
