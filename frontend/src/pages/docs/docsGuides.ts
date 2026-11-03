import type { DocsLocale } from "./quickStartTools";

export type DocsSection =
  | "quick-start"
  | "development"
  | "model-api"
  | "troubleshooting"
  | "admin-api";
export type GuideSpec = {
  id: string;
  section: DocsSection;
  title: string;
  summary: string;
  paragraphs?: string[];
  table?: { headers: string[]; rows: string[][] };
  examples?: { label: string; lang: string; content: string }[];
  links?: { label: string; href: string }[];
};

export function buildGuides(baseUrl: string, locale: DocsLocale): GuideSpec[] {
  const c = (zh: string, en: string) => (locale === "zh" ? zh : en);
  const base = baseUrl.replace(/\/+$/, "");
  return [
    {
      id: "authentication",
      section: "development",
      title: c("地址与认证", "URLs & authentication"),
      summary: c(
        "先确认请求发给谁，以及使用哪一种密钥。",
        "Choose the destination and the correct credential first.",
      ),
      paragraphs: [
        c(
          "客户端填写的是本服务的访问地址。账号的 proxy_url 是服务访问上游时使用的出站代理，不是客户端 API Base URL。",
          "Clients connect to this service. An account's proxy_url controls the service's outbound proxy; it is not the client API Base URL.",
        ),
        c(
          "公共接口通常使用后台创建的 API Key；管理接口使用 Admin Secret，不需要再叠加一个下游 API Key。仅在未配置任何 API Key 且显式开启 CODEX_ALLOW_ANONYMOUS=true 时，普通公共接口才允许匿名访问。异步生图任务仍要求后台创建的 API Key。",
          "Public endpoints normally use an API key created in the dashboard. Admin endpoints use the Admin Secret without an additional downstream API key. Anonymous access to ordinary public endpoints requires both no configured keys and CODEX_ALLOW_ANONYMOUS=true. Async image jobs still require a dashboard-created API key.",
        ),
      ],
      table: {
        headers: [
          c("用途", "Use"),
          c("地址 / 请求头", "URL / header"),
          c("说明", "Notes"),
        ],
        rows: [
          [
            "Codex CLI",
            base,
            c(
              "本服务兼容无 /v1 的 Responses 路径。",
              "This gateway also accepts Responses paths without /v1.",
            ),
          ],
          [
            "OpenAI SDK",
            `${base}/v1`,
            c(
              "SDK 在 Base URL 后追加 /responses 等资源路径。",
              "The SDK appends resource paths such as /responses.",
            ),
          ],
          [
            "Claude Code",
            base,
            c(
              "ANTHROPIC_BASE_URL；客户端会追加 /v1/messages。",
              "ANTHROPIC_BASE_URL; the client appends /v1/messages.",
            ),
          ],
          [
            c("公共 API", "Public API"),
            "Authorization: Bearer YOUR_API_KEY",
            c(
              "Messages 也接受 x-api-key。",
              "Messages also accepts x-api-key.",
            ),
          ],
          [
            c("管理 API", "Admin API"),
            "X-Admin-Key: YOUR_ADMIN_SECRET",
            c(
              "用于 /api/admin/*，不要填入普通客户端配置。",
              "For /api/admin/*; keep it out of ordinary client configuration.",
            ),
          ],
        ],
      },
      links: [
        {
          label: c("管理 API 密钥", "Manage API keys"),
          href: "/admin/api-keys",
        },
      ],
    },
    {
      id: "protocols",
      section: "development",
      title: c("选择协议与模型", "Protocols & models"),
      summary: c(
        "请求格式、模型名称和上游渠道共同决定实际调用路径。",
        "Request format, model name and upstream channel determine the route.",
      ),
      table: {
        headers: [
          c("接口", "Endpoint"),
          c("适用场景", "Use case"),
          c("关键字段", "Key fields"),
        ],
        rows: [
          [
            "Responses",
            c("Codex、工具调用、多轮响应", "Codex, tools and response chains"),
            "input · reasoning.effort · text.format",
          ],
          [
            "Chat Completions",
            c("已有 OpenAI 聊天客户端", "Existing OpenAI chat clients"),
            "messages · reasoning_effort · response_format",
          ],
          [
            "Messages",
            c(
              "Claude Code / Anthropic 客户端",
              "Claude Code / Anthropic clients",
            ),
            "messages · max_tokens · tools",
          ],
          [
            "Images / Videos",
            c(
              "图片生成、编辑及异步视频",
              "Image generation, editing and async video",
            ),
            "prompt · model · size / duration",
          ],
        ],
      },
      paragraphs: [
        c(
          "模型以当前 /v1/models 返回及密钥授权为准；返回在列表中不代表所有账号都具有该模型能力。渠道限制、分组授权、模型映射和账号可用状态都会影响调度。",
          "Use the current /v1/models catalog and your key's permissions. A listed model is not guaranteed to be available on every account. Channel restrictions, groups, mappings and account readiness affect scheduling.",
        ),
        c(
          "Codex 路径会转换或清洗部分参数。reasoning.effort 与 Chat 的 reasoning_effort 按最终模型归一；max 仅对支持的较新模型放行，旧模型降为 xhigh。fast 映射为 priority，ultrafast 保留；auto/default 等不强制指定上游档位。具体速度和额度以账号及实际响应为准。",
          "The Codex path transforms or sanitizes some parameters. reasoning.effort and Chat's reasoning_effort are normalized for the final model; max is retained for supported newer models and becomes xhigh on older ones. fast maps to priority, ultrafast is retained, and auto/default do not force a tier. Actual latency and limits depend on the account and response.",
        ),
        c(
          "Claude 原生 Messages 与 Codex 转换路径的能力不同。temperature、输出长度、内置工具和 JSON Schema 约束不要按所有渠道完全一致来使用；先用目标模型验证最小请求，再逐项增加参数。",
          "Native Claude Messages and the Codex translation path have different capabilities. Sampling, output limits, built-in tools and JSON Schema constraints are not identical across channels. Validate a minimal request with your target model before adding parameters.",
        ),
      ],
      links: [
        {
          label: c("模型定价与用量", "Model pricing & usage"),
          href: "/admin/model-pricing",
        },
        { label: c("模型映射说明", "Model mapping"), href: "#client-mapping" },
      ],
    },
    {
      id: "sdk-examples",
      section: "development",
      title: c("第一个 SDK 请求", "Your first SDK request"),
      summary: c(
        "在服务端设置 CODEX2API_KEY，使用当前实例的 /v1 地址。",
        "Set CODEX2API_KEY on your server and use this instance's /v1 URL.",
      ),
      paragraphs: [
        c(
          "Python 安装：pip install openai；Node.js 安装：npm install openai，将示例保存为 example.mjs。示例使用当前网关模型名，按可用模型替换。",
          "Python: pip install openai. Node.js: npm install openai, then save the example as example.mjs. Replace the gateway model name with one available to your key.",
        ),
      ],
      examples: [
        {
          label: "Python · Responses",
          lang: "python",
          content: `import os\nfrom openai import OpenAI\n\nclient = OpenAI(\n    api_key=os.environ["CODEX2API_KEY"],\n    base_url=${JSON.stringify(`${base}/v1`)},\n)\nresponse = client.responses.create(\n    model="gpt-5.5", input="Say hello in one sentence.", stream=False,\n)\nprint(response.output_text)`,
        },
        {
          label: "Node.js · Responses",
          lang: "javascript",
          content: `import OpenAI from "openai";\n\nconst client = new OpenAI({\n  apiKey: process.env.CODEX2API_KEY,\n  baseURL: ${JSON.stringify(`${base}/v1`)},\n});\nconst response = await client.responses.create({\n  model: "gpt-5.5", input: "Say hello in one sentence.", stream: false,\n});\nconsole.log(response.output_text);`,
        },
      ],
      links: [
        {
          label: c("OpenAI SDK 官方说明", "Official OpenAI SDK documentation"),
          href: "https://developers.openai.com/api/docs/libraries",
        },
      ],
    },
    {
      id: "streaming",
      section: "development",
      title: c("流式响应与结束状态", "Streaming & terminal states"),
      summary: c(
        "stream=false 返回 JSON；stream=true 返回 SSE，需要按事件读取。",
        "stream=false returns JSON; stream=true returns SSE events.",
      ),
      paragraphs: [
        c(
          "cURL 使用 -N 关闭输出缓冲。Responses 读取 response.output_text.delta，并处理 response.completed、response.incomplete、response.failed 或 error。连接中断不能当成成功完成。Chat 使用 choices[].delta 和 [DONE]；Messages 使用 content_block_delta、message_stop。",
          "Use curl -N to disable output buffering. For Responses, read response.output_text.delta and handle response.completed, response.incomplete, response.failed or error. A disconnected stream is not a successful completion. Chat uses choices[].delta and [DONE]; Messages uses content_block_delta and message_stop.",
        ),
      ],
      examples: [
        {
          label: "Node.js · SSE",
          lang: "javascript",
          content: `// Reuse the client from the SDK example.\nconst stream = await client.responses.create({\n  model: "gpt-5.5", input: "Say hello.", stream: true,\n});\nlet completed = false;\nfor await (const event of stream) {\n  if (event.type === "response.output_text.delta") process.stdout.write(event.delta);\n  if (event.type === "response.completed") completed = true;\n  if (["error", "response.failed", "response.incomplete"].includes(event.type)) {\n    throw new Error(JSON.stringify(event));\n  }\n}\nif (!completed) throw new Error("Stream ended before completion");`,
        },
      ],
      links: [
        {
          label: c("流式事件参考", "Streaming event reference"),
          href: "https://developers.openai.com/api/docs/guides/streaming-responses",
        },
      ],
    },
    {
      id: "conversation-tools",
      section: "development",
      title: c("多轮对话与工具调用", "Conversation history & tools"),
      summary: c(
        "保留完整历史与 call_id，才能让模型接着工具结果继续回答。",
        "Preserve history and call_id so the model can continue after a tool result.",
      ),
      paragraphs: [
        c(
          "最稳妥的方式是保存原始 input、全部 response.output 和工具结果，在下一轮一起提交。不要只保存 output_text，推理项和工具项也可能是下一轮所需的上下文。下面示例只执行允许的工具，并保留调用配对。",
          "Store the original input, all response.output items and tool results, then send them together on the next turn. Saving only output_text can lose reasoning and tool context. The example executes only the allowed tool and preserves call pairing.",
        ),
        c(
          "previous_response_id 依赖当前密钥作用域内可恢复的历史。缓存受 TTL、容量及写入策略约束，Memory 缓存配合 SQLite 文件主库时默认启用磁盘持久层，关闭后重启会丢失；store:false 的部分会话不会写入回放缓存。收到 409 previous_response_not_found 时重发完整历史并开始新链，不能只反复提交同一个失效 ID。",
          "previous_response_id requires recoverable history in the same key scope. TTL, capacity and write policy apply, and Memory caching with a file-backed SQLite database enables a local on-disk store by default (without it, history is lost on restart); some store:false sessions do not populate replay history. On 409 previous_response_not_found, send full history and start a new chain instead of retrying the same unavailable ID.",
        ),
      ],
      examples: [
        {
          label: "Python · function_call_output",
          lang: "python",
          content: `import json\n\nhistory = [{"role": "user", "content": "What is 2 plus 3?"}]\ntools = [{\n    "type": "function", "name": "add", "description": "Add two numbers",\n    "parameters": {"type": "object", "properties": {\n        "a": {"type": "number"}, "b": {"type": "number"}\n    }, "required": ["a", "b"], "additionalProperties": False},\n    "strict": True,\n}]\nfor _ in range(5):\n    reply = client.responses.create(model="gpt-5.5", input=history, tools=tools)\n    history.extend(reply.output)\n    calls = [item for item in reply.output if item.type == "function_call"]\n    if not calls:\n        print(reply.output_text)\n        break\n    for call in calls:\n        if call.name != "add":\n            raise ValueError("Unknown tool")\n        args = json.loads(call.arguments)\n        history.append({"type": "function_call_output", "call_id": call.call_id,\n                        "output": str(args["a"] + args["b"])})\nelse:\n    raise RuntimeError("Tool round limit reached")`,
        },
      ],
    },
    {
      id: "structured-output",
      section: "development",
      title: c("结构化输出", "Structured output"),
      summary: c(
        "Responses 的 schema 放在 text.format；Chat 使用 response_format。",
        "Responses uses text.format; Chat uses response_format.",
      ),
      paragraphs: [
        c(
          "先从简单对象开始，明确 required 与 additionalProperties。网关会依据最终模型、Lite 能力和 HTTP/WS 传输处理部分约束；不要把工具参数 schema 与输出 schema 的支持范围混为一谈。",
          "Start with a simple object and explicit required/additionalProperties. Some constraints depend on the final model, Lite capability and HTTP/WS transport. Tool parameter schemas and output schemas do not have identical support.",
        ),
      ],
      examples: [
        {
          label: "Responses · text.format",
          lang: "json",
          content: JSON.stringify(
            {
              model: "gpt-5.5",
              input: "Return a short greeting.",
              text: {
                format: {
                  type: "json_schema",
                  name: "greeting",
                  strict: true,
                  schema: {
                    type: "object",
                    properties: { answer: { type: "string" } },
                    required: ["answer"],
                    additionalProperties: false,
                  },
                },
              },
            },
            null,
            2,
          ),
        },
      ],
    },
    {
      id: "media-guide",
      section: "model-api",
      title: c("图片与视频接入说明", "Image & video guide"),
      summary: c(
        "同步图片、持久化图片任务和视频任务分别使用不同的返回结构。",
        "Synchronous images, persistent image jobs and video jobs have different response shapes.",
      ),
      table: {
        headers: [c("能力", "Capability"), c("如何使用", "How to use")],
        rows: [
          [
            "GPT Image 2.5",
            c(
              "Flare / Sunburst 支持 auto、low、medium、high、xhigh、max；省略模型仍默认 gpt-image-2。",
              "Flare / Sunburst accept auto, low, medium, high, xhigh and max; an omitted model still defaults to gpt-image-2.",
            ),
          ],
          [
            c("尺寸与格式", "Size & format"),
            c(
              "size 为宽x高；-2k/-4k 是本项目尺寸档位别名。output_format 决定编码格式，response_format 决定 b64_json 或 url 返回方式。",
              "size is WIDTHxHEIGHT; -2k/-4k are gateway size aliases. output_format selects encoding; response_format selects b64_json or url.",
            ),
          ],
          [
            c("编辑与遮罩", "Edits & masks"),
            c(
              "JSON 使用 images[].image_url；multipart 使用 image / image[]。mask 仅用于 gpt-image 系列。",
              "JSON uses images[].image_url; multipart uses image/image[]. Masks are supported only for the gpt-image family.",
            ),
          ],
          [
            c("图片任务", "Image jobs"),
            c(
              "POST /v1/images/jobs 返回 202 和 job；使用同一 API Key 查询 job.id，按 job.status 判断是否完成。",
              "POST /v1/images/jobs returns 202 and job. Query job.id with the same API key and inspect job.status.",
            ),
          ],
          [
            c("视频任务", "Video jobs"),
            c(
              "创建后拿 request_id，每隔 2–5 秒查询，done 后下载 /content。需要支持视频的付费 Grok 账号及允许该渠道的密钥。",
              "Poll the returned request_id every 2–5 seconds and download /content after done. Requires a paid Grok account with video access and a key allowing that channel.",
            ),
          ],
        ],
      },
      paragraphs: [
        c(
          "直接调用 Responses 生图时，顶层 model 是文本驱动，tools[].model 是图片模型。通过 Images 入口调用时，文本驱动由后台生图设置选择。费用和 token 明细可在模型定价、使用统计中查看。",
          "For image generation through Responses, top-level model is the text driver and tools[].model is the image model. The Images endpoint selects its driver from dashboard settings. See model pricing and usage for cost and token details.",
        ),
      ],
      links: [
        {
          label: c("打开生图工作台", "Open Image Studio"),
          href: "/admin/images/studio",
        },
        { label: c("使用统计", "Usage"), href: "/admin/usage" },
      ],
    },
    {
      id: "errors",
      section: "troubleshooting",
      title: c("按错误定位问题", "Find the cause by error"),
      summary: c(
        "先看 HTTP 状态与 error.code，再决定修正配置还是等待重试。",
        "Check the HTTP status and error.code before changing configuration or retrying.",
      ),
      table: {
        headers: [c("现象", "Symptom"), c("检查与处理", "What to check")],
        rows: [
          [
            "401",
            c(
              "核对使用的是下游 API Key，检查请求头、密钥状态和有效期；管理接口改用 Admin Secret。",
              "Check the downstream API key, header, key status and expiration. Use the Admin Secret for admin endpoints.",
            ),
          ],
          [
            "403",
            c(
              "检查密钥的渠道、模型和分组授权，以及上游账号的实际能力。",
              "Check key channel/model/group permissions and upstream account capability.",
            ),
          ],
          [
            "429 · rate_limit_reached",
            c(
              "可能命中模型周次数预算；检查 error.details 的 used、limit、reset_at 和 Retry-After。",
              "May indicate a weekly model budget. Inspect used, limit and reset_at in error.details, plus Retry-After.",
            ),
          ],
          [
            "503 · no_available_account",
            c(
              "查看账号启用状态、分组范围、冷却、并发和可用模型。",
              "Check enabled accounts, groups, cooldown, concurrency and available models.",
            ),
          ],
          [
            "503 · account_pool_concurrency_saturated",
            c(
              "匹配账号都在，但并发窗口已满。稍后重试，或提高单账号并发上限。",
              "Matching accounts exist, but their concurrency windows are full. Retry shortly or raise the per-account concurrency limit.",
            ),
          ],
          [
            "503 · account_pool_usage_limit_reached",
            c(
              "上游账号池额度耗尽；参考 Retry-After，等待重置或补充可用账号。",
              "Upstream account quota is exhausted. Respect Retry-After, wait for reset or add eligible accounts.",
            ),
          ],
          [
            "409 · previous_response_not_found",
            c(
              "上一轮上下文缺失；携带完整必需历史重新发起，开始新响应链。",
              "Previous context is unavailable. Resend the full required history and start a new chain.",
            ),
          ],
          [
            "503 · service_unavailable",
            c(
              "可能是上下文共享后端或预算存储暂时不可用；查看系统运维，退避后重试。",
              "Context or quota storage may be temporarily unavailable. Check operations and retry with backoff.",
            ),
          ],
          [
            c("流提前结束 / 一直等待", "Stream ends early / stalls"),
            c(
              "检查终止事件、客户端超时、反向代理缓冲和连接日志。已收到部分输出时，重试可能再次产生输出及用量。",
              "Check terminal events, client timeouts, reverse-proxy buffering and connection logs. Retrying after partial output can produce output and usage again.",
            ),
          ],
        ],
      },
      links: [
        { label: c("查看请求日志", "Request logs"), href: "/admin/usage" },
        { label: c("系统运维", "Operations"), href: "/admin/ops/overview" },
      ],
    },
    {
      id: "connection-check",
      section: "troubleshooting",
      title: c(
        "从连通到成功响应",
        "From connectivity to a successful response",
      ),
      summary: c(
        "健康检查、模型列表、生成请求分别验证不同环节。",
        "Health, model listing and generation validate different parts of the chain.",
      ),
      paragraphs: [
        c(
          "1. GET /health 成功只说明服务在线。2. 用客户端密钥请求 /v1/models，验证地址和认证。3. 选择有可用账号支持的模型，发送最小生成请求。4. 在使用统计查看最终模型、状态、耗时和 token。",
          "1. GET /health only verifies service health. 2. Request /v1/models with the client key to check URL and authentication. 3. Send a minimal generation request to a model with eligible accounts. 4. Inspect the final model, status, duration and tokens in Usage.",
        ),
        c(
          "出现 404 时先核对地址是否重复拼接 /v1、路径是否完整，以及服务器版本是否支持该端点。视频查询还需检查是否使用创建任务时的同一 API Key、任务绑定是否过期。",
          "For 404, check duplicated /v1, the complete path and whether the server version supports the endpoint. Video queries also require the creating API key and an unexpired task binding.",
        ),
      ],
      examples: [
        {
          label: "cURL · models",
          lang: "bash",
          content: `curl "${base}/v1/models" \\\n  -H "Authorization: Bearer YOUR_API_KEY"`,
        },
      ],
    },
    {
      id: "key-limits",
      section: "admin-api",
      title: c("密钥、额度与分组", "Keys, budgets & groups"),
      summary: c(
        "金额额度、周请求次数与上游账号额度是不同的限制。",
        "Monetary budgets, weekly request counts and upstream quotas are separate limits.",
      ),
      paragraphs: [
        c(
          "金额额度控制密钥累计费用；模型周次数按最终映射后的模型匹配规则，同一规则内的模型共享次数。周次数的时区、重置星期和时间独立配置，不等同于上游套餐额度。",
          "Monetary budgets limit a key's cumulative cost. Weekly rules match the final mapped model, and all models matching one rule share its count. Timezone, weekday and reset time are configured separately from upstream plan quotas.",
        ),
        c(
          "账号可以加入多个分组，API Key 可限制允许使用的分组和渠道。排查“有账号但无法调用”时，同时检查密钥范围与账号分组。模型周预算耗尽会返回 429，并提供本次规则的重置时间。",
          "Accounts can belong to multiple groups, while API keys can restrict groups and channels. If accounts exist but requests fail, check both key scope and account membership. Exhausted weekly model budgets return 429 with the rule's reset time.",
        ),
      ],
      examples: [
        {
          label: "API Key · limits",
          lang: "json",
          content: JSON.stringify(
            {
              limits: {
                model_request_limits: [
                  {
                    model: "gpt-6*",
                    window: "week",
                    max_requests: 50,
                    timezone: "Asia/Shanghai",
                    reset_weekday: 1,
                    reset_time: "00:00",
                  },
                ],
              },
            },
            null,
            2,
          ),
        },
      ],
      links: [
        {
          label: c("设置密钥限制", "Configure key limits"),
          href: "/admin/api-keys",
        },
      ],
    },
    {
      id: "claude-credentials",
      section: "admin-api",
      title: c("Claude 凭据类型", "Claude credential types"),
      summary: c(
        "选择凭据方式后，再配置原生 Messages 的模型和账号范围。",
        "Choose credentials, then configure native Messages models and account scope.",
      ),
      table: {
        headers: [c("类型", "Type"), c("行为", "Behavior")],
        rows: [
          [
            "OAuth",
            c(
              "Access Token + Refresh Token，可自动刷新并读取支持的订阅用量。",
              "Access Token + Refresh Token; supports refresh and subscription usage where available.",
            ),
          ],
          [
            "Setup Token",
            c(
              "长效令牌，无 Refresh Token；到期需重新授权，用量能力与 OAuth 不同。",
              "Long-lived token without refresh; reauthorize on expiry. Usage capabilities differ from OAuth.",
            ),
          ],
          [
            "API Key + Base URL",
            c(
              "连接 Anthropic 或 Messages 兼容服务；不走 OAuth 刷新或订阅用量采样。Base URL 可以带路径前缀或 /v1。",
              "Connect to Anthropic or a Messages-compatible service without OAuth refresh or subscription usage sampling. Base URL can include a path prefix or /v1.",
            ),
          ],
        ],
      },
      paragraphs: [
        c(
          "原生 Claude 路径与 Codex 模型映射回退取决于账号、密钥渠道和路由设置；指定 Claude 渠道并不保证能回退到 Codex。API Key 的 Base URL 是上游服务地址，出站 proxy_url 仍是单独设置。",
          "Native Claude routing and Codex mapping fallback depend on accounts, key channel and routing settings. A Claude-scoped key does not guarantee Codex fallback. An API key's Base URL is its upstream service address; proxy_url is configured separately.",
        ),
      ],
      links: [
        { label: c("管理账号", "Manage accounts"), href: "/admin/accounts" },
      ],
    },
    {
      id: "client-mapping",
      section: "admin-api",
      title: c("模型映射与运维入口", "Model mapping & operations"),
      summary: c(
        "客户端请求的模型名与最终上游模型可能不同。",
        "The requested model can differ from the final upstream model.",
      ),
      paragraphs: [
        c(
          "Claude 映射用于 Messages 转换到 Codex 时选择模型；Codex 模型重定向支持 gpt-*、*-mini、*codex* 等通配规则。在系统设置核对映射，在请求日志核对最终模型；不要仅凭客户端显示的名字判断实际调用。",
          "Claude mappings select the Codex model during Messages translation. Codex redirects support wildcards such as gpt-*, *-mini and *codex*. Check mappings in Settings and the final model in request logs rather than relying only on the client's displayed name.",
        ),
        c(
          "账号刷新、批量测试、代理池、统计、设置和生图资产等完整管理接口见仓库 API 文档。下面保留最常用的账号、密钥和分组接口，按需展开。",
          "The repository API reference covers account refresh, batch testing, proxy pools, usage, settings and image assets. The common account, key and group endpoints below expand on demand.",
        ),
      ],
      links: [
        { label: c("系统设置", "Settings"), href: "/admin/settings" },
        {
          label: c("完整 API 文档", "Full API reference"),
          href: "https://github.com/james-6-23/codex2api/blob/main/docs/API.md",
        },
        {
          label: c("部署与故障排查", "Deployment & troubleshooting"),
          href: "https://github.com/james-6-23/codex2api/blob/main/docs/TROUBLESHOOTING.md",
        },
      ],
    },
  ];
}

export function guideToMarkdown(guide: GuideSpec): string {
  const table = guide.table
    ? [
        guide.table.headers,
        guide.table.headers.map(() => "---"),
        ...guide.table.rows,
      ]
        .map(
          (row) =>
            `| ${row.map((cell) => cell.replace(/\|/g, "\\|")).join(" | ")} |`,
        )
        .join("\n")
    : "";
  return [
    `<a id="${guide.id}"></a>`,
    `### ${guide.title}`,
    guide.summary,
    ...(guide.paragraphs ?? []),
    table,
    ...(guide.examples ?? []).map(
      (e) => `#### ${e.label}\n\n\`\`\`${e.lang}\n${e.content}\n\`\`\``,
    ),
    ...(guide.links ?? []).map((link) => `[${link.label}](${link.href})`),
  ]
    .filter(Boolean)
    .join("\n\n");
}
