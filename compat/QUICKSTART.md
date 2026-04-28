# 🚀 快速开始：5 分钟集成 2K/4K 图片放大

## ✅ 前置条件

- Go 1.25+
- codex2api 项目已克隆
- 网络正常（用于下载依赖）

## 📦 步骤 1：安装依赖（1 分钟）

```bash
cd /d/cc/my-project/codex2api

# 配置 Go 代理（如果需要）
go env -w GOPROXY=https://goproxy.cn,direct

# 安装依赖
go mod tidy

# 验证编译
go build ./...
```

## 🔧 步骤 2：初始化兼容层（1 分钟）

在 `main.go` 或程序入口添加：

```go
import (
    "github.com/codex2api/compat"
)

func main() {
    // 初始化兼容层
    compat.InitCompat(
        true,  // 启用放大功能
        512,   // 缓存大小 512MB
        4,     // 并发数 4
    )
    
    // ... 原有代码 ...
}
```

## ✅ 步骤 3：验证集成（1 分钟）

运行测试：

```bash
cd compat/image
go test -v
```

预期输出：
```
=== RUN   TestValidateUpscale
--- PASS: TestValidateUpscale (0.00s)
=== RUN   TestDoUpscale
--- PASS: TestDoUpscale (0.01s)
=== RUN   TestUpscaleCache
--- PASS: TestUpscaleCache (0.00s)
PASS
ok      github.com/codex2api/compat/image       0.015s
```

## 🎨 步骤 4：前端调用（2 分钟）

### JavaScript 示例

```javascript
// 生成 2K 高清图片
const response = await fetch('/api/admin/image-studio/jobs', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer YOUR_API_KEY'
  },
  body: JSON.stringify({
    prompt: "a beautiful sunset",
    model: "gpt-image-2",
    upscale: "2k"  // 🆕 新增参数
  })
});

const result = await response.json();
console.log('生成成功:', result.job.assets[0].actual_size); // "2560x2560"
```

### cURL 测试

```bash
curl -X POST http://localhost:8080/api/admin/image-studio/jobs \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_API_KEY" \
  -d '{
    "prompt": "a beautiful sunset",
    "model": "gpt-image-2",
    "upscale": "2k"
  }'
```

## 🎯 完成！

现在你已经成功集成了 2K/4K 图片放大功能！

### 下一步

- 📖 阅读 [前端集成指南](./FRONTEND_INTEGRATION.md) 了解完整 API
- 🔧 查看 [集成指南](./INTEGRATION.md) 了解高级配置
- 📊 查看 [性能调优](./INTEGRATION.md#性能调优) 优化缓存设置

## 🐛 遇到问题？

### 问题 1：依赖下载失败

```bash
# 配置代理
go env -w GOPROXY=https://goproxy.cn,direct
go mod tidy
```

### 问题 2：编译错误

```bash
# 清理缓存重新编译
go clean -cache
go mod tidy
go build ./...
```

### 问题 3：测试失败

```bash
# 查看详细错误
cd compat/image
go test -v -run TestDoUpscale
```

## 📚 完整文档

- [README.md](./README.md) - 兼容层总览
- [INTEGRATION.md](./INTEGRATION.md) - 详细集成指南
- [FRONTEND_INTEGRATION.md](./FRONTEND_INTEGRATION.md) - 前端 API 文档
- [CHANGELOG.md](./CHANGELOG.md) - 变更日志

## 💡 示例代码

查看 `compat/examples/` 目录获取完整示例：
- React 组件示例
- Vue 组件示例
- 原生 JavaScript 示例
- cURL 测试脚本
