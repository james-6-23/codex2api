import { useTranslation } from 'react-i18next'
import ChannelLogo from './ChannelLogo'
import type { UpstreamChannel } from '../types'
import { cn } from '@/lib/utils'

// 设置项适用渠道标记：用在 SettingsCard / SettingField 标题旁，让"这条设置对哪家上游生效"
// 一眼可见。四家全适用时只显示四个 logo；单一渠道时补渠道名。
export const ALL_UPSTREAM_CHANNELS: readonly UpstreamChannel[] = ['codex', 'claude', 'antigravity', 'grok']

const CHANNEL_NAMES: Record<UpstreamChannel, string> = {
  codex: 'Codex',
  claude: 'Claude',
  antigravity: 'Antigravity',
  grok: 'Grok',
}

export function channelScopeLabel(channels: readonly UpstreamChannel[]): string {
  return channels.map((channel) => CHANNEL_NAMES[channel]).join(' / ')
}

export default function ChannelScopeBadges({
  channels,
  size = 'sm',
  className,
}: {
  channels: readonly UpstreamChannel[]
  size?: 'sm' | 'xs'
  className?: string
}) {
  const { t } = useTranslation()
  if (channels.length === 0) return null
  const coversAll = ALL_UPSTREAM_CHANNELS.every((channel) => channels.includes(channel))
  const label = coversAll
    ? t('settings.channelScopeAll')
    : t('settings.channelScope', { channels: channelScopeLabel(channels) })
  const logoSize = size === 'xs' ? 11 : 13
  return (
    <span
      title={label}
      aria-label={label}
      data-channel-scope={channels.join(',')}
      className={cn(
        'inline-flex shrink-0 items-center gap-1 rounded-full border border-border/70 bg-muted/40 px-1.5 py-0.5 text-[10px] font-medium leading-none text-muted-foreground',
        className,
      )}
    >
      {channels.map((channel) => (
        <ChannelLogo key={channel} channel={channel} size={logoSize} />
      ))}
      {channels.length === 1 ? <span>{CHANNEL_NAMES[channels[0]]}</span> : null}
    </span>
  )
}
