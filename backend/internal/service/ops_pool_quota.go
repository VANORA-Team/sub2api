package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// PoolQuotaWindow measures account-equivalents, not an absolute token allowance.
// Plans can have different limits, so these values must not be treated as tokens.
type PoolQuotaWindow struct {
	Known     int64   `json:"known"`
	Unknown   int64   `json:"unknown"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
}

type PoolQuotaSummary struct {
	FiveHour PoolQuotaWindow `json:"five_hour"`
	SevenDay PoolQuotaWindow `json:"seven_day"`
}

const poolQuotaMaxAge = 2 * time.Hour

func addCodexPoolQuota(summary *PoolQuotaSummary, acc *Account, now time.Time, observations ...*PoolQuotaObservation) {
	if acc == nil || acc.Platform != PlatformOpenAI || acc.Type != AccountTypeOAuth || acc.ParentAccountID != nil {
		return
	}
	if len(observations) > 0 && observations[0] != nil {
		for _, item := range []struct {
			name   string
			window *PoolQuotaWindow
		}{{"5h", &summary.FiveHour}, {"7d", &summary.SevenDay}} {
			found := false
			for _, tier := range observations[0].Windows {
				if tier.Window == item.name && tier.Label == "" && poolTierCurrent(tier, now) &&
					!math.IsNaN(tier.UsedPercent) && !math.IsInf(tier.UsedPercent, 0) && tier.UsedPercent >= 0 {
					used := math.Min(tier.UsedPercent, 100) / 100
					item.window.Known++
					item.window.Used += used
					item.window.Remaining += 1 - used
					found = true
					break
				}
			}
			if !found {
				item.window.Unknown++
			}
		}
		return
	}
	addCodexPoolWindow(&summary.FiveHour, acc.Extra, "5h", now)
	addCodexPoolWindow(&summary.SevenDay, acc.Extra, "7d", now)
}

func addCodexPoolWindow(window *PoolQuotaWindow, extra map[string]any, name string, now time.Time) {
	raw, present := extra["codex_"+name+"_used_percent"]
	updated, err := parseTime(fmt.Sprint(extra["codex_usage_updated_at"]))
	reset, hasReset := openAICodexWindowResetAt(extra, name)
	if !present || err != nil || updated.After(now.Add(time.Minute)) || now.Sub(updated) > poolQuotaMaxAge ||
		!hasReset || !now.Before(reset) {
		window.Unknown++
		return
	}
	used, valid := poolFiniteNonnegative(raw)
	if !valid {
		window.Unknown++
		return
	}
	used = math.Min(used, 100) / 100
	window.Known++
	window.Used += used
	window.Remaining += 1 - used
}

// Reject malformed values instead of silently interpreting them as zero.
func poolFiniteNonnegative(raw any) (float64, bool) {
	var used float64
	var err error
	switch value := raw.(type) {
	case float64:
		used = value
	case float32:
		used = float64(value)
	case int:
		used = float64(value)
	case int64:
		used = float64(value)
	case json.Number:
		used, err = value.Float64()
	case string:
		used, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
	default:
		return 0, false
	}
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
		return 0, false
	}
	return used, true
}
