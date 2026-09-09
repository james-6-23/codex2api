package promptfilter

import (
	"fmt"
	"sort"
)

type SessionCreationCooldownTier struct {
	MinAverageSeconds int `json:"min_average_seconds"`
	IntervalSeconds   int `json:"interval_seconds"`
}

type SessionCreationCooldownConfig struct {
	Mode                   string                        `json:"mode"`
	FrequencyWindowSeconds int                           `json:"frequency_window_seconds"`
	FreeCreations          int                           `json:"free_creations"`
	HistoryDays            int                           `json:"history_days"`
	MinSamples             int                           `json:"min_samples"`
	MaxSamples             int                           `json:"max_samples"`
	MaxIntervalSeconds     int                           `json:"max_interval_seconds"`
	Tiers                  []SessionCreationCooldownTier `json:"tiers"`
}

func DefaultSessionCreationCooldownConfig() SessionCreationCooldownConfig {
	return SessionCreationCooldownConfig{
		Mode: "off", FrequencyWindowSeconds: 1800, FreeCreations: 2,
		HistoryDays: 7, MinSamples: 10, MaxSamples: 20, MaxIntervalSeconds: 900,
		Tiers: []SessionCreationCooldownTier{{900, 0}, {600, 300}, {300, 600}, {0, 900}},
	}
}

func (cfg SessionCreationCooldownConfig) Validate() error {
	if cfg.Mode != "off" && cfg.Mode != "observe" && cfg.Mode != "enforce" {
		return fmt.Errorf("session_creation_cooldown.mode must be off, observe or enforce")
	}
	if cfg.FrequencyWindowSeconds < 60 || cfg.FrequencyWindowSeconds > 86400 || cfg.FreeCreations < 1 || cfg.FreeCreations > 1000 {
		return fmt.Errorf("session_creation_cooldown: frequency window must be 60..86400 seconds and free creations 1..1000")
	}
	if cfg.HistoryDays < 1 || cfg.HistoryDays > 90 || cfg.MinSamples < 1 || cfg.MaxSamples < cfg.MinSamples || cfg.MaxSamples > 200 {
		return fmt.Errorf("session_creation_cooldown: history must be 1..90 days and 1 <= min_samples <= max_samples <= 200")
	}
	if cfg.MaxIntervalSeconds < 0 || cfg.MaxIntervalSeconds > 86400 || len(cfg.Tiers) < 1 || len(cfg.Tiers) > 12 {
		return fmt.Errorf("session_creation_cooldown: maximum interval must be 0..86400 seconds with 1..12 tiers")
	}
	seen := make(map[int]bool)
	for _, tier := range cfg.Tiers {
		if tier.MinAverageSeconds < 0 || tier.MinAverageSeconds > 2592000 || tier.IntervalSeconds < 0 || tier.IntervalSeconds > 86400 || seen[tier.MinAverageSeconds] {
			return fmt.Errorf("session_creation_cooldown: tier lower bounds must be unique (0..2592000 seconds), intervals 0..86400 seconds")
		}
		seen[tier.MinAverageSeconds] = true
	}
	if !seen[0] {
		return fmt.Errorf("session_creation_cooldown: a tier starting at zero is required")
	}
	return nil
}

func (cfg SessionCreationCooldownConfig) Interval(averageSeconds float64, samples int) int {
	if cfg.Mode == "off" || samples < cfg.MinSamples || cfg.Validate() != nil {
		return 0
	}
	tiers := append([]SessionCreationCooldownTier(nil), cfg.Tiers...)
	sort.Slice(tiers, func(left, right int) bool { return tiers[left].MinAverageSeconds > tiers[right].MinAverageSeconds })
	for _, tier := range tiers {
		if averageSeconds >= float64(tier.MinAverageSeconds) {
			return min(tier.IntervalSeconds, cfg.MaxIntervalSeconds)
		}
	}
	return 0
}
