package auth

import (
	"encoding/json"
	"time"

	"github.com/codex2api/database"
)

var backgroundBeijing = time.FixedZone("Asia/Shanghai", 8*60*60)

type backgroundQuietSchedule struct {
	raw     string
	minutes [1440]bool
}

func (s *Store) SetBackgroundQuietHours(raw string) error {
	normalized, err := database.NormalizeBackgroundQuietHours(raw)
	if err != nil {
		return err
	}
	var periods []database.BackgroundQuietPeriod
	_ = json.Unmarshal([]byte(normalized), &periods)
	schedule := &backgroundQuietSchedule{raw: normalized}
	for _, p := range periods {
		start, _ := database.QuietHourMinute(p.Start, false)
		end, _ := database.QuietHourMinute(p.End, true)
		if end < start {
			end += 1440
		}
		for i := start; i < end; i++ {
			schedule.minutes[i%1440] = true
		}
	}
	s.backgroundQuietHours.Store(schedule)
	select {
	case s.backgroundRefreshWakeCh <- struct{}{}:
	default:
	}
	return nil
}

func (s *Store) GetBackgroundQuietHours() string {
	if s != nil {
		if schedule := s.backgroundQuietHours.Load(); schedule != nil {
			return schedule.raw
		}
	}
	return "[]"
}

// BackgroundInspectionPaused never depends on the host/container timezone.
func (s *Store) BackgroundInspectionPaused(now time.Time) bool {
	if s == nil {
		return false
	}
	schedule := s.backgroundQuietHours.Load()
	if schedule == nil {
		return false
	}
	local := now.In(backgroundBeijing)
	return schedule.minutes[local.Hour()*60+local.Minute()]
}

// NextBackgroundInspectionTime returns zero for a schedule covering all day.
func (s *Store) NextBackgroundInspectionTime(now time.Time) time.Time {
	if s == nil {
		return now
	}
	schedule := s.backgroundQuietHours.Load()
	if schedule == nil {
		return now
	}
	local := now.In(backgroundBeijing)
	minute := local.Hour()*60 + local.Minute()
	if !schedule.minutes[minute] {
		return now
	}
	for offset := 1; offset <= 1440; offset++ {
		if !schedule.minutes[(minute+offset)%1440] {
			return local.Truncate(time.Minute).Add(time.Duration(offset) * time.Minute)
		}
	}
	return time.Time{}
}

func (s *Store) backgroundRefreshDelay(now time.Time) time.Duration {
	if s.BackgroundInspectionPaused(now) {
		next := s.NextBackgroundInspectionTime(now)
		if next.IsZero() {
			return 24 * time.Hour
		}
		return next.Sub(now)
	}
	return s.GetBackgroundRefreshInterval()
}
