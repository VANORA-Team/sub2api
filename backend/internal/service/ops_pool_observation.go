package service

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

var poolExplicitUSDBalance = regexp.MustCompile(`(?i)^\s*(?:USD\s*\$?\s*|\$\s*)(-?(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?)\s*$`)

func poolOpenAIObservation(account *Account, usage *OpenAIQuotaUsage, now time.Time) *PoolQuotaObservation {
	if account == nil || usage == nil || usage.FetchedAt <= 0 {
		return nil
	}
	at := time.Unix(usage.FetchedAt, 0)
	if !poolObservationFresh(&at, now) {
		return nil
	}
	observation := &PoolQuotaObservation{Source: "upstream", ObservedAt: at}
	appendLimit := func(limit *OpenAIRateLimit, label string) {
		if limit == nil {
			return
		}
		for _, window := range []*OpenAIRateLimitWindow{limit.PrimaryWindow, limit.SecondaryWindow} {
			if window == nil {
				continue
			}
			if window.MissingUsedPercent || window.LimitWindowSeconds <= 0 || window.UsedPercent < 0 ||
				math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
				observation.Partial = true
				continue
			}
			reset := time.Time{}
			if window.ResetAt > 0 {
				reset = time.Unix(window.ResetAt, 0)
			} else if window.ResetAfterSeconds > 0 {
				reset = at.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
			}
			if reset.IsZero() || !now.Before(reset) {
				observation.Partial = true
				continue
			}
			name := fmt.Sprintf("%ds", window.LimitWindowSeconds)
			if window.LimitWindowSeconds%(24*60*60) == 0 {
				name = fmt.Sprintf("%dd", window.LimitWindowSeconds/(24*60*60))
			} else if window.LimitWindowSeconds%(60*60) == 0 {
				name = fmt.Sprintf("%dh", window.LimitWindowSeconds/(60*60))
			}
			observation.Windows = append(observation.Windows, domain.MonitorQuotaTier{
				Window: name, Label: label, UsedPercent: window.UsedPercent, ResetAt: reset.UTC().Format(time.RFC3339),
			})
		}
	}
	if !account.IsShadow() {
		appendLimit(usage.RateLimit, "")
	}
	for _, extra := range usage.AdditionalRateLimits {
		if account.IsShadow() && extra.MeteredFeature != "codex_bengalfox" {
			continue
		}
		label := strings.TrimSpace(extra.MeteredFeature)
		if label == "" {
			label = strings.TrimSpace(extra.LimitName)
		}
		if account.IsShadow() {
			label = ""
		} else if label == "" {
			label = "additional"
		}
		appendLimit(extra.RateLimit, label)
	}
	return nonEmptyPoolObservation(observation)
}

// PoolQuotaObservation retains the units and origin of each upstream observation.
// In particular, a configured or estimated allowance is not a cash balance.
type PoolQuotaObservation struct {
	Source     string                    `json:"source"`
	ObservedAt time.Time                 `json:"observed_at"`
	Partial    bool                      `json:"partial,omitempty"`
	Windows    []domain.MonitorQuotaTier `json:"windows,omitempty"`
	Balances   []domain.MonitorBalance   `json:"balances,omitempty"`
	BalanceLow *bool                     `json:"balance_low,omitempty"`
	Credits    []AICredit                `json:"credits,omitempty"`
	RawBalance string                    `json:"raw_balance,omitempty"`
}

