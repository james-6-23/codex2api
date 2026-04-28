# Phase 3: SSE 直出优化

**完成时间**: 2026-04-28  
**状态**: ⚠️ 可选功能（当前 codex2api 不需要）

---

## 📊 功能概述

SSE（Server-Sent Events）直出优化主要用于流式响应场景。经过分析：

- **gpt2api**: SSE 用于聊天流式输出（`text/event-stream`）
- **codex2api**: 图片生成是同步返回，不需要 SSE

---

## 🎯 评估结论

### 当前状态
codex2api 的图片生成流程：
1. 接收请求 → 2. 调用 OpenAI API → 3. 等待生成完成 → 4. 返回图片 URL

这是**同步模式**，不涉及流式输出，因此 SSE 优化不适用。

### 何时需要 SSE

如果未来 codex2api 需要以下功能，才需要 SSE：

1. **流式生成进度**: 实时推送生成进度（0% → 25% → 50% → 100%）
2. **多张图片流式返回**: 生成 4 张图时，每完成一张立即推送
3. **实时日志流**: 推送生成过程的详细日志

---

## 📁 预留接口设计

如果未来需要 SSE，可以参考以下设计：

### 1. SSE 响应格式

```go
// SSE 事件类型
const (
    EventTypeProgress = "progress"  // 进度更新
    EventTypeImage    = "image"     // 图片完成
    EventTypeError    = "error"     // 错误
    EventTypeDone     = "done"      // 全部完成
)

// SSE 消息格式
type SSEMessage struct {
    Event string      `json:"event"`
    Data  interface{} `json:"data"`
}

// 进度数据
type ProgressData struct {
    Percent int    `json:"percent"`
    Message string `json:"message"`
}

// 图片数据
type ImageData struct {
    Index int    `json:"index"`
    URL   string `json:"url"`
}
```

### 2. SSE 处理器示例

```go
func (h *ImagesHandler) ImageGenerationsSSE(c *gin.Context) {
    // 设置 SSE 响应头
    c.Header("Content-Type", "text/event-stream")
    c.Header("Cache-Control", "no-cache")
    c.Header("Connection", "keep-alive")
    
    // 创建 SSE 写入器
    flusher, ok := c.Writer.(http.Flusher)
    if !ok {
        c.AbortWithStatus(http.StatusInternalServerError)
        return
    }
    
    // 发送进度事件
    sendSSE := func(event string, data interface{}) {
        msg := SSEMessage{Event: event, Data: data}
        jsonData, _ := json.Marshal(msg)
        fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, jsonData)
        flusher.Flush()
    }
    
    // 示例：发送进度
    sendSSE(EventTypeProgress, ProgressData{Percent: 0, Message: "开始生成"})
    sendSSE(EventTypeProgress, ProgressData{Percent: 50, Message: "生成中"})
    
    // 示例：发送图片
    sendSSE(EventTypeImage, ImageData{Index: 0, URL: "https://..."})
    
    // 示例：完成
    sendSSE(EventTypeDone, nil)
}
```

### 3. 前端调用示例

```javascript
// 使用 EventSource 接收 SSE
const eventSource = new EventSource('/v1/images/generations-sse');

eventSource.addEventListener('progress', (e) => {
    const data = JSON.parse(e.data);
    console.log(`进度: ${data.percent}% - ${data.message}`);
});

eventSource.addEventListener('image', (e) => {
    const data = JSON.parse(e.data);
    console.log(`图片 ${data.index} 完成: ${data.url}`);
    displayImage(data.url);
});

eventSource.addEventListener('done', (e) => {
    console.log('全部完成');
    eventSource.close();
});

eventSource.addEventListener('error', (e) => {
    console.error('生成失败', e);
    eventSource.close();
});
```

---

## 🔧 实现建议

如果未来需要实现 SSE，建议：

### 方案 1: 轮询改造（简单）
```go
// 1. 生成任务时返回 task_id
// 2. 前端轮询 GET /v1/images/tasks/:id
// 3. 后端返回当前状态和进度
```

**优点**: 
- 实现简单
- 兼容性好
- 无需改动现有架构

**缺点**:
- 延迟较高（轮询间隔）
- 服务器压力大（频繁请求）

### 方案 2: SSE 流式推送（推荐）
```go
// 1. POST /v1/images/generations-sse 返回 SSE 流
// 2. 后端实时推送进度和结果
// 3. 前端 EventSource 接收事件
```

**优点**:
- 实时性好
- 服务器压力小
- 用户体验好

**缺点**:
- 实现复杂
- 需要长连接支持
- 需要改造现有架构

### 方案 3: WebSocket（高级）
```go
// 1. 建立 WebSocket 连接
// 2. 双向通信（可取消任务）
// 3. 实时推送进度和结果
```

**优点**:
- 双向通信
- 可取消任务
- 最佳用户体验

**缺点**:
- 实现最复杂
- 需要 WebSocket 支持
- 需要大幅改造

---

## 📊 性能对比

| 方案 | 实时性 | 服务器压力 | 实现复杂度 | 推荐度 |
|------|--------|-----------|-----------|--------|
| 轮询 | ⭐⭐ | ⭐ | ⭐⭐⭐ | ⭐⭐ |
| SSE | ⭐⭐⭐ | ⭐⭐⭐ | ⭐⭐ | ⭐⭐⭐ |
| WebSocket | ⭐⭐⭐ | ⭐⭐⭐ | ⭐ | ⭐⭐ |

---

## 🎯 结论

### 当前建议
**不实现 SSE**，原因：
1. codex2api 图片生成是同步模式
2. 生成时间通常 10-15 秒，用户可以等待
3. 增加 SSE 会增加复杂度，收益不大

### 未来建议
如果出现以下需求，再考虑 SSE：
1. 生成时间超过 30 秒，用户需要进度反馈
2. 支持批量生成（10+ 张图），需要流式返回
3. 需要实时日志调试功能

---

## 🔗 相关文档

- [Phase 1: 2K/4K 本地放大](../PHASE1_COMPLETION_REPORT.md)
- [Phase 2: HMAC 防盗链代理](../PHASE2_COMPLETION_REPORT.md)
- [Phase 4: RT/ST 双路径刷新](../PHASE4_COMPLETION_REPORT.md)

---

**报告生成时间**: 2026-04-28  
**状态**: ⚠️ Phase 3 评估完成，当前不需要实现
