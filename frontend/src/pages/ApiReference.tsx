import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Copy, Check } from 'lucide-react'
import PageHeader from '../components/PageHeader'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'

// 带复制按钮的代码块
function CodeBlock({ label, content, lang }: { label?: string; content: string; lang?: string }) {
  const [copied, setCopied] = useState(false)
  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(content)
    } catch {
      const ta = document.createElement('textarea')
      ta.value = content
      ta.style.cssText = 'position:fixed;left:-9999px'
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="bg-zinc-900 dark:bg-zinc-950 rounded-xl overflow-hidden">
      {label && (
        <div className="flex items-center justify-between px-4 py-2 bg-zinc-800 dark:bg-zinc-900 border-b border-zinc-700">
          <span className="text-xs text-zinc-400 font-mono">{label}</span>
          <button
            onClick={() => void handleCopy()}
            className={`flex items-center gap-1.5 px-2.5 py-1 text-xs font-medium rounded-lg transition-colors ${
              copied ? 'bg-emerald-500/20 text-emerald-400' : 'bg-zinc-700 hover:bg-zinc-600 text-zinc-300 hover:text-white'
            }`}
          >
            {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
      )}
      {!label && (
        <div className="absolute top-2 right-2 z-10">
          <button
            onClick={() => void handleCopy()}
            className={`flex items-center gap-1 px-2 py-1 text-xs rounded-md transition-colors ${
              copied ? 'bg-emerald-500/20 text-emerald-400' : 'bg-zinc-700/80 hover:bg-zinc-600 text-zinc-400 hover:text-white'
            }`}
          >
            {copied ? <Check className="size-3" /> : <Copy className="size-3" />}
          </button>
        </div>
      )}
      <pre className={`p-4 font-mono text-zinc-100 overflow-x-auto leading-relaxed ${lang === 'json' ? 'text-[13px]' : 'text-sm'}`}>
        <code>{content}</code>
      </pre>
    </div>
  )
}

// 状态码 Tab 切换
function StatusTabs({ tabs, active, onChange }: { tabs: { code: number; label?: string }[]; active: number; onChange: (c: number) => void }) {
  return (
    <div className="flex items-center gap-1 border-b border-border mb-0">
      {tabs.map(tab => (
        <button
          key={tab.code}
          onClick={() => onChange(tab.code)}
          className={`px-3 py-2 text-sm font-medium border-b-2 transition-colors ${
            active === tab.code
              ? 'border-foreground text-foreground'
              : 'border-transparent text-muted-foreground hover:text-foreground'
          }`}
        >
          {tab.code}
        </button>
      ))}
    </div>
  )
}

// 方法颜色
function MethodBadge({ method }: { method: string }) {
  const colors: Record<string, string> = {
    GET: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400 border-emerald-200 dark:border-emerald-800',
    POST: 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400 border-blue-200 dark:border-blue-800',
    PUT: 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400 border-amber-200 dark:border-amber-800',
    DELETE: 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400 border-red-200 dark:border-red-800',
  }
  return (
    <span className={`inline-flex items-center px-2.5 py-1 rounded-lg text-xs font-bold border ${colors[method] || 'bg-muted text-foreground border-border'}`}>
      {method}
    </span>
  )
}

// 单个端点文档
function EndpointDoc({ method, path, title, description, curlExample, responseExamples }: {
  method: string
  path: string
  title: string
  description: string
  curlExample: string
  responseExamples: { code: number; body: string }[]
}) {
  const [activeStatus, setActiveStatus] = useState(responseExamples[0]?.code ?? 200)
  const activeBody = responseExamples.find(r => r.code === activeStatus)?.body ?? ''

  return (
    <Card className="mb-6">
      <CardContent className="p-6">
        {/* 标题 */}
        <h3 className="text-xl font-bold text-foreground mb-1">{title}</h3>
        <p className="text-sm text-muted-foreground mb-4">{description}</p>

        {/* 端点路径栏 */}
        <div className="flex items-center gap-3 p-3 rounded-xl border border-border bg-muted/30 mb-5">
          <MethodBadge method={method} />
          <code className="font-mono text-sm font-semibold text-foreground">{path}</code>
        </div>

        {/* cURL 示例 */}
        <div className="mb-5">
          <CodeBlock label="cURL" content={curlExample} />
        </div>

        {/* 响应示例 */}
        <div className="border border-border rounded-xl overflow-hidden">
          <div className="px-4 pt-2 bg-muted/30">
            <StatusTabs
              tabs={responseExamples.map(r => ({ code: r.code }))}
              active={activeStatus}
              onChange={setActiveStatus}
            />
          </div>
          <pre className="p-4 font-mono text-[13px] text-foreground overflow-x-auto leading-relaxed bg-muted/10 max-h-[400px]">
            <code>{activeBody}</code>
          </pre>
        </div>
      </CardContent>
    </Card>
  )
}

