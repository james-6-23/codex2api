package timezone

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

func Load(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" || len(name) > 100 {
		return nil, fmt.Errorf("invalid IANA timezone: %q", name)
	}
	return time.LoadLocation(name)
}

func Normalize(name string) string {
	location, err := Load(name)
	if err != nil {
		return ""
	}
	return location.String()
}
