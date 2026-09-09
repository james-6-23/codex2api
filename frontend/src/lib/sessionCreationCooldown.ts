export interface SessionCreationCooldownTier {
  min_average_seconds: number
  interval_seconds: number
}

export interface SessionCreationCooldownConfig {
  mode: 'off' | 'observe' | 'enforce'
  frequency_window_seconds: number
  free_creations: number
  history_days: number
  min_samples: number
  max_samples: number
  max_interval_seconds: number
  tiers: SessionCreationCooldownTier[]
}

export function defaultSessionCreationCooldown(): SessionCreationCooldownConfig {
  return {
    mode: 'off', frequency_window_seconds: 1800, free_creations: 2,
    history_days: 7, min_samples: 10, max_samples: 20, max_interval_seconds: 900,
    tiers: [
      { min_average_seconds: 900, interval_seconds: 0 },
      { min_average_seconds: 600, interval_seconds: 300 },
      { min_average_seconds: 300, interval_seconds: 600 },
      { min_average_seconds: 0, interval_seconds: 900 },
    ],
  }
}

export function parseSessionCreationCooldown(value: unknown): SessionCreationCooldownConfig {
  const defaults = defaultSessionCreationCooldown()
  if (!value || typeof value !== 'object' || Array.isArray(value)) return defaults
  const raw = value as Record<string, unknown>
  const config = { ...defaults }
  if (raw.mode === 'off' || raw.mode === 'observe' || raw.mode === 'enforce') config.mode = raw.mode
  for (const key of ['frequency_window_seconds', 'free_creations', 'history_days', 'min_samples', 'max_samples', 'max_interval_seconds'] as const) {
    if (typeof raw[key] === 'number' && Number.isFinite(raw[key])) config[key] = raw[key]
  }
  if (Array.isArray(raw.tiers)) {
    config.tiers = raw.tiers.filter((tier): tier is SessionCreationCooldownTier =>
      tier && typeof tier === 'object' && Number.isFinite(tier.min_average_seconds) && Number.isFinite(tier.interval_seconds))
  }
  return config
}

export function sessionCreationCooldownInterval(config: SessionCreationCooldownConfig, averageSeconds: number, samples: number): number {
  if (config.mode === 'off' || samples < config.min_samples || !Number.isFinite(averageSeconds)) return 0
  const tier = [...config.tiers].sort((left, right) => right.min_average_seconds - left.min_average_seconds)
    .find((candidate) => averageSeconds >= candidate.min_average_seconds)
  return Math.min(tier?.interval_seconds ?? 0, config.max_interval_seconds)
}
