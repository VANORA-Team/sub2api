//go:build unit

package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

func TestPoolUsageObservationOpenAINeverInventsQuota(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	// AccountUsageService may create this empty bar solely for local stats.
	usage := &UsageInfo{UpdatedAt: &now, FiveHour: &UsageProgress{Utilization: 0}}
	require.Nil(t, poolUsageObservation(account, usage, now))
	account.Extra["codex_usage_updated_at"] = now.Format(time.RFC3339)
	account.Extra["codex_5h_used_percent"] = "invalid"
	account.Extra["codex_5h_reset_at"] = now.Add(time.Hour).Format(time.RFC3339)
	require.Nil(t, poolUsageObservation(account, usage, now))
	account.Extra["codex_5h_used_percent"] = 76.0
	observation := poolUsageObservation(account, usage, now)
	require.NotNil(t, observation)
	require.Len(t, observation.Windows, 1)
	require.Equal(t, 76.0, observation.Windows[0].UsedPercent)
	account.Extra["codex_5h_reset_at"] = now.Format(time.RFC3339)
	require.Nil(t, poolUsageObservation(account, usage, now))
}

func TestPoolOpenAIObservationPreservesAdditionalWindows(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	quota := &OpenAIQuotaUsage{FetchedAt: now.Unix(),
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{
			UsedPercent: 80, LimitWindowSeconds: 5 * 3600, ResetAt: now.Add(time.Hour).Unix(),
		}},
		AdditionalRateLimits: []OpenAIAdditionalRateLimit{{MeteredFeature: "codex_bengalfox",
			RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{
				UsedPercent: 95, LimitWindowSeconds: 24 * 3600, ResetAfterSeconds: 3600,
			}}}},
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	observation := poolOpenAIObservation(account, quota, now)
	require.NotNil(t, observation)
	require.Len(t, observation.Windows, 2)
	require.Equal(t, "5h", observation.Windows[0].Window)
	require.Equal(t, "1d", observation.Windows[1].Window)
	require.Equal(t, "codex_bengalfox", observation.Windows[1].Label)
	parentID := int64(10)
	account.ParentAccountID = &parentID
	observation = poolOpenAIObservation(account, quota, now)
	require.Len(t, observation.Windows, 1)
	require.Empty(t, observation.Windows[0].Label)
	require.Equal(t, "1d", observation.Windows[0].Window)
}

func TestPoolOpenAIObservationRejectsMissingUsedPercent(t *testing.T) {
	now := time.Now()
	var window OpenAIRateLimitWindow
	require.NoError(t, json.Unmarshal([]byte(`{"limit_window_seconds":18000,"reset_after_seconds":3600}`), &window))
	quota := &OpenAIQuotaUsage{FetchedAt: now.Unix(), RateLimit: &OpenAIRateLimit{PrimaryWindow: &window}}
	require.Nil(t, poolOpenAIObservation(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, quota, now))
}

func TestPoolUsageObservationProviderWindowsAndFreshness(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	tests := []struct {
		name    string
		account *Account
		usage   *UsageInfo
		source  string
		window  string
	}{
		{"anthropic", &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, &UsageInfo{
			UpdatedAt: &now, FiveHour: &UsageProgress{Utilization: 85, ResetsAt: &reset},
		}, "upstream", "5h"},
		{"gemini", &Account{Platform: PlatformGemini}, &UsageInfo{
			UpdatedAt: &now, GeminiSharedDaily: &UsageProgress{Utilization: 40, ResetsAt: &reset},
		}, "local_estimate", "daily"},
		{"antigravity", &Account{Platform: PlatformAntigravity}, &UsageInfo{
			UpdatedAt: &now, AntigravityQuota: map[string]*AntigravityModelQuota{
				"model-a": {Utilization: 70, ResetTime: reset.Format(time.RFC3339)},
			},
		}, "upstream", "total"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := poolUsageObservation(tc.account, tc.usage, now)
			require.NotNil(t, got)
			require.Equal(t, tc.source, got.Source)
			require.Equal(t, tc.window, got.Windows[0].Window)
			stale := now.Add(-3 * time.Hour)
			tc.usage.UpdatedAt = &stale
			require.Nil(t, poolUsageObservation(tc.account, tc.usage, now))
		})
	}
	setup := &Account{Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Extra: map[string]any{
		"passive_usage_sampled_at": now.Format(time.RFC3339),
	}}
	require.Nil(t, poolUsageObservation(setup, &UsageInfo{FiveHour: &UsageProgress{}}, now))
	oauth := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	require.Nil(t, poolUsageObservation(oauth, &UsageInfo{UpdatedAt: &now, FiveHour: &UsageProgress{}}, now),
		"a synthesized zero without an observed reset is unknown")
}

