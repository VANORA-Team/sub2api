//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestPoolMonitorSnapshotIdentityExpiryAndThreshold(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	account := &Account{ID: 9, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "old"}}
	observation := &PoolQuotaObservation{Source: "upstream", ObservedAt: now,
		Windows: []domain.MonitorQuotaTier{
			{Window: "5h", UsedPercent: 95, ResetAt: now.Add(time.Hour).Format(time.RFC3339)},
			{Window: "minute", UsedPercent: 99, ResetAt: now.Add(time.Minute).Format(time.RFC3339)},
		}}
	account.Extra = map[string]any{PoolQuotaSnapshotExtraKey: &poolQuotaSnapshot{
		Identity: poolAccountIdentity(account), AttemptedAt: now, Observation: observation,
	}}
	current := poolCurrentObservation(account, now)
	require.NotNil(t, current)
	var health PoolQuotaHealth
	addPoolQuotaHealth(&health, account, current)
	require.Equal(t, PoolQuotaHealth{Known: 1, Low: 1}, health)

	// Rotation must never show a previous account's quota as the new one.
	account.Credentials["access_token"] = "new"
	require.Nil(t, poolCurrentObservation(account, now))
	health = PoolQuotaHealth{}
	addPoolQuotaHealth(&health, account, nil)
	require.Equal(t, int64(1), health.Unknown)
	account.Credentials["access_token"] = "old"
	require.Nil(t, poolCurrentObservation(account, now.Add(3*time.Hour)))
}

func TestPoolAlertsHandlePartialCoverageWithoutFalseRecovery(t *testing.T) {
	now := time.Now()
	availability := &OpsAccountAvailability{Accounts: map[int64]*AccountAvailability{
		1: {AccountID: 1, QuotaMonitored: true, Quota: &PoolQuotaObservation{
			ObservedAt: now,
			Windows:    []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}},
			Balances:   []domain.MonitorBalance{{Currency: "USD", Balance: 2}},
		}},
		2: {AccountID: 2, QuotaMonitored: true},
	}}
	svc := &OpsAlertEvaluatorService{opsService: &OpsService{
		getAccountAvailability: func(context.Context, string, *int64) (*OpsAccountAvailability, error) {
			return availability, nil
		},
	}}
	low := &OpsAlertRule{MetricType: "pool_quota_low_accounts", Operator: ">", Threshold: 0}
	value, ok := svc.computeRuleMetric(context.Background(), low, nil, now, now, "", nil)
	require.True(t, ok)
	require.Equal(t, 1.0, value)
	usd := &OpsAlertRule{MetricType: "pool_min_balance_usd", Operator: "<=", Threshold: 5}
	value, ok = svc.computeRuleMetric(context.Background(), usd, nil, now, now, "", nil)
	require.True(t, ok)
	require.Equal(t, 2.0, value)
	availability.Accounts[1].Quota.Windows[0].UsedPercent = 20
	availability.Accounts[1].Quota.Balances[0].Balance = 20
	_, ok = svc.computeRuleMetric(context.Background(), low, nil, now, now, "", nil)
	require.False(t, ok, "unknown samples cannot resolve a low-quota alert")
	_, ok = svc.computeRuleMetric(context.Background(), usd, nil, now, now, "", nil)
	require.False(t, ok, "unknown samples cannot resolve a low-balance alert")
	availability.Accounts[2].Quota = &PoolQuotaObservation{ObservedAt: now,
		Windows: []domain.MonitorQuotaTier{{Window: "7d", UsedPercent: 10}}}
	availability.Accounts[2].Quota.Partial = true
	_, ok = svc.computeRuleMetric(context.Background(), usd, nil, now, now, "", nil)
	require.False(t, ok, "a partial sample cannot establish balance recovery")
	availability.Accounts[2].Quota.Partial = false
	value, ok = svc.computeRuleMetric(context.Background(), usd, nil, now, now, "", nil)
	require.True(t, ok)
	require.Equal(t, 20.0, value)
}

