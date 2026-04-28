# main.go 初始化示例

## 在 main.go 中集成兼容层

在项目的 `main.go` 文件中添加以下代码来初始化兼容层：

```go
package main

import (
    "log"
    
    "github.com/codex2api/compat"
    // ... 其他导入
)

func main() {
    // 初始化兼容层（在其他初始化之前调用）
    compat.InitCompat(
        true,  // enableUpscale: 启用图片放大功能
        512,   // cacheMB: 缓存大小 512MB
        4,     // concurrency: 并发数 4
    )
    
    log.Println("兼容层初始化完成")
    
    // ... 原有的初始化代码
    // 例如：启动 HTTP 服务器、连接数据库等
}
```

## 参数说明

| 参数 | 类型 | 说明 | 推荐值 |
|------|------|------|--------|
| `enableUpscale` | bool | 是否启用图片放大功能 | `true` |
| `cacheMB` | int64 | LRU 缓存大小（MB） | `512` (可根据服务器内存调整) |
| `concurrency` | int | 并发放大任务数 | `4` (可根据 CPU 核心数调整) |

## 性能调优建议

### 缓存大小调整

```go
// 小型服务器（2GB 内存）
compat.InitCompat(true, 256, 2)

// 中型服务器（8GB 内存）
compat.InitCompat(true, 512, 4)

// 大型服务器（16GB+ 内存）
compat.InitCompat(true, 1024, 8)
```

### 并发数调整

```go
import "runtime"

func main() {
    // 根据 CPU 核心数自动调整
    cpuCount := runtime.NumCPU()
    concurrency := cpuCount / 2
    if concurrency < 2 {
        concurrency = 2
    }
    
    compat.InitCompat(true, 512, concurrency)
    
    // ... 其他代码
}
```

## 禁用放大功能

如果不需要图片放大功能，可以禁用：

```go
func main() {
    // 禁用放大功能
    compat.InitCompat(false, 0, 0)
    
    // 或者直接不调用 InitCompat()
    // 此时 API 中的 upscale 参数会被忽略
}
```

## 完整示例

```go
package main

import (
    "log"
    "net/http"
    "runtime"
    
    "github.com/codex2api/compat"
    "github.com/gin-gonic/gin"
)

func main() {
    // 1. 初始化兼容层
    cpuCount := runtime.NumCPU()
    compat.InitCompat(
        true,
        512,
        cpuCount/2,
    )
    log.Println("✅ 兼容层初始化完成")
    
    // 2. 初始化 Web 框架
    router := gin.Default()
    
    // 3. 注册路由
    // ... 注册你的 API 路由
    
    // 4. 启动服务器
    log.Println("🚀 服务器启动在 :8080")
    if err := router.Run(":8080"); err != nil {
        log.Fatalf("服务器启动失败: %v", err)
    }
}
```

## 验证初始化

启动服务后，查看日志应该看到：

```
[compat] 图片兼容层已初始化: cache=512MB concurrency=4
✅ 兼容层初始化完成
```

## 故障排查

### 问题 1：编译错误 "undefined: compat"

**解决**：确保已运行 `go mod tidy` 安装依赖

```bash
cd /d/cc/my-project/codex2api
go mod tidy
```

### 问题 2：运行时错误 "panic: runtime error"

**解决**：检查参数是否合理，缓存大小不要超过可用内存

### 问题 3：放大功能不生效

**解决**：
1. 确认 `enableUpscale` 参数为 `true`
2. 检查 API 请求中是否正确传递了 `upscale` 参数
3. 查看服务器日志是否有错误信息

## 下一步

初始化完成后：

1. 启动服务：`go run main.go`
2. 使用测试页面：打开 `compat/examples/test.html`
3. 测试 API：参考 `compat/FRONTEND_INTEGRATION.md`
