import { useEffect, useMemo, useState } from 'react'
import { Check, Copy, ExternalLink } from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Select } from '@/components/ui/select'

type TabOption<T extends string> = {
  id: T
  label: string
}

type ApiKeyOption = {
  name: string
  key: string
}

type ClientTool = {
  id: string
  name: string
  badge: string
  blurb: string
  glyph: string
  tone: string
}

const CLIENT_TOOLS: ClientTool[] = [
  {
    id: 'codex',
    name: 'Codex CLI',
    badge: 'Responses',
    blurb: 'Write config.toml and auth.json for the OpenAI Responses wire API.',
    glyph: 'CX',
    tone: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  },
  {
    id: 'claude',
    name: 'Claude Code',
    badge: 'Anthropic',
    blurb: 'Use environment variables or settings.json for the Messages endpoint.',
    glyph: 'CC',
    tone: 'bg-orange-500/10 text-orange-600 dark:text-orange-400',
  },
  {
    id: 'cc-switch',
    name: 'CC Switch',
    badge: 'Deeplink',
    blurb: 'Launch the desktop switcher and import this server as a provider.',
    glyph: 'CS',
    tone: 'bg-fuchsia-500/10 text-fuchsia-600 dark:text-fuchsia-400',
  },
]

const FALLBACK_MODELS = ['gpt-5.5', 'gpt-5.4-mini', 'gpt-5.3-codex', 'claude-sonnet-4-5-20250514']

function encodeBase64(text: string): string {
  return btoa(unescape(encodeURIComponent(text)))
}

function buildCcSwitchUrl(baseUrl: string, apiKey: string): string {
  const config = encodeURIComponent(encodeBase64(JSON.stringify({
    name: 'codex2api',
    baseURL: baseUrl,
    apiKey,
    anthropicVersion: '2023-06-01',
  })))
  return `cc-switch://import?data=${config}`
}

function keyLabel(item: ApiKeyOption): string {
  if (!item.key) return item.name || 'API Key'
  const masked = item.key.length > 14 ? `${item.key.slice(0, 7)}...${item.key.slice(-4)}` : item.key
  return item.name ? `${item.name} - ${masked}` : masked
}

