# Codex 大请求 413 诊断

可以通过环境变量配置单请求上限和逻辑正文总预算，例如：

```yaml
environment:
  CODEX_MAX_REQUEST_BODY_SIZE_MB: "96"
  CODEX_REQUEST_MEMORY_BUDGET_MB: "192"
```

这两个设置在容器重建后生效。预算包含压缩正文与解压正文，因此接近上限的
低压缩率请求或并发请求仍可能返回可重试的 503。预算不包含所有 JSON 工作
副本和 Go 堆开销，不是进程 RSS 上限。应结合小内存主机实际负载评估。

HTTP 响应新增 `X-Codex2API-Max-Request-Body-Bytes`，可从 `/health`
确认本机生效限制。413 错误保留 `invalid_request_error` /
`request_too_large`，并包含请求 ID、上限、观察到的大小和触发阶段。

大小中间件在访问日志之前拒绝请求；本补丁直接写 `[request-size]` JSON
日志，覆盖 Content-Length 超限、未知长度正文超限、解压后超限以及 zstd
解码窗口超限。日志不包含正文、Authorization、查询参数或客户端 URL。

- `content_length`：声明的长度，未读取正文。
- `wire_body` / `decompressed_body`：最多读取到上限加一字节；记录下界，
  不能当作完整大小。
- `decoder_window`：zstd 解码器的窗口或内存限制；实际解压大小未知，
  `observed_body_bytes=-1`，`observed_is_lower_bound=false`。

补丁不裁剪会话、图片或工具历史。超过配置上限时仍应使用更短的会话或减少
附件。入站通过不代表上游接受该请求，也不保证模型上下文窗口足够。
