import type { EndpointSpec, ParameterSpec } from "./docsContent";
import type { DocsLocale } from "./quickStartTools";

export function addEndpointDetails(
  endpoint: EndpointSpec,
  locale: DocsLocale,
): EndpointSpec {
  const c = (zh: string, en: string) => (locale === "zh" ? zh : en);
  const p = (
    name: string,
    type: string,
    required: boolean,
    description: string,
    defaultValue?: string,
  ): ParameterSpec => ({ name, type, required, description, defaultValue });
  const model = p(
    "model",
    "string",
    true,
    c(
      "使用当前 /v1/models 中且密钥有权调用的模型。",
      "Use a model available to your key in /v1/models.",
    ),
  );
  const stream = p(
    "stream",
    "boolean",
    false,
    c(
      "true 返回 SSE；false 返回普通 JSON。",
      "true returns SSE; false returns JSON.",
    ),
    "false",
  );
  const tier = p(
    "service_tier",
    "string",
    false,
    c(
      "Codex: fast → priority；ultrafast 保留；default/auto 不指定档位。",
      "Codex: fast maps to priority; ultrafast is retained; default/auto do not force a tier.",
    ),
  );
  const input = p(
    "input",
    "string | array",
    true,
    c(
      "输入文本或带 role/content 的历史项；续轮保留工具项。",
      "Text or history items with role/content; preserve tool items on later turns.",
    ),
  );
  const messages = p(
    "messages",
    "array",
    true,
    c(
      "对话历史，包含 role 与 content。",
      "Conversation history with role and content.",
    ),
  );
  const params: Record<string, ParameterSpec[]> = {
    "api-responses": [
      model,
      input,
      stream,
      p(
        "reasoning.effort",
        "string",
        false,
        c(
          "none/minimal/low/medium/high/xhigh/ultra；max 按模型支持归一。",
          "none/minimal/low/medium/high/xhigh/ultra; max is normalized by model capability.",
        ),
      ),
      tier,
      p(
        "previous_response_id",
        "string",
        false,
        c(
          "同一密钥范围内的上一响应 ID，依赖可恢复的上下文。",
          "Previous response ID in the same key scope; requires recoverable context.",
        ),
      ),
      p(
        "tools",
        "array",
        false,
        c(
          "函数、custom 或支持的内置工具。",
          "Functions, custom tools or supported built-in tools.",
        ),
      ),
      p(
        "text.format",
        "object",
        false,
        c(
          "结构化输出格式；例如 type=json_schema。",
          "Structured output format, such as type=json_schema.",
        ),
      ),
    ],
    "api-chat": [
      model,
      messages,
      stream,
      p(
        "reasoning_effort",
        "string",
        false,
        c(
          "Chat 顶层思考强度，按模型支持归一。",
          "Top-level Chat reasoning effort, normalized by model capability.",
        ),
      ),
      tier,
      p(
        "tools",
        "array",
        false,
        c(
          "函数或 custom 工具；结果用 tool_call_id 配对。",
          "Function/custom tools; pair results using tool_call_id.",
        ),
      ),
      p(
        "response_format",
        "object",
        false,
        c(
          "JSON 或 JSON Schema 输出约束。",
          "JSON or JSON Schema output constraints.",
        ),
      ),
    ],
    "api-messages": [
      model,
      messages,
      p(
        "max_tokens",
        "integer",
        true,
        c(
          "期望最大输出长度；原生与转换路径行为不同。",
          "Requested output limit; native and translated paths differ.",
        ),
      ),
      stream,
      p(
        "system",
        "string | array",
        false,
        c("系统提示词。", "System instructions."),
      ),
      p(
        "tools",
        "array",
        false,
        c(
          "Anthropic 工具定义，参数放在 input_schema。",
          "Anthropic tool definitions with input_schema.",
        ),
      ),
    ],
    "api-images-gen": [
      p(
        "model",
        "string",
        false,
        c(
          "gpt-image-2、gpt-image-2.5-flare/sunburst 或可用 Grok Imagine 模型。",
          "gpt-image-2, gpt-image-2.5-flare/sunburst or an available Grok Imagine model.",
        ),
        "gpt-image-2",
      ),
      p("prompt", "string", true, c("图片描述。", "Image prompt.")),
      p(
        "size",
        "string",
        false,
        c(
          "宽x高；支持模型档位别名 -2k/-4k。",
          "WIDTHxHEIGHT; gateway aliases -2k/-4k select size tiers.",
        ),
      ),
      p(
        "quality",
        "string",
        false,
        c(
          "2.5: auto/low/medium/high/xhigh/max；旧型号按其能力选择。",
          "2.5: auto/low/medium/high/xhigh/max; use supported levels for older models.",
        ),
      ),
      p("output_format", "string", false, "png / jpeg / webp"),
      p(
        "response_format",
        "string",
        false,
        c(
          "b64_json 或 url。配置对象存储时 url 返回限时链接，否则为 data URL。",
          "b64_json or url. With object storage, url is a temporary link; otherwise it is a data URL.",
        ),
        "b64_json",
      ),
    ],
    "api-images-edit": [
      p(
        "model",
        "string",
        false,
        c("可用的图片模型。", "An available image model."),
        "gpt-image-2",
      ),
      p(
        "prompt",
        "string",
        true,
        c("描述需要修改的内容。", "Describe the desired edit."),
      ),
      p(
        "images[].image_url",
        "string",
        true,
        c(
          "JSON 图片 URL 或 data URL；multipart 改用 image/image[] 文件字段。",
          "JSON image URL or data URL; multipart uses image/image[] files instead.",
        ),
      ),
      p(
        "mask.image_url",
        "string",
        false,
        c(
          "可选遮罩，仅 gpt-image 系列支持；multipart 使用 mask 文件。",
          "Optional mask for gpt-image models; multipart uses a mask file.",
        ),
      ),
      p(
        "size / quality / output_format",
        "string",
        false,
        c(
          "与图片生成参数相同，按模型能力选择。",
          "As in image generation; choose values supported by the model.",
        ),
      ),
    ],
  };
  return {
    ...endpoint,
    category:
      endpoint.category ?? (endpoint.id.includes("images") ? "media" : "text"),
    parameters: endpoint.parameters ?? params[endpoint.id],
  };
}

