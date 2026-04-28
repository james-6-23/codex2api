# 集成验证完整指南

## 📊 一、集成内容详细说明

### 1.1 文件位置总览

```
D:/cc/my-project/codex2api/
├── compat/                          # 新增：独立兼容层目录
│   ├── image/                       # Phase 1: 2K/4K 图片放大
│   │   ├── upscale.go              # 核心放大算法 (218行)
│   │   ├── thumb.go                # 缩略图生成 (167行)
│   │   ├── config.go               # 配置管理 (50行)
│   │   └── upscale_test.go         # 单元测试 (100行)
│   ├── proxy/                       # Phase 2: HMAC 防盗链
│   │   ├── hmac.go                 # HMAC 签名 (60行)
│   │   ├── handler.go              # 代理处理器 (110行)
│   │   └── hmac_test.go            # 单元测试 (70行)
│   ├── sse/                         # Phase 3: SSE 流式响应
│   │   ├── stream.go               # SSE 核心 (380行)
│   │   └── stream_test.go          # 单元测试 (240行)
│   ├── refresh/                     # Phase 4: RT/ST 刷新
│   │   ├── refresher.go            # 刷新逻辑 (320行)
│   │   └── refresher_test.go       # 单元测试 (140行)
│   ├── init.go                      # 全局初始化入口
│   ├── examples/test.html           # HTML 测试页面
│   └── 9 个文档文件
├── admin/image_studio.go            # 已修改：集成放大逻辑 (+37行)
└── go.mod                           # 已修改：添加依赖
```

### 1.2 三个核心集成点

**集成点 1: 导入兼容层包**
```go
// admin/image_studio.go:23
compatImage "github.com/codex2api/compat/image"
```

**集成点 2: 验证 upscale 参数**
```go
// admin/image_studio.go:260
req.Upscale = compatImage.ValidateUpscale(req.Upscale)
```

**集成点 3: 执行图片放大逻辑**
```go
// admin/image_studio.go:659-688 (37行新代码)
if upscale := compatImage.ValidateUpscale(req.Upscale); upscale != "" {
    cache := compatImage.GetGlobalCache()
    cacheKey := fmt.Sprintf("%d-%02d-%s", jobID, idx+1, upscale)
    
    // 缓存查询
    if cached, cachedCT, ok := cache.Get(cacheKey); ok {
        imageBytes = cached
        contentType = cachedCT
        log.Printf("[upscale] 缓存命中: %s", cacheKey)
    } else {
        // 执行放大
        cache.Acquire()
        upscaled, upCT, upErr := compatImage.DoUpscale(imageBytes, upscale)
        cache.Release()
        
        if upErr == nil && len(upscaled) > 0 {
            imageBytes = upscaled
            contentType = upCT
            cache.Put(cacheKey, imageBytes, contentType)
            log.Printf("[upscale] 放大成功: %s", cacheKey)
        } else {
            log.Printf("[upscale] 放大失败，使用原图: %v", upErr)
        }
    }
}
```

---

## 🧪 二、三个部分的验证方法

### 2.1 Phase 1: 2K/4K 图片放大功能

#### 验证方法 1: 单元测试 ✅

```bash
cd D:/cc/my-project/codex2api
go test ./compat/image -v
```

**测试结果**:
```
✅ TestValidateUpscale - 验证 upscale 参数（"", "2k", "4k", "invalid"）
✅ TestDoUpscale - 测试图片放大功能
   ✅ 空档位返回原图
   ✅ 无效输入处理
✅ TestUpscaleCache - 测试 LRU 缓存
   ✅ 基础存取
   ✅ 并发控制
✅ TestClampThumbKB - 测试缩略图参数
```

#### 验证方法 2: 集成测试（需要启动服务）

**步骤 1: 初始化兼容层**

在 `main.go` 中添加：
```go
import "github.com/codex2api/compat"

func main() {
    // 初始化兼容层
    compat.InitCompat(true, 512, 4)
    
    // ... 原有代码
}
```

