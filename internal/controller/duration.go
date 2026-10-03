package controller

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var extendedDurationPattern = regexp.MustCompile(`^[0-9]+[smhd]$`)

// parseExtendedDuration accepts one positive integer with s, m, h, or d.
func parseExtendedDuration(durationStr string) (time.Duration, error) {
	if strings.ContainsAny(durationStr, "+-") {
		return 0, fmt.Errorf("duration must be positive and unsigned")
	}
	if !extendedDurationPattern.MatchString(durationStr) {
		return 0, fmt.Errorf("invalid duration format: %q", durationStr)
	}

	var duration time.Duration
	if strings.HasSuffix(durationStr, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(durationStr, "d"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("number of days is too large: %s", durationStr)
		}
		if days > math.MaxInt64/int64(24*time.Hour) {
			return 0, fmt.Errorf("number of days is too large: %s", durationStr)
		}
		duration = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		duration, err = time.ParseDuration(durationStr)
		if err != nil {
			return 0, err
		}
	}
	if duration <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return duration, nil
}

func parseRestartEvery(durationStr string) (time.Duration, error) {
	duration, err := parseExtendedDuration(durationStr)
	if err != nil {
		return 0, err
	}
	if duration < minimumRestartEveryInterval {
		return 0, fmt.Errorf("duration must be at least %s", minimumRestartEveryInterval)
	}
	return duration, nil
}
