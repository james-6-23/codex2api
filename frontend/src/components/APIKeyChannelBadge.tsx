import { Waypoints } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { UpstreamChannel } from '../types'
import ChannelLogo from './ChannelLogo'
import { Badge } from './ui/badge'

const CHANNEL_NAMES: Record<UpstreamChannel, string> = {
  codex: 'Codex',
  grok: 'Grok',
  antigravity: 'Antigravity',
  claude: 'Claude',
}

export default function APIKeyChannelBadge({ channel }: { channel?: UpstreamChannel }) {
  const { t } = useTranslation()
  const known = channel && Object.prototype.hasOwnProperty.call(CHANNEL_NAMES, channel) ? channel : undefined
  return (
    <Badge
      variant="outline"
      title={t(known ? `apiKeys.limits.upstreamChannel${CHANNEL_NAMES[known]}` : 'apiKeys.limits.upstreamChannelHint.auto')}
      className={`gap-1 border-transparent bg-muted/70 px-1.5 py-0 text-[11px] font-semibold ${known ? 'text-foreground' : 'text-muted-foreground'}`}
    >
      {known ? <ChannelLogo channel={known} size={12} /> : <Waypoints className="size-3" />}
      {known ? CHANNEL_NAMES[known] : t('apiKeys.limits.upstreamChannelAutoTab')}
    </Badge>
  )
}