export function buildExtraEndpoints(
  baseUrl: string,
  locale: DocsLocale,
): EndpointSpec[] {
  const c = (zh: string, en: string) => (locale === "zh" ? zh : en);
  const json = (v: unknown) => JSON.stringify(v, null, 2);
  const param = (
    name: string,
    type: string,
    required: boolean,
    description: string,
  ): ParameterSpec => ({ name, type, required, description });
  const post = (path: string, body: unknown) =>
    `curl --request POST '${baseUrl}${path}' \\\n  --header 'Authorization: Bearer YOUR_API_KEY' \\\n  --header 'Content-Type: application/json' \\\n  --data '${json(body)}'`;
  const get = (path: string) =>
    `curl '${baseUrl}${path}' \\\n  --header 'Authorization: Bearer YOUR_API_KEY'`;
  const imageJob = {
    model: "gpt-image-2.5-flare",
    prompt: "A small orange cat",
    size: "1024x1024",
    quality: "high",
    n: 1,
  };
  const videoBody = {
    model: "grok-imagine-video-1.5",
    prompt: "Ocean waves at sunset",
    duration: 4,
    resolution: "480p",
  };
  const endpoints: EndpointSpec[] = [
    {
      id: "api-image-jobs",
      category: "media",
      method: "POST",
      path: "/v1/images/jobs",
      title: c("创建异步图片任务", "Create an async image job"),
      description: c(
        "任务进入后台队列，返回 202。仅接受后台创建的 API Key；input_images 非空时执行图片编辑。",
        "Queues a background job and returns 202. Requires a dashboard-created API key; non-empty input_images selects editing.",
      ),
      defaultBody: json(imageJob),
      curl: post("/v1/images/jobs", imageJob),
      parameters: [
        param(
          "prompt",
          "string",
          true,
          c(
            "图片描述，最多 8000 字符。",
            "Image prompt, up to 8000 characters.",
          ),
        ),
        param(
          "model / size / quality",
          "string",
          false,
          c("图片模型及生成参数。", "Image model and generation options."),
        ),
        param(
          "input_images",
          "string[]",
          false,
          c(
            "编辑源图 URL 或 data URL。",
            "Source image URLs or data URLs for editing.",
          ),
        ),
        param(
          "n",
          "integer",
          false,
          c("输出数量，默认 1。", "Output count, default 1."),
        ),
      ],
      responses: [
        {
          code: 202,
          body: json({
            job: { id: 42, status: "queued", prompt: imageJob.prompt },
          }),
        },
      ],
    },
    {
      id: "api-image-job-status",
      category: "media",
      method: "GET",
      path: "/v1/images/jobs/:id",
      title: c("查询图片任务", "Get an image job"),
      description: c(
        "替换 :id 为 job.id，使用同一 API Key。根据 queued/running/succeeded/failed 判断状态；成功后使用 job.assets[].proxy_url 下载图片，相对路径按本服务地址解析；失败时查看 error_message。",
        "Replace :id with job.id and use the creating API key. Inspect queued/running/succeeded/failed. Download successful assets using job.assets[].proxy_url, resolving relative paths against this service URL; inspect error_message on failure.",
      ),
      curl: get("/v1/images/jobs/42"),
      parameters: [
        param(
          "id",
          "path integer",
          true,
          c("创建响应中的 job.id。", "job.id from the create response."),
        ),
      ],
      responses: [
        {
          code: 200,
          label: "running",
          body: json({ job: { id: 42, status: "running" } }),
        },
        {
          code: 200,
          label: "succeeded",
          body: json({
            job: {
              id: 42,
              status: "succeeded",
              assets: [
                {
                  id: 7,
                  mime_type: "image/png",
                  proxy_url: "<signed-download-url>",
                },
              ],
            },
          }),
        },
        {
          code: 404,
          body: json({ error: { message: "Image job not found" } }),
        },
      ],
    },
    {
      id: "api-videos-gen",
      category: "media",
      method: "POST",
      path: "/v1/videos/generations",
      title: c("创建视频任务", "Generate a video"),
      description: c(
        "需要付费 Grok 账号。返回 request_id，随后查询任务状态。省略模型时使用 grok-imagine-video-1.5。",
        "Requires a paid Grok account. Returns request_id for polling. An omitted model defaults to grok-imagine-video-1.5.",
      ),
      defaultBody: json(videoBody),
      curl: post("/v1/videos/generations", videoBody),
      parameters: [
        param(
          "model",
          "string",
          false,
          "grok-imagine-video / grok-imagine-video-1.5",
        ),
        param(
          "prompt",
          "string",
          false,
          c("无图片输入时必填。", "Required without image input."),
        ),
        param(
          "duration",
          "integer",
          false,
          c("1–15 秒，默认 8 秒。", "1–15 seconds, default 8."),
        ),
        param(
          "resolution",
          "string",
          false,
          c(
            "480p/720p/1080p；1080p 仅 1.5 且无参考图。",
            "480p/720p/1080p; 1080p requires 1.5 and no reference images.",
          ),
        ),
        param(
          "image",
          "object",
          false,
          c(
            "首帧图片，格式 {url: ...}；与 reference_images 互斥。",
            "First frame as {url: ...}; mutually exclusive with reference_images.",
          ),
        ),
        param(
          "reference_images",
          "array",
          false,
          c("最多 7 张参考图。", "Up to 7 reference images."),
        ),
      ],
      responses: [{ code: 200, body: json({ request_id: "YOUR_REQUEST_ID" }) }],
    },
    ...(["edits", "extensions"] as const).map((operation): EndpointSpec => {
      const body = {
        model: "grok-imagine-video",
        prompt:
          operation === "edits"
            ? "Change the background to a beach"
            : "Continue the scene",
        video: { url: "https://example.com/source.mp4" },
      };
      return {
        id: `api-videos-${operation}`,
        category: "media",
        method: "POST",
        path: `/v1/videos/${operation}`,
        title:
          operation === "edits"
            ? c("编辑视频", "Edit a video")
            : c("延展视频", "Extend a video"),
        description: c(
          "使用 grok-imagine-video；1.5 不支持该操作。返回 request_id，按视频任务流程查询和下载。",
          "Use grok-imagine-video; 1.5 does not support this operation. Poll and download using the returned request_id.",
        ),
        curl: post(`/v1/videos/${operation}`, body),
        defaultBody: json(body),
        parameters: [
          param(
            "video",
            "object",
            true,
            c("源视频 {url: ...}。", "Source video as {url: ...}."),
          ),
          param(
            "prompt",
            "string",
            false,
            c("修改或续写要求。", "Edit or continuation instructions."),
          ),
        ],
        responses: [
          { code: 200, body: json({ request_id: "YOUR_REQUEST_ID" }) },
        ],
      };
    }),
    {
      id: "api-video-status",
      category: "media",
      method: "GET",
      path: "/v1/videos/:request_id",
      title: c("查询视频任务", "Get video status"),
      description: c(
        "每隔 2–5 秒查询，使用创建时的同一 API Key。状态为 pending/done/failed/expired；任务绑定有效期 24 小时。Memory 模式重启后绑定丢失。",
        "Poll every 2–5 seconds with the creating API key. Status is pending/done/failed/expired. Bindings expire after 24 hours and are lost on restart in Memory mode.",
      ),
      curl: get("/v1/videos/YOUR_REQUEST_ID"),
      responses: [
        {
          code: 200,
          label: "pending",
          body: json({ status: "pending", progress: 42 }),
        },
        {
          code: 200,
          label: "done",
          body: json({
            status: "done",
            progress: 100,
            video: {
              url: `${baseUrl}/v1/videos/YOUR_REQUEST_ID/content`,
              duration: 4,
            },
          }),
        },
      ],
    },
    {
      id: "api-video-content",
      category: "media",
      method: "GET",
      path: "/v1/videos/:request_id/content",
      title: c("下载视频", "Download a video"),
      description: c(
        "任务完成后返回 video/mp4，支持 Range 部分下载。使用创建任务时的 API Key。",
        "Returns video/mp4 after completion and supports Range downloads. Use the creating API key.",
      ),
      curl: `${get("/v1/videos/YOUR_REQUEST_ID/content")} \\\n  --output video.mp4`,
      responses: [
        {
          code: 200,
          label: "video/mp4",
          lang: "text",
          body: c(
            "MP4 二进制内容（保存为文件）",
            "MP4 binary data (save to a file)",
          ),
        },
      ],
    },
    ...(["messages/count_tokens", "responses/input_tokens"] as const).map(
      (path): EndpointSpec => {
        const body = path.startsWith("messages")
          ? {
              model: "claude-sonnet-4-5",
              messages: [{ role: "user", content: "Hello" }],
            }
          : { model: "gpt-5.5", input: "Hello" };
        return {
          id: path.startsWith("messages")
            ? "api-count-tokens"
            : "api-input-tokens",
          category: "advanced",
          method: "POST",
          path: `/v1/${path}`,
          title: path.startsWith("messages")
            ? c("Messages Token 估算", "Estimate Messages tokens")
            : c("Responses Token 估算", "Estimate Responses tokens"),
          description: c(
            "仅做本地字符估算，不调用上游、不消耗账号额度。用于客户端兼容探测，不是精确 tokenizer，也不是最终计费依据。",
            "A local character-based estimate without upstream calls or account quota consumption. For compatibility probes, not an exact tokenizer or billing measurement.",
          ),
          curl: post(`/v1/${path}`, body),
          defaultBody: json(body),
          responses: [{ code: 200, body: json({ input_tokens: 32 }) }],
        };
      },
    ),
    {
      id: "api-compact",
      category: "advanced",
      method: "POST",
      path: "/v1/responses/compact",
      title: c("压缩 Responses 上下文", "Compact Responses context"),
      description: c(
        "提交完整必需历史，获得可供后续请求携带的压缩项。保留返回的 output 项及不透明字段；能力由所选模型和渠道决定。",
        "Submit the complete required history to receive compaction items for later requests. Preserve output items and opaque fields; support depends on model and channel.",
      ),
      curl: post("/v1/responses/compact", {
        model: "gpt-5.5",
        input: [{ role: "user", content: "Summarize this conversation." }],
      }),
      defaultBody: json({
        model: "gpt-5.5",
        input: [{ role: "user", content: "Summarize this conversation." }],
      }),
      responses: [
        {
          code: 200,
          label: c("结构示意", "Shape example"),
          body: json({
            object: "response.compaction",
            output: [{ type: "compaction", encrypted_content: "..." }],
          }),
        },
      ],
    },
    {
      id: "api-responses-ws",
      category: "advanced",
      method: "GET",
      path: "/v1/responses",
      transport: "websocket",
      title: c("Responses WebSocket", "Responses WebSocket"),
      description: c(
        "使用支持自定义认证头的 WebSocket 客户端。连接后发送 response.create，读取增量及终止事件；上下文不可恢复时需重发完整历史。",
        "Use a WebSocket client supporting authentication headers. Send response.create after connecting, then read delta and terminal events. Resend full history if continuation is unavailable.",
      ),
      curl: `# WebSocket URL: ${baseUrl.replace(/^http/, "ws")}/v1/responses\n# Header: Authorization: Bearer YOUR_API_KEY\n# Send this frame after the connection opens:\n${json({ type: "response.create", model: "gpt-5.5", input: [{ role: "user", content: "Hello" }] })}`,
      responses: [
        {
          code: 101,
          label: "WebSocket",
          body: json({ type: "response.output_text.delta", delta: "Hello" }),
        },
      ],
    },
  ];
  return endpoints;
}
