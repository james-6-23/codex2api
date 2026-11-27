import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { cn } from '@/lib/utils'

async function copyString(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Fall through to execCommand fallback.
    }
  }
  try {
    const textarea = document.createElement('textarea')
    textarea.value = text
    textarea.setAttribute('readonly', 'true')
    textarea.style.position = 'fixed'
    textarea.style.opacity = '0'
    document.body.appendChild(textarea)
    textarea.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(textarea)
    return ok
  } catch {
    return false
  }
}

/**
 * Line-numbered code / JSON / diff viewer inspired by beautifului.dev #18 Code Block.
 */
export function CodeSurface({
  code,
  title,
  language,
  diff = false,
  maxHeightClassName = 'max-h-[320px]',
  className,
}: {
  code: string
  title?: string
  language?: string
  diff?: boolean
  maxHeightClassName?: string
  className?: string
}) {
  const [copied, setCopied] = useState(false)
  const lines = code.replace(/\r\n/g, '\n').split('\n')

  const handleCopy = async () => {
    const ok = await copyString(code)
    if (!ok) return
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1600)
  }

  return (
    <div
      className={cn(
        'overflow-hidden rounded-xl border border-border/80 bg-card shadow-2xs dark:border-white/10',
        className,
      )}
    >
      {(title || language) && (
        <div className="flex items-center justify-between gap-2 border-b border-border/70 bg-muted/35 px-3 py-1.5 text-[11px] dark:bg-white/[0.03]">
          <div className="flex min-w-0 items-center gap-2">
            {title ? <span className="truncate font-medium text-foreground">{title}</span> : null}
            {language ? (
              <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wider text-muted-foreground">
                {language}
              </span>
            ) : null}
            <span className="font-mono text-[10px] tabular-nums text-muted-foreground/75">
              {lines.length}L
            </span>
          </div>
          <button
            type="button"
            onClick={() => void handleCopy()}
            className="inline-flex h-6 items-center gap-1 rounded-md px-2 text-[11px] font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            aria-label="Copy code"
          >
            {copied ? (
              <>
                <Check className="size-3 text-emerald-500" />
                <span className="text-emerald-600 dark:text-emerald-400">已复制</span>
              </>
            ) : (
              <>
                <Copy className="size-3" />
                <span>复制</span>
              </>
            )}
          </button>
        </div>
      )}
      <div className={cn('overflow-auto font-mono text-[11.5px] leading-relaxed', maxHeightClassName)}>
        <table className="w-full border-collapse">
          <tbody>
            {lines.map((line, idx) => {
              const isAdd = diff && line.startsWith('+') && !line.startsWith('+++')
              const isDel = diff && line.startsWith('-') && !line.startsWith('---')
              return (
                <tr
                  key={idx}
                  className={cn(
                    'transition-colors hover:bg-muted/35',
                    isAdd && 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
                    isDel && 'bg-destructive/10 text-destructive dark:text-red-300',
                  )}
                >
                  <td className="w-10 shrink-0 select-none border-r border-border/50 px-2.5 py-0.5 text-right text-[10.5px] tabular-nums text-muted-foreground/55">
                    {idx + 1}
                  </td>
                  <td className="whitespace-pre-wrap break-all px-3 py-0.5 text-foreground/90">
                    {line || ' '}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}