**步骤 2: 启动服务**
```bash
cd D:/cc/my-project/codex2api
go run main.go
```

**步骤 3: 测试 API 调用**

```bash
# 测试原图（不放大）
curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "prompt": "a beautiful sunset",
    "model": "gpt-image-2"
  }'

# 测试 2K 放大
curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "prompt": "a beautiful sunset",
    "model": "gpt-image-2",
    "upscale": "2k"
  }'

# 测试 4K 放大
curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "prompt": "a beautiful sunset",
    "model": "gpt-image-2",
    "upscale": "4k"
  }'
```

**预期响应**:
```json
{
  "job": {
    "id": 123,
    "status": "success",
    "assets": [
      {
        "id": 456,
        "width": 2560,
        "height": 2560,
        "actual_size": "2560x2560",
        "bytes": 10485760
      }
    ]
  }
}
```

#### 验证方法 3: HTML 测试页面

```bash
# 打开测试页面
start D:/cc/my-project/codex2api/compat/examples/test.html
```

**测试步骤**:
1. 输入 API 地址：`http://localhost:8080`
2. 输入 API Key（如果需要）
3. 输入提示词：`a beautiful sunset`
4. 选择分辨率：原图 / 2K / 4K
5. 点击"生成图片"按钮
6. 查看生成结果和图片信息

**验证点**:
- ✅ 原图：1024×1024，2-5 MB
- ✅ 2K：2560×2560，8-15 MB
- ✅ 4K：3840×3840，20-30 MB
- ✅ 首次生成时间：+1-2秒
- ✅ 缓存命中时间：<10ms

---

### 2.2 Phase 2: HMAC 防盗链代理

#### 验证方法 1: 单元测试 ✅

```bash
cd D:/cc/my-project/codex2api
go test ./compat/proxy -v
```

**测试结果**:
```
✅ TestComputeImgSig - 测试签名生成
✅ TestVerifyImgSig - 测试签名验证
   ✅ 有效签名通过
   ✅ 错误 assetID 拒绝
   ✅ 错误 expMs 拒绝
   ✅ 过期签名拒绝
✅ TestBuildImageProxyURL - 测试 URL 生成
```

#### 验证方法 2: 集成测试（需要启动服务）

**步骤 1: 注册代理路由**

在 `main.go` 中添加：
```go
import "github.com/codex2api/compat/proxy"

func main() {
    router := gin.Default()
    
    // 创建代理处理器
    proxyHandler := proxy.NewProxyHandler(&AssetResolverImpl{})
    
    // 注册代理路由
    router.GET("/p/img/:asset_id", proxyHandler.HandleImageProxy)
    
    // ... 其他路由
}
```

**步骤 2: 测试代理 URL**

```bash
# 生成图片并获取代理 URL
curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
  -H "Content-Type: application/json" \
  -d '{"prompt":"test","upscale":"2k"}' \
  | jq '.job.assets[0].proxy_url'

# 输出示例：/p/img/456?exp=1735459200000&sig=a1b2c3d4e5f6g7h8i9j0k1l2

# 访问代理 URL
curl http://localhost:8080/p/img/456?exp=1735459200000&sig=a1b2c3d4e5f6g7h8i9j0k1l2 \
  --output test.png
```

**验证点**:
- ✅ 有效签名返回图片（200 OK）
- ✅ 无效签名返回 403 Forbidden
- ✅ 过期签名返回 403 Forbidden
- ✅ 错误 asset_id 返回 404 Not Found

---

### 2.3 Phase 3: SSE 流式响应

#### 验证方法 1: 单元测试 ✅

```bash
cd D:/cc/my-project/codex2api
go test ./compat/sse -v
```