func TestPoolDeduplicatesSharedOllamaAccount(t *testing.T) {
	now := time.Now()
	old := &PoolQuotaObservation{ObservedAt: now.Add(-time.Hour),
		Windows: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}}}
	current := &PoolQuotaObservation{ObservedAt: now,
		Windows: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 20}}}
	accounts := map[int64]*AccountAvailability{
		1: {AccountID: 1, QuotaMonitored: true, QuotaPoolKey: "shared", Quota: old},
		2: {AccountID: 2, QuotaMonitored: true, QuotaPoolKey: "shared", Quota: current},
		3: {AccountID: 3, QuotaMonitored: true, QuotaPoolKey: "shared"},
	}
	require.Equal(t, PoolQuotaHealth{Known: 1}, poolHealthFromAccounts(accounts))
	value, ok := poolObservationMetric(accounts, "pool_max_usage_percent")
	require.True(t, ok)
	require.Equal(t, 20.0, value)
	require.False(t, poolHasMissingObservation(accounts))
}

func TestPoolKeyDeduplicatesSharedCredentialsButSeparatesShadowQuota(t *testing.T) {
	first := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user", "access_token": "secret-a"}}
	second := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user", "access_token": "secret-b"}}
	require.Equal(t, poolAccountKey(first), poolAccountKey(second))
	require.NotContains(t, poolAccountKey(first), "secret-a")
	second.Credentials["chatgpt_user_id"] = "other-user"
	require.NotEqual(t, poolAccountKey(first), poolAccountKey(second))
	shadow := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ParentAccountID: &first.ID, QuotaDimension: "spark"}
	require.NotEqual(t, poolAccountKey(first), poolAccountKey(shadow, first))
	identity := poolAccountIdentity(shadow, first)
	shadow.QuotaDimension = "global"
	require.NotEqual(t, identity, poolAccountIdentity(shadow, first))
}

func TestPoolBalanceBufferAcceptsEitherCurrency(t *testing.T) {
	accounts := map[int64]*AccountAvailability{
		1: {AccountID: 1, QuotaMonitored: true, Quota: &PoolQuotaObservation{
			Balances: []domain.MonitorBalance{{Currency: "USD", Balance: 0}, {Currency: "CNY", Balance: 90}},
		}},
	}
	value, ok := poolObservationMetric(accounts, "pool_balance_buffer_ratio")
	require.True(t, ok)
	require.Equal(t, 3.0, value, "an empty USD wallet must not hide an adequate CNY balance")
	accounts[1].Quota.Balances[1].Balance = 15
	value, ok = poolObservationMetric(accounts, "pool_balance_buffer_ratio")
	require.True(t, ok)
	require.Equal(t, .5, value)
	accounts[2] = &AccountAvailability{AccountID: 2, QuotaMonitored: true, Quota: &PoolQuotaObservation{
		Balances: []domain.MonitorBalance{{Currency: "USD", Balance: 2}},
	}}
	value, ok = poolObservationMetric(accounts, "pool_balance_buffer_ratio")
	require.True(t, ok)
	require.Equal(t, .4, value, "the least buffered account determines the pool alert")
	require.Equal(t, int64(2), poolHealthFromAccounts(accounts).BalanceLow)
}

func TestPoolLocalSpendingLimitIsDistinctFromUpstreamBalance(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Extra: map[string]any{
			"quota_limit": 100.0, "quota_used": 95.0,
			"quota_daily_limit": 50.0, "quota_daily_used": 49.0,
			"quota_daily_start": time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339),
		}}
	local := poolLocalQuota(account)
	require.Len(t, local, 2)
	require.Equal(t, "local_limit_usd", local[0].Label)
	require.Equal(t, 95.0, local[0].UsedPercent)
	require.Zero(t, local[1].Used, "an expired daily cap starts a new local window")
	accounts := map[int64]*AccountAvailability{
		1: {AccountID: 1, LocalQuota: local},
	}
	require.Equal(t, PoolQuotaHealth{Known: 1, Low: 1}, poolHealthFromAccounts(accounts))
	value, ok := poolObservationMetric(accounts, "pool_max_usage_percent")
	require.True(t, ok)
	require.Equal(t, 95.0, value)
	_, ok = poolObservationMetric(accounts, "pool_min_balance_usd")
	require.False(t, ok, "a local spending cap is not a provider cash balance")
}

type poolAlertEvaluationRepo struct {
	OpsRepository
	rule        *OpsAlertRule
	event       *OpsAlertEvent
	resolved    bool
	emailMarked bool
}

func (r *poolAlertEvaluationRepo) UpdateAlertEventEmailSent(_ context.Context, _ int64, sent bool) error {
	r.emailMarked = sent
	return nil
}