export default function ApiReference() {
  const { t } = useTranslation()

  return (
    <>
      <PageHeader
        title={t('apiRef.title')}
        description={t('apiRef.description')}
      />

      {/* 认证说明 */}
      <Card className="mb-6">
        <CardContent className="p-6">
          <h3 className="text-base font-semibold text-foreground mb-2">{t('apiRef.authSection')}</h3>
          <p className="text-sm text-muted-foreground mb-3">{t('apiRef.authDesc')}</p>
          <div className="space-y-1.5 text-sm font-mono">
            <div className="flex items-center gap-2">
              <Badge variant="outline" className="text-[11px]">Header</Badge>
              <code className="text-[13px]">Authorization: Bearer &lt;key&gt;</code>
            </div>
            <div className="flex items-center gap-2">
              <Badge variant="outline" className="text-[11px]">Header</Badge>
              <code className="text-[13px]">x-api-key: &lt;key&gt;</code>
            </div>
          </div>
        </CardContent>
      </Card>

      {/* POST /v1/responses */}
      <EndpointDoc
        method="POST"
        path="/v1/responses"
        title={t('apiRef.responses.title')}
        description={t('apiRef.responses.desc')}
        curlExample={`curl --request POST \\
  --url http://your-server:8080/v1/responses \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.4",
  "input": [
    {
      "role": "user",
      "content": [
        {"type": "input_text", "text": "Hello, what can you do?"}
      ]
    }
  ],
  "stream": true,
  "reasoning": {"effort": "high"}
}'`}
        responseExamples={[
          { code: 200, body: `{
  "id": "resp_abc123",
  "object": "response",
  "model": "gpt-5.4",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {
          "type": "output_text",
          "text": "Hello! I'm an AI assistant..."
        }
      ]
    }
  ],
  "usage": {
    "input_tokens": 12,
    "output_tokens": 45,
    "total_tokens": 57
  }
}` },
          { code: 400, body: `{
  "error": {
    "code": "invalid_request",
    "message": "model is required",
    "type": "invalid_request_error"
  }
}` },
          { code: 401, body: `{
  "error": {
    "code": "invalid_api_key",
    "message": "Invalid API key provided",
    "type": "authentication_error"
  }
}` },
          { code: 429, body: `{
  "error": {
    "message": "Rate limit exceeded",
    "type": "server_error",
    "code": "account_pool_usage_limit_reached",
    "resets_in_seconds": 18000
  }
}` },
        ]}
      />

      {/* POST /v1/chat/completions */}
      <EndpointDoc
        method="POST"
        path="/v1/chat/completions"
        title={t('apiRef.chat.title')}
        description={t('apiRef.chat.desc')}
        curlExample={`curl --request POST \\
  --url http://your-server:8080/v1/chat/completions \\
  --header 'Authorization: Bearer <token>' \\
  --header 'Content-Type: application/json' \\
  --data '{
  "model": "gpt-5.4",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": true,
  "reasoning_effort": "high"
}'`}
        responseExamples={[
          { code: 200, body: `{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "model": "gpt-5.4",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 18,
    "completion_tokens": 9,
    "total_tokens": 27
  }
}` },
          { code: 400, body: `{
  "error": {
    "code": "invalid_request",
    "message": "Request validation failed",
    "type": "invalid_request_error"
  }
}` },
          { code: 401, body: `{
  "error": {
    "code": "invalid_api_key",
    "message": "Invalid API key provided",
    "type": "authentication_error"
  }
}` },
        ]}
      />

      {/* POST /v1/messages */}
      <EndpointDoc
        method="POST"
        path="/v1/messages"
        title={t('apiRef.messages.title')}
        description={t('apiRef.messages.desc')}
        curlExample={`curl --request POST \\
  --url http://your-server:8080/v1/messages \\
  --header 'x-api-key: <token>' \\
  --header 'Content-Type: application/json' \\
  --header 'anthropic-version: 2023-06-01' \\
  --data '{
  "model": "claude-sonnet-4-5-20250514",
  "max_tokens": 1024,
  "messages": [
    {"role": "user", "content": "Hello, Claude!"}
  ]
}'`}
        responseExamples={[
          { code: 200, body: `{
  "id": "msg_abc123",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-5-20250514",
  "content": [
    {
      "type": "text",
      "text": "Hello! How can I assist you today?"
    }
  ],
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 10,
    "output_tokens": 12,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  }
}` },
          { code: 400, body: `{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "model is required"
  }
}` },
          { code: 401, body: `{
  "type": "error",
  "error": {
    "type": "authentication_error",
    "message": "Invalid API key"
  }
}` },
          { code: 429, body: `{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "All accounts rate limited"
  }
}` },
        ]}
      />

      {/* GET /v1/models */}
      <EndpointDoc
        method="GET"
        path="/v1/models"
        title={t('apiRef.models.title')}
        description={t('apiRef.models.desc')}
        curlExample={`curl --request GET \\
  --url http://your-server:8080/v1/models \\
  --header 'Authorization: Bearer <token>'`}
        responseExamples={[
          { code: 200, body: `{
  "object": "list",
  "data": [
    {"id": "gpt-5.4", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.4-mini", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.3-codex", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.2-codex", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.1-codex", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.1-codex-mini", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.1-codex-max", "object": "model", "owned_by": "openai"}
  ]
}` },
          { code: 401, body: `{
  "error": {
    "code": "invalid_api_key",
    "message": "Invalid API key provided",
    "type": "authentication_error"
  }
}` },
        ]}
      />

      {/* GET /health */}
      <EndpointDoc
        method="GET"
        path="/health"
        title={t('apiRef.health.title')}
        description={t('apiRef.health.desc')}
        curlExample={`curl --request GET \\
  --url http://your-server:8080/health`}
        responseExamples={[
          { code: 200, body: `{
  "status": "ok",
  "available": 5,
  "total": 8
}` },
        ]}
      />
    </>
  )
}
