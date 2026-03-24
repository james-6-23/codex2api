interface BadgeInfo {
  cls: string
  text: string
}

interface StatusBadgeProps {
  status?: string | null
}

const statusMap: Record<string, BadgeInfo> = {
  active: { cls: 'badge-success', text: '可用' },
  ready: { cls: 'badge-success', text: '就绪' },
  cooldown: { cls: 'badge-warning', text: '冷却中' },
  error: { cls: 'badge-danger', text: '错误' },
  paused: { cls: 'badge-info', text: '已暂停' },
}

export default function StatusBadge({ status }: StatusBadgeProps) {
  const text = status ?? 'unknown'
  const info = statusMap[text] ?? { cls: 'badge-info', text }

  return (
    <span className={`badge ${info.cls}`}>
      <span className="badge-dot" />
      {info.text}
    </span>
  )
}