**测试结果**:
```
✅ TestStreamReader - 测试 SSE 流读取
✅ TestStreamWriter - 测试 SSE 流写入
✅ TestParseImageSSE - 测试事件解析
   ✅ 进度事件
   ✅ 图片完成事件
   ✅ 空事件处理
✅ TestBufferedStreamWriter - 测试缓冲写入
✅ TestStreamReaderTimeout - 测试超时控制
✅ TestWriteComment - 测试注释写入
✅ TestMultilineData - 测试多行数据
```

#### 验证方法 2: 集成测试（需要启动服务）

**步骤 1: 注册 SSE 路由**

在 `main.go` 中添加：
```go
import "github.com/codex2api/compat/sse"

func (h *ImagesHandler) ImageGenerationsSSE(c *gin.Context) {
    writer, err := sse.NewStreamWriter(c.Writer)
    if err != nil {
        c.AbortWithStatus(http.StatusInternalServerError)
        return
    }
    
    // 发送进度事件
    writer.WriteEvent("progress", sse.ImageProgressEvent{
        Type:    "progress",
        Percent: 0,
        Message: "开始生成图片",
    })
    
    // ... 生成逻辑 ...
    
    // 发送完成事件
    writer.WriteEvent("done", sse.ImageProgressEvent{
        Type:    "done",
        Message: "生成完成",
    })
}

func main() {
    router.POST("/v1/images/generations-sse", handler.ImageGenerationsSSE)
}
```

**步骤 2: 测试 SSE 流**

```javascript
// 在浏览器控制台中运行
const eventSource = new EventSource('/v1/images/generations-sse');

eventSource.addEventListener('progress', (e) => {
    const data = JSON.parse(e.data);
    console.log(`进度: ${data.percent}% - ${data.message}`);
});

eventSource.addEventListener('image', (e) => {
    const data = JSON.parse(e.data);
    console.log(`图片完成: ${data.url}`);
});

eventSource.addEventListener('done', (e) => {
    console.log('全部完成');
    eventSource.close();
});
```

**验证点**:
- ✅ 实时接收进度事件
- ✅ 实时接收图片完成事件
- ✅ 接收完成事件
- ✅ 事件延迟 <100ms

---

### 2.4 Phase 4: RT/ST 双路径刷新

#### 验证方法 1: 单元测试 ✅

```bash
cd D:/cc/my-project/codex2api
go test ./compat/refresh -v
```

**测试结果**:
```
✅ TestParseJWTExp - 测试 JWT 过期时间解析
✅ TestParseJWTExpInvalid - 测试无效 JWT 处理
✅ TestFriendlyRefreshErr - 测试错误友好化
✅ TestTruncate - 测试字符串截断
✅ TestNewRefresher - 测试刷新器初始化
✅ TestRefreshAutoEmptyTokens - 测试空 Token 处理
```

#### 验证方法 2: 集成测试（需要启动服务）

**步骤 1: 创建刷新器**

```go
import "github.com/codex2api/compat/refresh"

func main() {
    // 创建刷新器
    refresher := refresh.NewRefresher("your-client-id")
    
    // 刷新账号
    result, err := refresher.RefreshAuto(
        context.Background(),
        accountID,
        email,
        refreshToken,  // RT
        sessionToken,  // ST
    )
    
    if result.OK {
        log.Printf("刷新成功: source=%s expires_at=%v", 
            result.Source, result.ExpiresAt)
    }
}
```

**步骤 2: 测试刷新逻辑**

```bash
# 测试 RT 刷新
curl -X POST http://localhost:8080/api/accounts/123/refresh \
  -H "Content-Type: application/json" \
  -d '{
    "refresh_token": "your-rt-token"
  }'

# 预期响应
{
  "ok": true,
  "source": "rt",
  "expires_at": "2026-04-29T15:30:00Z",
  "at_verified": true
}
```

**验证点**:
- ✅ RT 刷新成功（source=rt）
- ✅ RT 失败回退 ST（source=st）
- ✅ Web 作用域验证（at_verified=true）
- ✅ 刷新时间 <1秒

---

## 🚀 三、完整启动和测试流程

### 3.1 准备工作

