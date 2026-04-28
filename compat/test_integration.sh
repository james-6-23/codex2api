#!/bin/bash
# 完整集成测试脚本

set -e

echo "=========================================="
echo "🚀 Codex2API 兼容层集成测试"
echo "=========================================="
echo ""

# 步骤 1: 安装依赖
echo "📦 步骤 1/5: 安装依赖..."
cd /d/cc/my-project/codex2api
go mod tidy
echo "✅ 依赖安装完成"
echo ""

# 步骤 2: 运行单元测试
echo "🧪 步骤 2/5: 运行单元测试..."
cd compat/image
go test -v
echo "✅ 单元测试通过"
echo ""

# 步骤 3: 编译检查
echo "🔨 步骤 3/5: 编译检查..."
cd /d/cc/my-project/codex2api
go build ./...
echo "✅ 编译成功"
echo ""

# 步骤 4: 测试 API 调用（需要服务运行）
echo "🌐 步骤 4/5: 测试 API 调用..."
echo "请确保 codex2api 服务正在运行..."
echo ""
echo "测试命令："
echo "curl -X POST http://localhost:8080/api/admin/image-studio/jobs \\"
echo "  -H 'Content-Type: application/json' \\"
echo "  -H 'Authorization: Bearer YOUR_API_KEY' \\"
echo "  -d '{\"prompt\":\"test\",\"model\":\"gpt-image-2\",\"upscale\":\"2k\"}'"
echo ""

# 步骤 5: 生成集成报告
echo "📊 步骤 5/5: 生成集成报告..."
cat > /d/cc/my-project/codex2api/compat/INTEGRATION_REPORT.md << 'EOF'
# 集成报告

## ✅ 已完成

### 后端集成
- [x] 添加 `upscale` 字段到 `imageGenerationJobPayload`
- [x] 导入 `compat/image` 包
- [x] 在 `saveImageJobAssets` 中集成放大逻辑
- [x] 添加缓存机制（LRU）
- [x] 添加并发控制
- [x] 添加错误处理和日志

### 兼容层代码
- [x] `compat/image/upscale.go` - 核心放大算法（218行）
- [x] `compat/image/thumb.go` - 缩略图生成（167行）
- [x] `compat/image/config.go` - 配置管理（50行）
- [x] `compat/image/upscale_test.go` - 单元测试（100行）
- [x] `compat/init.go` - 全局初始化

### 文档
- [x] `compat/README.md` - 兼容层总览
- [x] `compat/INTEGRATION.md` - 详细集成指南
- [x] `compat/FRONTEND_INTEGRATION.md` - 前端 API 文档
- [x] `compat/QUICKSTART.md` - 5分钟快速开始
- [x] `compat/CHANGELOG.md` - 变更日志

### 依赖
- [x] 更新 `go.mod` 添加 `golang.org/x/image v0.39.0`

## 🔧 待完成

### 主程序初始化
- [ ] 在 `main.go` 中添加 `compat.InitCompat()` 调用
- [ ] 添加配置文件支持（可选）

### 前端集成
- [ ] 前端添加 `upscale` 参数选择器
- [ ] 前端显示实际分辨率
- [ ] 前端显示文件大小

### 测试验证
- [ ] 启动服务测试 API
- [ ] 验证 2K 放大功能
- [ ] 验证 4K 放大功能
- [ ] 验证缓存命中

## 📝 集成清单

### 后端代码修改

1. **admin/image_studio.go**
   - L3: 添加 `compatImage "github.com/codex2api/compat/image"` 导入
   - L47-58: 添加 `Upscale string` 字段
   - L259: 添加 `req.Upscale = compatImage.ValidateUpscale(req.Upscale)`
   - L651-688: 添加放大逻辑（37行新代码）

2. **go.mod**
   - 添加 `golang.org/x/image v0.39.0` 依赖

3. **main.go**（待添加）
   ```go
   import "github.com/codex2api/compat"

   func main() {
       compat.InitCompat(true, 512, 4)
       // ... 原有代码
   }
   ```

### 前端代码示例

```javascript
// 生成 2K 图片
fetch('/api/admin/image-studio/jobs', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({
    prompt: "a beautiful sunset",
    model: "gpt-image-2",
    upscale: "2k"  // 新增参数
  })
})
```

## 🎯 验证步骤

1. **编译测试**
   ```bash
   cd /d/cc/my-project/codex2api
   go mod tidy
   go build ./...
   ```

2. **单元测试**
   ```bash
   cd compat/image
   go test -v
   ```

3. **启动服务**
   ```bash
   cd /d/cc/my-project/codex2api
   go run main.go
   ```

4. **API 测试**
   ```bash
   # 测试原图
   curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
     -H 'Content-Type: application/json' \
     -d '{"prompt":"test","model":"gpt-image-2"}'

   # 测试 2K
   curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
     -H 'Content-Type: application/json' \
     -d '{"prompt":"test","model":"gpt-image-2","upscale":"2k"}'

   # 测试 4K
   curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
     -H 'Content-Type: application/json' \
     -d '{"prompt":"test","model":"gpt-image-2","upscale":"4k"}'
   ```

## 📊 性能指标

### 预期性能
- 首次 2K 放大: +1s
- 首次 4K 放大: +2s
- 缓存命中: < 10ms
- 内存占用: 512MB 缓存

### 文件大小
- 原图 (1024x1024): 2-5 MB
- 2K (2560x2560): 8-15 MB
- 4K (3840x3840): 20-30 MB

## 🐛 已知问题

无

## 📅 时间线

- 2026-04-28: 阶段 1 完成（2K/4K 本地放大）
- 待定: 阶段 2（HMAC 防盗链）
- 待定: 阶段 3（SSE 直出优化）
- 待定: 阶段 4（RT/ST 双路径刷新）

## 📚 相关文档

- [快速开始](./QUICKSTART.md)
- [集成指南](./INTEGRATION.md)
- [前端集成](./FRONTEND_INTEGRATION.md)
- [变更日志](./CHANGELOG.md)
EOF

echo "✅ 集成报告已生成: compat/INTEGRATION_REPORT.md"
echo ""

echo "=========================================="
echo "✅ 集成测试完成！"
echo "=========================================="
echo ""
echo "📋 下一步操作："
echo "1. 在 main.go 中添加 compat.InitCompat() 调用"
echo "2. 启动服务: go run main.go"
echo "3. 测试 API: 参考 compat/FRONTEND_INTEGRATION.md"
echo "4. 查看完整报告: compat/INTEGRATION_REPORT.md"
echo ""
