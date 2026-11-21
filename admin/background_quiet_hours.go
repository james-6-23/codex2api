package admin

import (
	"encoding/json"
	"github.com/codex2api/database"
)

func decodeBackgroundQuietHours(raw string) []database.BackgroundQuietPeriod {
	periods := []database.BackgroundQuietPeriod{}
	_ = json.Unmarshal([]byte(raw), &periods)
	return periods
}
