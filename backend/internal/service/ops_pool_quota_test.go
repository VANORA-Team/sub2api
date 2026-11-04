//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexPoolQuotaCoverageAndReset(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	account := Account{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{
			"codex_usage_updated_at": now.Add(-time.Minute).Format(time.RFC3339),
			"codex_5h_used_percent":  75.0,
			"codex_5h_reset_at":      now.Add(time.Hour).Format(time.RFC3339),
			"codex_7d_used_percent":  20.0,
			"codex_7d_reset_at":      now.Add(24 * time.Hour).Format(time.RFC3339),
		},
	}
	var summary PoolQuotaSummary
	addCodexPoolQuota(&summary, &account, now)
	require.Equal(t, int64(1), summary.FiveHour.Known)
	require.InDelta(t, .75, summary.FiveHour.Used, 0.001)
	require.InDelta(t, .25, summary.FiveHour.Remaining, 0.001)
	require.InDelta(t, .8, summary.SevenDay.Remaining, 0.001)

	account.Extra["codex_5h_reset_at"] = now.Format(time.RFC3339)
	account.Extra["codex_7d_used_percent"] = "invalid"
	addCodexPoolQuota(&summary, &account, now)
	require.Equal(t, int64(1), summary.FiveHour.Unknown)
	require.Equal(t, int64(1), summary.SevenDay.Unknown)

	account.Extra["codex_usage_updated_at"] = now.Add(-3 * time.Hour).Format(time.RFC3339)
	addCodexPoolQuota(&summary, &account, now)
	require.Equal(t, int64(2), summary.FiveHour.Unknown)
	parent := int64(1)
	account.ParentAccountID = &parent
	addCodexPoolQuota(&summary, &account, now)
	require.Equal(t, int64(2), summary.FiveHour.Unknown)
}

func TestPoolQuotaAlertRequiresCompleteCoverage(t *testing.T) {
	groupID := int64(42)
	availability := &OpsAccountAvailability{Group: &GroupAvailability{
		CodexQuota: PoolQuotaSummary{FiveHour: PoolQuotaWindow{Known: 2, Remaining: 0.8}},
	}}
	svc := &OpsAlertEvaluatorService{opsService: &OpsService{
		getAccountAvailability: func(context.Context, string, *int64) (*OpsAccountAvailability, error) {
			return availability, nil
		},
	}}
	now := time.Now()
	rule := &OpsAlertRule{MetricType: "group_codex_5h_remaining"}
	value, ok := svc.computeRuleMetric(context.Background(), rule, nil, now, now, "", &groupID)
	require.True(t, ok)
	require.InDelta(t, 0.8, value, 0.001)
	availability.Group.CodexQuota.FiveHour.Unknown = 1
	_, ok = svc.computeRuleMetric(context.Background(), rule, nil, now, now, "", &groupID)
	require.False(t, ok)
}
