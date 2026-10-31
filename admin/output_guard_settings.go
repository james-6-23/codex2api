package admin

import (
	"fmt"

	"github.com/codex2api/database"
)

func validateOutputGuardSettingsUpdateRanges(update database.OutputGuardSettingsUpdate) error {
	switch {
	case update.MaxToolArgumentBytes != nil &&
		(*update.MaxToolArgumentBytes < database.MinOutputGuardMaxToolArgumentBytes ||
			*update.MaxToolArgumentBytes > database.MaxOutputGuardMaxToolArgumentBytes):
		return fmt.Errorf("%w: output_guard_max_tool_argument_bytes must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxToolArgumentBytes, database.MaxOutputGuardMaxToolArgumentBytes)
	case update.MaxToolArgumentDeltas != nil &&
		(*update.MaxToolArgumentDeltas < database.MinOutputGuardMaxToolArgumentDeltas ||
			*update.MaxToolArgumentDeltas > database.MaxOutputGuardMaxToolArgumentDeltas):
		return fmt.Errorf("%w: output_guard_max_tool_argument_deltas must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxToolArgumentDeltas, database.MaxOutputGuardMaxToolArgumentDeltas)
	case update.MaxChannelBytes != nil &&
		(*update.MaxChannelBytes < database.MinOutputGuardMaxChannelBytes ||
			*update.MaxChannelBytes > database.MaxOutputGuardMaxChannelBytes):
		return fmt.Errorf("%w: output_guard_max_channel_bytes must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxChannelBytes, database.MaxOutputGuardMaxChannelBytes)
	case update.MaxRequestBytes != nil &&
		(*update.MaxRequestBytes < database.MinOutputGuardMaxRequestBytes ||
			*update.MaxRequestBytes > database.MaxOutputGuardMaxRequestBytes):
		return fmt.Errorf("%w: output_guard_max_request_bytes must be between %d and %d",
			database.ErrInvalidOutputGuardSettings, database.MinOutputGuardMaxRequestBytes, database.MaxOutputGuardMaxRequestBytes)
	default:
		return nil
	}
}