// poolUsageObservation converts the account usage service's different provider
// responses into a conservative observation suitable for account-pool monitoring.
// A nil result means the data was missing, expired, or had no usable value.
func poolUsageObservation(account *Account, usage *UsageInfo, now time.Time) *PoolQuotaObservation {
	if account == nil || usage == nil {
		return nil
	}
	if failed, credentialInvalid, _ := usageFailureInfo(usage); failed &&
		!(account.Platform == PlatformGrok && usage.ErrorCode == errorCodeRateLimited && !credentialInvalid) {
		return nil
	}

	observedAt := usage.UpdatedAt
	source := "upstream"
	switch account.Platform {
	case PlatformOpenAI:
		// GetUsage can synthesize empty progress bars from local usage logs.
		// Only the persisted Codex snapshot represents observed upstream quota.
		at, err := parseTime(stringValue(account.Extra["codex_usage_updated_at"]))
		if err != nil {
			return nil
		}
		observedAt = &at
		if !poolObservationFresh(observedAt, now) {
			return nil
		}
		observation := &PoolQuotaObservation{Source: source, ObservedAt: at}
		for _, window := range []string{"5h", "7d"} {
			reset, ok := openAICodexWindowResetAt(account.Extra, window)
			if _, valid := poolFiniteNonnegative(account.Extra["codex_"+window+"_used_percent"]); !valid {
				continue
			}
			progress := buildCodexUsageProgressFromExtra(account.Extra, window, now)
			if ok && now.Before(reset) && progress != nil {
				poolAppendProgress(&observation.Windows, window, "", progress, now)
			}
		}
		return nonEmptyPoolObservation(observation)
	case PlatformAnthropic:
		if account.Type == AccountTypeSetupToken {
			// Setup tokens have no profile API: their windows are passive samples.
			at, err := parseTime(stringValue(account.Extra["passive_usage_sampled_at"]))
			if err != nil {
				return nil
			}
			observedAt = &at
			source = "passive"
		}
	case PlatformGemini:
		// Gemini quotas are calculated from configured tier limits and local logs.
		source = "local_estimate"
	case PlatformGrok:
		return poolGrokObservation(account, usage, now)
	}
	if !poolObservationFresh(observedAt, now) {
		return nil
	}
	observation := &PoolQuotaObservation{Source: source, ObservedAt: *observedAt, Partial: usage.QuotaIncomplete}
	appendWindow := func(name, label string, value *UsageProgress) {
		if value != nil && !poolAppendProgress(&observation.Windows, name, label, value, now) && name != "minute" {
			observation.Partial = true
		}
	}

	if account.Platform == PlatformAnthropic && usage.FiveHour != nil &&
		usage.FiveHour.ResetsAt == nil && usage.FiveHour.Utilization == 0 {
		observation.Partial = true
	}
	if account.Platform != PlatformAnthropic || (usage.FiveHour != nil &&
		(usage.FiveHour.ResetsAt != nil || usage.FiveHour.Utilization > 0) &&
		(account.Type != AccountTypeSetupToken || account.Extra["session_window_utilization"] != nil)) {
		appendWindow("5h", "", usage.FiveHour)
	}
	for _, item := range []struct {
		name  string
		value *UsageProgress
	}{{"7d", usage.SevenDay}, {"7d-sonnet", usage.SevenDaySonnet}, {"7d-fable", usage.SevenDayFable}} {
		if account.Platform == PlatformAnthropic && item.value != nil && item.value.ResetsAt == nil && item.value.Utilization == 0 {
			observation.Partial = true
			continue
		}
		appendWindow(item.name, "", item.value)
	}
	appendWindow("30d", "", usage.ThirtyDay)
	appendWindow("daily", "shared", usage.GeminiSharedDaily)
	appendWindow("daily", "pro", usage.GeminiProDaily)
	appendWindow("daily", "flash", usage.GeminiFlashDaily)
	appendWindow("minute", "shared", usage.GeminiSharedMinute)
	appendWindow("minute", "pro", usage.GeminiProMinute)
	appendWindow("minute", "flash", usage.GeminiFlashMinute)
	for _, model := range sortedQuotaModelNames(usage.AntigravityQuota) {
		quota := usage.AntigravityQuota[model]
		if quota == nil || quota.Utilization < 0 {
			observation.Partial = true
			continue
		}
		tier := domain.MonitorQuotaTier{Window: "total", Label: model, UsedPercent: float64(quota.Utilization), ResetAt: quota.ResetTime}
		if poolTierCurrent(tier, now) {
			observation.Windows = append(observation.Windows, tier)
		} else {
			observation.Partial = true
		}
	}
	for _, credit := range usage.AICredits {
		if !math.IsNaN(credit.Amount) && !math.IsInf(credit.Amount, 0) && credit.Amount >= 0 {
			observation.Credits = append(observation.Credits, credit)
		} else {
			observation.Partial = true
		}
	}
	return nonEmptyPoolObservation(observation)
}