func (r *poolAlertEvaluationRepo) ListAlertRules(context.Context) ([]*OpsAlertRule, error) {
	return []*OpsAlertRule{r.rule}, nil
}
func (r *poolAlertEvaluationRepo) GetLatestSystemMetrics(context.Context, int) (*OpsSystemMetricsSnapshot, error) {
	return nil, nil
}
func (r *poolAlertEvaluationRepo) GetActiveAlertEvent(context.Context, int64) (*OpsAlertEvent, error) {
	return r.event, nil
}
func (r *poolAlertEvaluationRepo) UpdateAlertEventStatus(_ context.Context, _ int64, status string, _ *time.Time) error {
	r.resolved = status == OpsAlertStatusResolved
	return nil
}
func (r *poolAlertEvaluationRepo) UpsertJobHeartbeat(context.Context, *OpsUpsertJobHeartbeatInput) error {
	return nil
}

func TestPoolAlertStaysFiringAfterEvaluatorRestartUntilRecovery(t *testing.T) {
	now := time.Now()
	availability := &OpsAccountAvailability{Accounts: map[int64]*AccountAvailability{
		1: {AccountID: 1, QuotaMonitored: true, Quota: &PoolQuotaObservation{
			ObservedAt: now, Windows: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}},
		}},
	}}
	repo := &poolAlertEvaluationRepo{
		rule: &OpsAlertRule{ID: 1, Name: "pool", Enabled: true, MetricType: "pool_quota_low_accounts",
			Operator: ">", Threshold: 0, SustainedMinutes: 5},
		event: &OpsAlertEvent{ID: 12, RuleID: 1, Status: OpsAlertStatusFiring, EmailSent: true},
	}
	ops := &OpsService{getAccountAvailability: func(context.Context, string, *int64) (*OpsAccountAvailability, error) {
		return availability, nil
	}}
	evaluator := NewOpsAlertEvaluatorService(ops, repo, nil, nil, nil, nil)
	evaluator.evaluateOnce(time.Minute)
	require.False(t, repo.resolved, "an active breach must not resolve while sustained state warms up after restart")
	availability.Accounts[1].Quota.Windows[0].UsedPercent = 20
	evaluator.evaluateOnce(time.Minute)
	require.True(t, repo.resolved)
}

func TestPoolAlertUsesOnlyEnabledVerifiedQuotaRecipientsAsFallback(t *testing.T) {
	settings := newMockSettingRepo()
	settings.data[SettingKeyAccountQuotaNotifyEnabled] = "true"
	settings.data[SettingKeyAccountQuotaNotifyEmails] = `[
		{"email":"admin@example.com","verified":true},
		{"email":"unverified@example.com","verified":false},
		{"email":"disabled@example.com","verified":true,"disabled":true}
	]`
	evaluator := &OpsAlertEvaluatorService{opsService: &OpsService{settingRepo: settings}}
	cfg := defaultOpsEmailNotificationConfig()
	rule := &OpsAlertRule{MetricType: "pool_balance_buffer_ratio"}
	require.Equal(t, []string{"admin@example.com"}, evaluator.alertRecipients(context.Background(), cfg, rule))
	require.Empty(t, evaluator.alertRecipients(context.Background(), cfg, &OpsAlertRule{MetricType: "error_rate"}))
	cfg.Alert.Recipients = []string{"ops@example.com"}
	require.Equal(t, []string{"ops@example.com"}, evaluator.alertRecipients(context.Background(), cfg, rule))
	cfg.Alert.Recipients = nil
	settings.data[SettingKeyAccountQuotaNotifyEnabled] = "false"
	require.Empty(t, evaluator.alertRecipients(context.Background(), cfg, rule))
}

func TestPoolAlertEmailRetryIsBoundedAndResetsOnResolution(t *testing.T) {
	now := time.Now()
	evaluator := &OpsAlertEvaluatorService{}
	require.True(t, evaluator.claimEmailRetry(4, now))
	require.False(t, evaluator.claimEmailRetry(4, now.Add(time.Minute)))
	require.True(t, evaluator.claimEmailRetry(4, now.Add(5*time.Minute)))
	evaluator.forgetEmailRetry(4)
	require.True(t, evaluator.claimEmailRetry(4, now.Add(5*time.Minute)))
}