function Tabs<T extends string>({ tabs, active, onChange }: {
  tabs: TabOption<T>[]
  active: T
  onChange: (value: T) => void
}) {
  return (
    <div className="mb-4 flex flex-wrap gap-1.5 border-b border-border">
      {tabs.map((tab) => (
        <button
          key={tab.id}
          type="button"
          onClick={() => onChange(tab.id)}
          className={`-mb-px border-b-2 px-3 py-2 text-sm font-semibold transition-colors ${
            active === tab.id
              ? 'border-primary text-primary'
              : 'border-transparent text-muted-foreground hover:text-foreground'
          }`}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

function CodeBlock({ label, content }: { label: string; content: string }) {
  const [copied, setCopied] = useState(false)

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(content)
    } catch {
      const textarea = document.createElement('textarea')
      textarea.value = content
      textarea.style.cssText = 'position:fixed;left:-9999px'
      document.body.appendChild(textarea)
      textarea.select()
      document.execCommand('copy')
      document.body.removeChild(textarea)
    }
    setCopied(true)
    setTimeout(() => setCopied(false), 1600)
  }

  return (
    <div className="overflow-hidden rounded-lg bg-zinc-900 dark:bg-zinc-950">
      <div className="flex items-center justify-between gap-3 border-b border-zinc-700 bg-zinc-800 px-4 py-2 dark:bg-zinc-900">
        <span className="truncate font-mono text-xs text-zinc-400">{label}</span>
        <button
          type="button"
          onClick={() => void handleCopy()}
          className={`inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-semibold transition-colors ${
            copied ? 'bg-emerald-500/20 text-emerald-300' : 'bg-zinc-700 text-zinc-300 hover:bg-zinc-600 hover:text-white'
          }`}
        >
          {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <pre className="overflow-x-auto p-4 font-mono text-[13px] leading-relaxed text-zinc-100">
        <code>{content}</code>
      </pre>
    </div>
  )
}

function ClientCard({ tool, activeKey, baseUrl }: {
  tool: ClientTool
  activeKey: string
  baseUrl: string
}) {
  const [codexTab, setCodexTab] = useState<'unix' | 'windows'>('unix')
  const [claudeTab, setClaudeTab] = useState<'env-unix' | 'env-windows' | 'settings'>('env-unix')
  const key = activeKey || 'YOUR_API_KEY'
  const codexDir = codexTab === 'windows' ? '%userprofile%\\.codex' : '~/.codex'

  const codexConfig = `model_provider = "OpenAI"
model = "gpt-5.5"
review_model = "gpt-5.5"
model_reasoning_effort = "xhigh"
disable_response_storage = true
network_access = "enabled"

[model_providers.OpenAI]
name = "OpenAI"
base_url = "${baseUrl}"
wire_api = "responses"
requires_openai_auth = true

[features]
goals = true`

  const codexAuth = `{
  "OPENAI_API_KEY": "${key}"
}`

  const claudeEnvUnix = `export ANTHROPIC_BASE_URL="${baseUrl}"
export ANTHROPIC_AUTH_TOKEN="${key}"
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`

  const claudeEnvWindows = `set ANTHROPIC_BASE_URL=${baseUrl}
set ANTHROPIC_AUTH_TOKEN=${key}
set CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`

  const claudeSettings = `{
  "env": {
    "ANTHROPIC_BASE_URL": "${baseUrl}",
    "ANTHROPIC_AUTH_TOKEN": "${key}",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}`

  const ccSwitchUrl = buildCcSwitchUrl(baseUrl, key)

  return (
    <Card id={`client-${tool.id}`} className="scroll-mt-20">
      <CardContent className="p-5">
        <div className="mb-4 flex items-start gap-3">
          <div className={`flex size-10 shrink-0 items-center justify-center rounded-lg text-sm font-bold ${tool.tone}`}>
            {tool.glyph}
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-2">
              <h3 className="text-base font-semibold text-foreground">{tool.name}</h3>
              <Badge variant="outline" className="text-[10px] font-bold">{tool.badge}</Badge>
            </div>
            <p className="mt-1 text-sm text-muted-foreground">{tool.blurb}</p>
          </div>
        </div>

        {tool.id === 'codex' && (
          <>
            <Tabs
              tabs={[{ id: 'unix', label: 'macOS / Linux' }, { id: 'windows', label: 'Windows' }]}
              active={codexTab}
              onChange={setCodexTab}
            />
            <div className="space-y-4">
              <CodeBlock label={`${codexDir}/config.toml`} content={codexConfig} />
              <CodeBlock label={`${codexDir}/auth.json`} content={codexAuth} />
            </div>
          </>
        )}

        {tool.id === 'claude' && (
          <>
            <Tabs
              tabs={[
                { id: 'env-unix', label: 'macOS / Linux env' },
                { id: 'env-windows', label: 'Windows env' },
                { id: 'settings', label: 'settings.json' },
              ]}
              active={claudeTab}
              onChange={setClaudeTab}
            />
            {claudeTab === 'env-unix' && <CodeBlock label="Terminal" content={claudeEnvUnix} />}
            {claudeTab === 'env-windows' && <CodeBlock label="Command Prompt" content={claudeEnvWindows} />}
            {claudeTab === 'settings' && <CodeBlock label="~/.claude/settings.json" content={claudeSettings} />}
          </>
        )}

        {tool.id === 'cc-switch' && (
          <div className="space-y-4">
            <CodeBlock label="cc-switch deeplink" content={ccSwitchUrl} />
            <Button
              asChild
              disabled={!activeKey}
              className="gap-2"
            >
              <a href={ccSwitchUrl}>
                <ExternalLink className="size-4" />
                Import to CC Switch
              </a>
            </Button>
            {!activeKey && <p className="text-xs text-amber-600 dark:text-amber-400">Create or select an API key before launching the deeplink.</p>}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

export default function Guide() {
  const baseUrl = useMemo(() => window.location.origin, [])
  const [apiKeys, setApiKeys] = useState<ApiKeyOption[]>([])
  const [selectedKey, setSelectedKey] = useState('')
  const [models, setModels] = useState(FALLBACK_MODELS)
  const [selectedModel, setSelectedModel] = useState('gpt-5.5')
  const [curlTab, setCurlTab] = useState<'responses' | 'chat' | 'messages'>('responses')

  useEffect(() => {
    api.getAPIKeys().then((res) => {
      const keys = (res.keys ?? []).map((item) => ({ name: item.name, key: item.raw_key || item.key }))
      setApiKeys(keys)
      if (keys[0]) setSelectedKey(keys[0].key)
    }).catch(() => {})

    api.getModels().then((res) => {
      const next = (res.models?.length ? res.models : res.items?.map((item) => item.id) ?? [])
        .filter((model): model is string => Boolean(model))
      if (next.length > 0) {
        setModels(next)
        setSelectedModel(next.includes('gpt-5.5') ? 'gpt-5.5' : next[0])
      }
    }).catch(() => {})
  }, [])

  const activeKey = selectedKey || apiKeys[0]?.key || ''
  const keyForSnippet = activeKey || 'YOUR_API_KEY'
  const messagesModel = selectedModel.startsWith('claude-') ? selectedModel : 'claude-sonnet-4-5-20250514'

  const curlExamples = {
    responses: `curl -X POST ${baseUrl}/v1/responses \\
  -H "Authorization: Bearer ${keyForSnippet}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${selectedModel}",
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "Hello"}]}],
    "stream": true
  }'`,
    chat: `curl -X POST ${baseUrl}/v1/chat/completions \\
  -H "Authorization: Bearer ${keyForSnippet}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${selectedModel}",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'`,
    messages: `curl -X POST ${baseUrl}/v1/messages \\
  -H "x-api-key: ${keyForSnippet}" \\
  -H "Content-Type: application/json" \\
  -H "anthropic-version: 2023-06-01" \\
  -d '{
    "model": "${messagesModel}",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'`,
  }

  const tocItems = [
    ['quick-start', 'Quick Start'],
    ['client-codex', 'Codex CLI'],
    ['client-claude', 'Claude Code'],
    ['client-cc-switch', 'CC Switch'],
    ['curl-examples', 'cURL Examples'],
    ['auth', 'Authentication'],
  ]

  return (
    <>
      <PageHeader
        title="Usage Guide"
        description="Configure clients and test the OpenAI / Anthropic compatible endpoints."
      />

      <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_240px]">
        <div className="min-w-0 space-y-4">
          <Card id="quick-start" className="scroll-mt-20">
            <CardContent className="p-5">
              <div className="flex flex-wrap items-end justify-between gap-3">
                <div>
                  <h2 className="text-lg font-bold text-foreground">Quick Start</h2>
                  <p className="mt-1 text-sm text-muted-foreground">Choose a key once; client cards and cURL examples update together.</p>
                </div>
                <div className="min-w-[240px]">
                  <label className="mb-1.5 block text-xs font-semibold uppercase tracking-wide text-muted-foreground">API Key</label>
                  {apiKeys.length > 0 ? (
                    <Select
                      value={selectedKey}
                      onValueChange={setSelectedKey}
                      options={apiKeys.map((item) => ({ label: keyLabel(item), value: item.key }))}
                    />
                  ) : (
                    <a
                      href="/admin/api-keys"
                      className="inline-flex h-10 items-center rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 text-sm font-semibold text-amber-700 dark:text-amber-300"
                    >
                      Create an API key first
                    </a>
                  )}
                </div>
              </div>
            </CardContent>
          </Card>

          <div className="grid gap-4 lg:grid-cols-3">
            {CLIENT_TOOLS.map((tool) => (
              <ClientCard key={tool.id} tool={tool} activeKey={activeKey} baseUrl={baseUrl} />
            ))}
          </div>

          <Card id="curl-examples" className="scroll-mt-20">
            <CardContent className="p-5">
              <div className="mb-4 flex flex-wrap items-end justify-between gap-3">
                <div>
                  <h2 className="text-lg font-bold text-foreground">cURL Examples</h2>
                  <p className="mt-1 text-sm text-muted-foreground">Switch endpoint format and model without editing the snippet manually.</p>
                </div>
                <div className="min-w-[220px]">
                  <label className="mb-1.5 block text-xs font-semibold uppercase tracking-wide text-muted-foreground">Model</label>
                  <Select
                    value={selectedModel}
                    onValueChange={setSelectedModel}
                    options={models.map((model) => ({ label: model, value: model }))}
                  />
                </div>
              </div>
              <Tabs
                tabs={[
                  { id: 'responses', label: '/v1/responses' },
                  { id: 'chat', label: '/v1/chat/completions' },
                  { id: 'messages', label: '/v1/messages' },
                ]}
                active={curlTab}
                onChange={setCurlTab}
              />
              <CodeBlock label="cURL" content={curlExamples[curlTab]} />
            </CardContent>
          </Card>

          <Card id="auth" className="scroll-mt-20">
            <CardContent className="p-5">
              <h2 className="text-lg font-bold text-foreground">Authentication</h2>
              <p className="mt-1 text-sm text-muted-foreground">Downstream API requests accept standard OpenAI and Anthropic header styles.</p>
              <div className="mt-4 grid gap-2">
                <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2">
                  <Badge variant="outline">Header</Badge>
                  <code className="font-mono text-sm">Authorization: Bearer &lt;key&gt;</code>
                </div>
                <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2">
                  <Badge variant="outline">Header</Badge>
                  <code className="font-mono text-sm">x-api-key: &lt;key&gt;</code>
                </div>
                <div className="flex items-center gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2">
                  <Badge variant="outline">Header</Badge>
                  <code className="font-mono text-sm">anthropic-auth-token: &lt;key&gt;</code>
                </div>
              </div>
            </CardContent>
          </Card>
        </div>

        <aside className="sticky top-4 hidden max-h-[calc(100dvh-2rem)] self-start overflow-y-auto xl:block">
          <div className="rounded-lg border border-border bg-card p-4">
            <div className="mb-3 text-xs font-bold uppercase tracking-wide text-muted-foreground">On this page</div>
            <nav className="space-y-1">
              {tocItems.map(([id, label]) => (
                <a
                  key={id}
                  href={`#${id}`}
                  className="block rounded-md px-2 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
                >
                  {label}
                </a>
              ))}
            </nav>
          </div>
        </aside>
      </div>
    </>
  )
}