```bash
# 1. 切换到项目目录
cd D:/cc/my-project/codex2api

# 2. 安装依赖（已完成）
go mod tidy

# 3. 运行所有单元测试
go test ./compat/... -v
```

### 3.2 初始化兼容层

在 `main.go` 中添加初始化代码：

```go
package main

import (
    "github.com/codex2api/compat"
    "github.com/codex2api/compat/proxy"
    "github.com/codex2api/compat/refresh"
    "github.com/gin-gonic/gin"
)

func main() {
    // 初始化兼容层
    compat.InitCompat(true, 512, 4)
    
    router := gin.Default()
    
    // 注册代理路由
    proxyHandler := proxy.NewProxyHandler(&AssetResolverImpl{})
    router.GET("/p/img/:asset_id", proxyHandler.HandleImageProxy)
    
    // 注册 SSE 路由（可选）
    router.POST("/v1/images/generations-sse", handler.ImageGenerationsSSE)
    
    // ... 原有路由 ...
    
    router.Run(":8080")
}
```

### 3.3 启动服务

```bash
cd D:/cc/my-project/codex2api
go run main.go
```

**预期输出**:
```
[compat] 图片兼容层已初始化: cache=512MB concurrency=4
[GIN-debug] GET    /p/img/:asset_id          --> proxy.HandleImageProxy
[GIN-debug] POST   /v1/images/generations-sse --> handler.ImageGenerationsSSE
[GIN-debug] Listening and serving HTTP on :8080
```

### 3.4 使用 HTML 测试页面

```bash
# 打开测试页面
start D:/cc/my-project/codex2api/compat/examples/test.html
```

**测试步骤**:
1. 输入 API 地址：`http://localhost:8080`
2. 输入提示词：`a beautiful sunset over mountains`
3. 选择分辨率：2K 高清
4. 点击"生成图片"
5. 观察进度和结果

---

## ✅ 验证清单

### Phase 1: 2K/4K 图片放大
- [x] 单元测试全部通过
- [ ] API 调用返回正确分辨率
- [ ] 缓存机制正常工作
- [ ] 首次放大时间 +1-2秒
- [ ] 缓存命中时间 <10ms

### Phase 2: HMAC 防盗链
- [x] 单元测试全部通过
- [ ] 代理 URL 生成正确
- [ ] 有效签名返回图片
- [ ] 无效签名返回 403
- [ ] 过期签名返回 403

### Phase 3: SSE 流式响应
- [x] 单元测试全部通过
- [ ] 实时接收进度事件
- [ ] 实时接收图片事件
- [ ] 事件延迟 <100ms

### Phase 4: RT/ST 刷新
- [x] 单元测试全部通过
- [ ] RT 刷新成功
- [ ] RT 失败回退 ST
- [ ] Web 作用域验证
- [ ] 刷新时间 <1秒

---

## 📞 故障排查

### 问题 1: 编译错误

```bash
# 清理缓存重新编译
go clean -cache
go mod tidy
go build ./...
```

### 问题 2: 测试失败

```bash
# 查看详细错误
go test ./compat/image -v -run TestDoUpscale
```

### 问题 3: 服务启动失败

```bash
# 检查端口占用
netstat -ano | findstr :8080

# 检查依赖
go mod verify
```

### 问题 4: API 调用失败

```bash
# 检查服务日志
# 查看 [upscale] 相关日志
# 查看 [proxy] 相关日志
```

---

## 📚 相关文档

- [完整报告](D:/cc/my-project/codex2api/compat/SOLUTION_A_FINAL_REPORT.md)
- [快速开始](D:/cc/my-project/codex2api/compat/QUICKSTART.md)
- [集成指南](D:/cc/my-project/codex2api/compat/INTEGRATION.md)
- [前端集成](D:/cc/my-project/codex2api/compat/FRONTEND_INTEGRATION.md)

---

**文档生成时间**: 2026-04-28  
**状态**: ✅ 所有单元测试通过，等待集成测试