func poolCNQuotaObservation(result *CNProviderQuotaProbeResult, now time.Time) *PoolQuotaObservation {
	if result == nil || !result.Success || result.FetchedAt <= 0 {
		return nil
	}
	at := time.Unix(result.FetchedAt, 0)
	if !poolObservationFresh(&at, now) {
		return nil
	}
	observation := &PoolQuotaObservation{Source: "cn_quota", ObservedAt: at}
	for _, item := range result.Tiers {
		if item.Unknown || math.IsNaN(item.UsedPercent) || math.IsInf(item.UsedPercent, 0) || item.UsedPercent < 0 {
			observation.Partial = true
			continue
		}
		window := item.Window
		if item.Kind == "credit_limit" {
			window = "credit"
		}
		tier := domain.MonitorQuotaTier{Window: window, Label: item.Kind, UsedPercent: item.UsedPercent, ResetAt: item.ResetAt}
		if poolTierCurrent(tier, now) {
			observation.Windows = append(observation.Windows, tier)
		} else {
			observation.Partial = true
		}
	}
	return nonEmptyPoolObservation(observation)
}

func poolCNBalanceObservation(result *CNProviderBalanceResult, threshold float64, now time.Time) *PoolQuotaObservation {
	if result == nil || !result.Success || result.FetchedAt <= 0 {
		return nil
	}
	at := time.Unix(result.FetchedAt, 0)
	if !poolObservationFresh(&at, now) {
		return nil
	}
	observation := &PoolQuotaObservation{Source: "cn_balance", ObservedAt: at}
	entries := result.Balances
	if len(entries) == 0 {
		entries = []CNProviderBalanceEntry{{Currency: result.Currency, Balance: result.Balance}}
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Currency) == "" || math.IsNaN(entry.Balance) || math.IsInf(entry.Balance, 0) {
			// Partial multi-currency data cannot establish that all balances are low.
			return nil
		}
		observation.Balances = append(observation.Balances, domain.MonitorBalance{Currency: entry.Currency, Balance: entry.Balance})
	}
	if len(observation.Balances) == 0 {
		return nil
	}
	// Use the same all-currency policy as the existing provider balance checker.
	low := !result.Available || allCNBalancesBelowThreshold(result, threshold)
	observation.BalanceLow = &low
	return observation
}

func poolOllamaObservation(snapshot *OllamaCloudUsageSnapshot, now time.Time) *PoolQuotaObservation {
	if snapshot == nil || snapshot.Status != OllamaCloudUsageStatusOK || snapshot.Data == nil ||
		!poolObservationFresh(snapshot.FetchedAt, now) {
		return nil
	}
	observation := &PoolQuotaObservation{Source: "ollama_settings", ObservedAt: *snapshot.FetchedAt, RawBalance: strings.TrimSpace(snapshot.Data.Balance)}
	if match := poolExplicitUSDBalance.FindStringSubmatch(observation.RawBalance); len(match) == 2 {
		if amount, err := strconv.ParseFloat(strings.ReplaceAll(match[1], ",", ""), 64); err == nil && !math.IsInf(amount, 0) {
			observation.Balances = append(observation.Balances, domain.MonitorBalance{Currency: "USD", Balance: amount})
		}
	}
	if observation.RawBalance != "" && len(observation.Balances) == 0 {
		observation.Partial = true
	}
	for _, item := range []struct {
		window string
		value  *OllamaCloudUsageWindow
	}{{"5h", snapshot.Data.FiveHour}, {"7d", snapshot.Data.SevenDay}} {
		if item.value == nil {
			continue
		}
		if !poolAppendProgress(&observation.Windows, item.window, "", &UsageProgress{
			Utilization: item.value.UsedPercent,
			ResetsAt:    item.value.ResetAt,
		}, now) {
			observation.Partial = true
		}
	}
	return nonEmptyPoolObservation(observation)
}

func poolObservationFresh(observedAt *time.Time, now time.Time) bool {
	return observedAt != nil && !observedAt.IsZero() && !observedAt.After(now.Add(time.Minute)) &&
		now.Sub(*observedAt) <= poolQuotaMaxAge
}

func nonEmptyPoolObservation(observation *PoolQuotaObservation) *PoolQuotaObservation {
	if observation == nil || (len(observation.Windows) == 0 && len(observation.Balances) == 0 &&
		len(observation.Credits) == 0 && observation.RawBalance == "") {
		return nil
	}
	return observation
}