func TestPoolAlertIdentifiesAffectedAccountsAndEscapesEmail(t *testing.T) {
	rule := &OpsAlertRule{Name: "pool", MetricType: "pool_quota_low_accounts", Operator: ">", Threshold: 0}
	accounts := map[int64]*AccountAvailability{
		4: {AccountID: 4, AccountName: "<script>bad</script>", QuotaMonitored: true,
			Quota: &PoolQuotaObservation{Windows: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}}}},
		5: {AccountID: 5, AccountName: "healthy", QuotaMonitored: true,
			Quota: &PoolQuotaObservation{Windows: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 10}}}},
	}
	names := poolAlertAccounts(accounts, rule)
	require.Contains(t, names, "<script>bad</script> (#4)")
	require.NotContains(t, names, "healthy")
	body := buildOpsAlertEmailBody(rule, &OpsAlertEvent{Description: names, FiredAt: time.Now()})
	require.NotContains(t, body, "<script>bad</script>")
	require.Contains(t, body, "&lt;script&gt;")
}

func TestPoolAlertDeliversToVerifiedQuotaRecipientWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	settings := newNotificationEmailMemorySettingRepo()
	smtp := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, settings.SetMultiple(ctx, smtp.settings()))
	require.NoError(t, settings.Set(ctx, SettingKeyAccountQuotaNotifyEnabled, "true"))
	require.NoError(t, settings.Set(ctx, SettingKeyAccountQuotaNotifyEmails,
		`[{"email":"admin@example.com","verified":true},{"email":"other@example.com","verified":false}]`))
	email := NewEmailService(settings, nil)
	NewNotificationEmailService(settings, email)
	repo := &poolAlertEvaluationRepo{}
	evaluator := NewOpsAlertEvaluatorService(&OpsService{settingRepo: settings}, repo, email, nil, nil, nil)
	rule := &OpsAlertRule{Name: "pool low", MetricType: "pool_quota_low_accounts",
		Operator: ">", Threshold: 0, Severity: "P1", NotifyEmail: true}
	event := &OpsAlertEvent{ID: 404, RuleID: 1, Status: OpsAlertStatusFiring,
		Description: "accounts: Kimi (#4)", FiredAt: time.Now()}

	require.True(t, evaluator.maybeSendAlertEmail(ctx, defaultOpsAlertRuntimeSettings(), rule, event))
	require.True(t, repo.emailMarked)
	require.Equal(t, int64(1), smtp.messageCount())
	require.Contains(t, smtp.lastMessageBody(t), "Kimi (#4)")
	// A retry after an uncertain event-state write is deduplicated per recipient.
	require.True(t, evaluator.maybeSendAlertEmail(ctx, defaultOpsAlertRuntimeSettings(), rule, event))
	require.Equal(t, int64(1), smtp.messageCount())
}

func TestPoolMonitorSkipsUnsupportedAndShortWindows(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true}
	require.False(t, poolAccountMonitored(account))
	account.Platform = PlatformZhipu
	account.Credentials = map[string]any{"account_mode": "payg"}
	require.False(t, poolAccountMonitored(account))
	account.Platform = PlatformDeepseek
	require.True(t, poolAccountMonitored(account))
	account.Status = StatusError
	account.Schedulable = false
	require.True(t, poolAccountMonitored(account), "quota monitoring covers accounts even when routing is paused")
	account.Platform = PlatformKimi
	account.Credentials["base_url"] = "https://third-party.example/v1"
	require.False(t, poolAccountMonitored(account))
	known, low := poolObservationLow(&PoolQuotaObservation{Windows: []domain.MonitorQuotaTier{
		{Window: "minute", UsedPercent: 100},
	}})
	require.False(t, known)
	require.False(t, low)
	zero := false
	known, low = poolObservationLow(&PoolQuotaObservation{BalanceLow: &zero})
	require.True(t, known)
	require.False(t, low)
}

func TestPoolExpiredMinuteWindowDoesNotInvalidateDailyQuota(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 9, Platform: PlatformGemini, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "key"}}
	account.Extra = map[string]any{PoolQuotaSnapshotExtraKey: &poolQuotaSnapshot{
		Identity: poolAccountIdentity(account), AttemptedAt: now, Observation: &PoolQuotaObservation{
			Source: "local_estimate", ObservedAt: now,
			Windows: []domain.MonitorQuotaTier{
				{Window: "daily", UsedPercent: 50, ResetAt: now.Add(time.Hour).Format(time.RFC3339)},
				{Window: "minute", UsedPercent: 100, ResetAt: now.Add(-time.Second).Format(time.RFC3339)},
			},
		},
	}}
	observation := poolCurrentObservation(account, now)
	require.NotNil(t, observation)
	require.False(t, observation.Partial)
	require.Len(t, observation.Windows, 1)
	require.Equal(t, "daily", observation.Windows[0].Window)
}