func TestPoolGrokKeepsIndependentFreshnessAndMissingRemainingUnknown(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	limit, remaining := int64(100), int64(10)
	prepaid := 2.0
	stale := now.Add(-3 * time.Hour).Format(time.RFC3339)
	snapshot := &xai.QuotaSnapshot{
		LastHeadersSeenAt: now.Format(time.RFC3339),
		Requests:          &xai.QuotaWindow{Limit: &limit},
		Tokens:            &xai.QuotaWindow{Limit: &limit, Remaining: &remaining},
	}
	billing := &xai.BillingSummary{WeeklyUpdatedAt: stale, PrepaidBalance: &prepaid}
	account := &Account{Platform: PlatformGrok, Extra: map[string]any{
		grokQuotaSnapshotExtraKey: snapshot,
		grokBillingExtraKey:       billing,
	}}
	usage := &UsageInfo{UpdatedAt: &now, SevenDay: &UsageProgress{Utilization: 80}}
	got := poolUsageObservation(account, usage, now)
	require.NotNil(t, got)
	require.Len(t, got.Windows, 1)
	require.Equal(t, "tokens", got.Windows[0].Label)
	require.Empty(t, got.Balances)
	require.True(t, got.Partial, "stale billing must remain visible as incomplete coverage")
	billing.WeeklyUpdatedAt = now.Format(time.RFC3339)
	got = poolUsageObservation(account, usage, now)
	require.Len(t, got.Windows, 2)
	require.Equal(t, 2.0, got.Balances[0].Balance)
}

func TestPoolCNAndOllamaObservations(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	quota := &CNProviderQuotaProbeResult{Success: true, FetchedAt: now.Unix(), Tiers: []CNQuotaTier{
		{Window: "5h", UsedPercent: 91, ResetAt: now.Add(time.Hour).Format(time.RFC3339)},
		{Window: "weekly", UsedPercent: 10, ResetAt: now.Add(-time.Second).Format(time.RFC3339)},
	}}
	require.Len(t, poolCNQuotaObservation(quota, .5, now).Windows, 1)
	require.True(t, poolCNQuotaObservation(quota, .5, now).Partial)
	quota.Balance = &CNProviderBalanceResult{Success: true, Available: true, FetchedAt: now.Unix(), Currency: "USD", Balance: 2}
	combined := poolCNQuotaObservation(quota, 3, now)
	require.Len(t, combined.Windows, 1)
	require.Equal(t, 2.0, combined.Balances[0].Balance)
	require.True(t, *combined.BalanceLow, "the configured balance threshold also applies to quota probes")
	quota.Tiers = nil
	require.NotNil(t, poolCNQuotaObservation(quota, .5, now), "balance-only plans remain observable")
	quota.Balance.FetchedAt = now.Add(-poolQuotaMaxAge - time.Second).Unix()
	require.Nil(t, poolCNQuotaObservation(quota, .5, now), "expired balances must not appear as current")
	quota.Tiers = []CNQuotaTier{{Window: "5h", UsedPercent: 1}}
	combined = poolCNQuotaObservation(quota, .5, now)
	require.Empty(t, combined.Balances)
	require.True(t, combined.Partial)
	balance := &CNProviderBalanceResult{Success: true, Available: true, FetchedAt: now.Unix(), Balances: []CNProviderBalanceEntry{
		{Currency: "CNY", Balance: .1}, {Currency: "USD", Balance: 2},
	}}
	got := poolCNBalanceObservation(balance, .5, now)
	require.Len(t, got.Balances, 2)
	require.False(t, *got.BalanceLow)
	balance.Balances[1].Currency = ""
	require.Nil(t, poolCNBalanceObservation(balance, .5, now))
	ollama := &OllamaCloudUsageSnapshot{Status: OllamaCloudUsageStatusOK, FetchedAt: &now,
		Data: &OllamaCloudUsageData{Balance: "$1.00", FiveHour: &OllamaCloudUsageWindow{UsedPercent: 55}},
	}
	ollamaObservation := poolOllamaObservation(ollama, now)
	require.Equal(t, "$1.00", ollamaObservation.RawBalance)
	require.Equal(t, 1.0, ollamaObservation.Balances[0].Balance)
	ollama.Data.Balance = "5 credits"
	require.Empty(t, poolOllamaObservation(ollama, now).Balances, "an ambiguous unit is not a USD balance")
	ollama.Status = OllamaCloudUsageStatusUnauthorized
	require.Nil(t, poolOllamaObservation(ollama, now))
}