// Grok billing and rate-limit headers have independent sampling times. Never
// allow fresh headers to make an older billing balance or window look fresh.
func poolGrokObservation(account *Account, usage *UsageInfo, now time.Time) *PoolQuotaObservation {
	observation := &PoolQuotaObservation{Source: "upstream_snapshot"}
	setOldest := func(at time.Time) {
		if observation.ObservedAt.IsZero() || at.Before(observation.ObservedAt) {
			observation.ObservedAt = at
		}
	}
	billing, _ := grokBillingSnapshotFromExtra(account.Extra)
	if billing != nil {
		observation.Partial = billing.Partial
		weeklyAt, weeklyErr := parseTime(strings.TrimSpace(billing.WeeklyUpdatedAt))
		if weeklyErr == nil && poolObservationFresh(&weeklyAt, now) {
			before := len(observation.Windows)
			poolAppendProgress(&observation.Windows, "7d", "", usage.SevenDay, now)
			if len(observation.Windows) != before {
				setOldest(weeklyAt)
			}
			if billing.PrepaidBalance != nil {
				value := *billing.PrepaidBalance
				if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
					observation.Balances = append(observation.Balances, domain.MonitorBalance{Currency: "USD", Balance: value})
					setOldest(weeklyAt)
				}
			}
		} else if billing.UsagePercent != nil || billing.PrepaidBalance != nil {
			observation.Partial = true
		}
		monthlyAt, monthlyErr := parseTime(strings.TrimSpace(billing.MonthlyUpdatedAt))
		if monthlyErr == nil && poolObservationFresh(&monthlyAt, now) {
			before := len(observation.Windows)
			poolAppendProgress(&observation.Windows, "30d", "", usage.ThirtyDay, now)
			if len(observation.Windows) != before {
				setOldest(monthlyAt)
			}
		} else if billing.UsedPercent != nil || (billing.MonthlyLimitCents != nil && billing.UsedCents != nil) {
			observation.Partial = true
		}
	}
	snapshot, _ := grokQuotaSnapshotFromExtra(account.Extra)
	if snapshot != nil {
		headersAt, err := parseTime(strings.TrimSpace(snapshot.LastHeadersSeenAt))
		if err == nil && poolObservationFresh(&headersAt, now) {
			before := len(observation.Windows)
			poolAppendGrokWindow(&observation.Windows, "requests", snapshot.Requests, now)
			poolAppendGrokWindow(&observation.Windows, "tokens", snapshot.Tokens, now)
			if len(observation.Windows) != before {
				setOldest(headersAt)
			}
		} else if snapshot.Requests != nil || snapshot.Tokens != nil {
			observation.Partial = true
		}
	}
	return nonEmptyPoolObservation(observation)
}

func poolAppendProgress(tiers *[]domain.MonitorQuotaTier, window, label string, progress *UsageProgress, now time.Time) bool {
	if progress == nil || math.IsNaN(progress.Utilization) || math.IsInf(progress.Utilization, 0) || progress.Utilization < 0 {
		return false
	}
	tier := domain.MonitorQuotaTier{Window: window, Label: label, UsedPercent: progress.Utilization}
	if progress.ResetsAt != nil {
		tier.ResetAt = progress.ResetsAt.UTC().Format(time.RFC3339)
	}
	if progress.LimitRequests > 0 {
		tier.Used, tier.Limit = float64(progress.UsedRequests), float64(progress.LimitRequests)
	}
	if poolTierCurrent(tier, now) {
		*tiers = append(*tiers, tier)
		return true
	}
	return false
}

func poolAppendGrokWindow(tiers *[]domain.MonitorQuotaTier, label string, window *xai.QuotaWindow, now time.Time) {
	// A limit without a remaining value does not establish consumption.
	if window == nil || window.Limit == nil || *window.Limit <= 0 || window.Remaining == nil || *window.Remaining < 0 {
		return
	}
	var candidates []domain.MonitorQuotaTier
	// Headers do not specify the window duration; do not label it as a day.
	appendQuotaWindowTier(&candidates, "rate_limit", label, window)
	if len(candidates) == 1 && poolTierCurrent(candidates[0], now) {
		*tiers = append(*tiers, candidates[0])
	}
}

func poolTierCurrent(tier domain.MonitorQuotaTier, now time.Time) bool {
	if strings.TrimSpace(tier.ResetAt) == "" {
		return true
	}
	reset, err := parseTime(tier.ResetAt)
	return err == nil && now.Before(reset)
}
