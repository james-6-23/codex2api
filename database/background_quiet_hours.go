package database

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// BackgroundQuietPeriod is a daily half-open interval in Beijing time (UTC+8).
// An end before the start crosses midnight; only an end may use 24:00.
type BackgroundQuietPeriod struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func QuietHourMinute(value string, end bool) (int, error) {
	if end && value == "24:00" {
		return 1440, nil
	}
	if len(value) != 5 || value[2] != ':' {
		return 0, fmt.Errorf("时间必须为 HH:MM")
	}
	h, e1 := strconv.Atoi(value[:2])
	m, e2 := strconv.Atoi(value[3:])
	if e1 != nil || e2 != nil || h < 0 || h > 23 || m < 0 || m > 59 || fmt.Sprintf("%02d:%02d", h, m) != value {
		return 0, fmt.Errorf("无效时间 %q", value)
	}
	return h*60 + m, nil
}

func NormalizeBackgroundQuietHours(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "[]"
	}
	var periods []BackgroundQuietPeriod
	if err := json.Unmarshal([]byte(raw), &periods); err != nil || strings.TrimSpace(raw) == "null" {
		return "", fmt.Errorf("巡检暂停时段必须为数组")
	}
	if len(periods) > 32 {
		return "", fmt.Errorf("巡检暂停时段最多 32 个")
	}
	for i, period := range periods {
		start, err := QuietHourMinute(period.Start, false)
		if err != nil {
			return "", fmt.Errorf("第 %d 个暂停时段开始时间: %w", i+1, err)
		}
		end, err := QuietHourMinute(period.End, true)
		if err != nil {
			return "", fmt.Errorf("第 %d 个暂停时段结束时间: %w", i+1, err)
		}
		if start == end {
			return "", fmt.Errorf("第 %d 个暂停时段开始和结束不能相同；全天暂停请用 00:00–24:00", i+1)
		}
	}
	if periods == nil {
		periods = []BackgroundQuietPeriod{}
	}
	encoded, _ := json.Marshal(periods)
	return string(encoded), nil
}
