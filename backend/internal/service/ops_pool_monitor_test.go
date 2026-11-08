//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type poolMonitorAccountStub struct {
	AccountRepository
	current *Account
	writes  int
}

func (r *poolMonitorAccountStub) ListWithFilters(_ context.Context, _ pagination.PaginationParams,
	_, _, _, _ string, _ int64, _ string,
) ([]Account, *pagination.PaginationResult, error) {
	account, err := r.GetByID(context.Background(), r.current.ID)
	if err != nil {
		return nil, nil, err
	}
	return []Account{*account}, &pagination.PaginationResult{Total: 1}, nil
}

func (r *poolMonitorAccountStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.current == nil || r.current.ID != id {
		return nil, ErrAccountNotFound
	}
	copy := *r.current
	if r.current.Credentials != nil {
		copy.Credentials = make(map[string]any, len(r.current.Credentials))
		for key, value := range r.current.Credentials {
			copy.Credentials[key] = value
		}
	}
	copy.Extra = make(map[string]any, len(r.current.Extra))
	for key, value := range r.current.Extra {
		copy.Extra[key] = value
	}
	return &copy, nil
}

func (r *poolMonitorAccountStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.writes++
	if r.current.Extra == nil {
		r.current.Extra = make(map[string]any)
	}
	for key, value := range updates {
		r.current.Extra[key] = value
	}
	return nil
}

type poolMonitorCNStub struct {
	onQuery func()
}

type poolMonitorOpenAIStub struct {
	calls int
}

func (s *poolMonitorOpenAIStub) QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error) {
	s.calls++
	return nil, nil
}

func (s *poolMonitorCNStub) QueryUsageForMonitoring(_ context.Context, _ *Account) (*CNProviderQuotaProbeResult, error) {
	if s.onQuery != nil {
		s.onQuery()
	}
	now := time.Now()
	return &CNProviderQuotaProbeResult{Success: true, FetchedAt: now.Unix(), Tiers: []CNQuotaTier{
		{Window: "5h", UsedPercent: 92, ResetAt: now.Add(time.Hour).Format(time.RFC3339)},
	}}, nil
}

func TestPoolCollectorPersistsQuotaAndInvalidatesChangedIdentity(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding", "api_key": "original"},
	}
	repo := &poolMonitorAccountStub{current: account}
	monitor := &PoolQuotaMonitor{accounts: repo, cnQuota: &poolMonitorCNStub{}, instance: "test"}
	monitor.collectOne(context.Background(), account.ID)
	require.Equal(t, 1, repo.writes)
	observation := poolCurrentObservation(account, time.Now())
	require.NotNil(t, observation)
	require.Equal(t, 92.0, observation.Windows[0].UsedPercent)
	// A successful fresh result is not collected on every cycle.
	monitor.collectOne(context.Background(), account.ID)
	require.Equal(t, 1, repo.writes)

	// Credential changes during an in-flight query do not attach old data to
	// the replacement identity, even though the atomic extra merge succeeds.
	account.Extra = nil
	monitor.cnQuota = &poolMonitorCNStub{onQuery: func() { account.Credentials["api_key"] = "replacement" }}
	monitor.collectOne(context.Background(), account.ID)
	require.Equal(t, 2, repo.writes)
	require.Nil(t, poolCurrentObservation(account, time.Now()))
	require.True(t, poolMonitorDue(account, time.Now()))
}

func TestPoolCollectorFailureIsUnknownAndRetriedWithBackoff(t *testing.T) {
	account := &Account{ID: 2, Platform: PlatformGrok, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	repo := &poolMonitorAccountStub{current: account}
	monitor := &PoolQuotaMonitor{accounts: repo, instance: "test"}
	monitor.collectOne(context.Background(), account.ID)
	require.Equal(t, 1, repo.writes)
	require.Nil(t, poolCurrentObservation(account, time.Now()))
	require.False(t, poolMonitorDue(account, time.Now()))
	require.True(t, poolMonitorDue(account, time.Now().Add(poolMonitorFailureRetry+time.Second)))
}

func TestPoolCollectorUsesPerAccountCrossInstanceLock(t *testing.T) {
	account := &Account{ID: 3, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding", "api_key": "key"}}
	repo := &poolMonitorAccountStub{current: account}
	locks := &fakeLeaderLockCache{}
	started := make(chan struct{})
	unblock := make(chan struct{})
	finished := make(chan struct{})
	source := &poolMonitorCNStub{onQuery: func() {
		close(started)
		<-unblock
	}}
	first := &PoolQuotaMonitor{accounts: repo, cnQuota: source, locks: locks, instance: "first"}
	second := &PoolQuotaMonitor{accounts: repo, cnQuota: source, locks: locks, instance: "second"}
	go func() {
		defer close(finished)
		first.collectOne(context.Background(), account.ID)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first collector did not start")
	}
	second.collectOne(context.Background(), account.ID)
	close(unblock)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("first collector did not finish")
	}
	require.Equal(t, 1, repo.writes)
}

func TestPoolCollectorSkipsWhenConfiguredLockBackendFails(t *testing.T) {
	account := &Account{ID: 5, Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"account_mode": "coding", "api_key": "key"}}
	repo := &poolMonitorAccountStub{current: account}
	monitor := &PoolQuotaMonitor{accounts: repo, cnQuota: &poolMonitorCNStub{},
		locks: &fakeLeaderLockCache{acquireErr: errors.New("redis unavailable")}, instance: "test"}
	monitor.collectOne(context.Background(), account.ID)
	require.Zero(t, repo.writes)
}

func TestPoolCollectorNeverProbesSyntheticAccounts(t *testing.T) {
	account := &Account{ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"synthetic_ui_test": true}}
	repo := &poolMonitorAccountStub{current: account}
	source := &poolMonitorOpenAIStub{}
	monitor := &PoolQuotaMonitor{accounts: repo, openai: source, instance: "test"}
	monitor.collectOne(context.Background(), account.ID)
	require.Zero(t, source.calls)
	require.Nil(t, poolCurrentObservation(account, time.Now()))
}

func TestPoolCollectionFlowsIntoAvailabilityAndAlert(t *testing.T) {
	account := &Account{ID: 4, Name: "Kimi", Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"account_mode": "coding", "api_key": "sensitive-pool-key-unique"},
		Groups:      []*Group{{ID: 7, Name: "production", Platform: PlatformKimi}},
	}
	repo := &poolMonitorAccountStub{current: account}
	monitor := &PoolQuotaMonitor{accounts: repo, cnQuota: &poolMonitorCNStub{}, instance: "test"}
	monitor.collectOne(context.Background(), account.ID)
	ops := &OpsService{accountRepo: repo}
	groupID := int64(7)
	availability, err := ops.GetAccountAvailability(context.Background(), "", &groupID)
	require.NoError(t, err)
	require.NotNil(t, availability.Group)
	require.Equal(t, int64(1), availability.Group.PoolQuota.Known)
	require.Equal(t, int64(1), availability.Group.PoolQuota.Low)
	require.Equal(t, 92.0, availability.Accounts[4].Quota.Windows[0].UsedPercent)
	encoded, err := json.Marshal(availability.Accounts[4])
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "sensitive-pool-key-unique")
	require.NotContains(t, string(encoded), poolAccountKey(account))
	require.NotContains(t, string(encoded), poolAccountIdentity(account))
	evaluator := &OpsAlertEvaluatorService{opsService: ops}
	now := time.Now()
	metric, ok := evaluator.computeRuleMetric(context.Background(), &OpsAlertRule{
		MetricType: "pool_quota_low_accounts", Operator: ">", Threshold: 0,
	}, nil, now, now, "", &groupID)
	require.True(t, ok)
	require.Equal(t, 1.0, metric)
}

func TestPoolCollectorCommandCodeQuotaAndBalance(t *testing.T) {
	t.Run("windows and balance reach account availability", func(t *testing.T) {
		now := time.Now().UTC()
		account := commandCodeUsageAccount()
		account.Schedulable = true
		account.Groups = []*Group{{ID: 7, Name: "command code", Platform: PlatformCommandCode}}
		upstream := newCommandCodeAlphaUpstream(`{"org":null}`)
		upstream.responses["/alpha/billing/credits"] = commandCodeAlphaResponse{
			status: http.StatusOK,
			body: fmt.Sprintf(`{"credits":{"monthlyCredits":68.09,"purchasedCredits":0,"freeCredits":0},
				"windowLimits":{"limited":true,
				"fiveHour":{"used":1,"cap":100,"resetAt":%d},
				"weekly":{"used":5,"cap":100,"resetAt":%d}}}`,
				now.Add(5*time.Hour).UnixMilli(), now.Add(7*24*time.Hour).UnixMilli()),
		}
		upstream.responses["/alpha/billing/subscriptions"] = commandCodeAlphaResponse{
			status: http.StatusOK,
			body: fmt.Sprintf(`{"data":{"planId":"individual-goat","currentPeriodStart":%q,"currentPeriodEnd":%q}}`,
				now.Add(-24*time.Hour).Format(time.RFC3339), now.Add(29*24*time.Hour).Format(time.RFC3339)),
		}
		upstream.responses["/alpha/usage/summary"] = commandCodeAlphaResponse{
			status: http.StatusOK, body: `{"totalMonthlyCredits":2.11}`,
		}
		repo := &poolMonitorAccountStub{current: account}
		monitor := &PoolQuotaMonitor{
			accounts: repo, instance: "test",
			cnQuota: NewCNProviderQuotaService(repo, nil, upstream, &config.Config{}),
		}
		monitor.collectOne(context.Background(), account.ID)
		require.Equal(t, 1, repo.writes, "monitoring persists only its own snapshot")
		require.Len(t, account.Extra, 1)
		require.Contains(t, account.Extra, PoolQuotaSnapshotExtraKey)
		observation := poolCurrentObservation(account, time.Now())
		require.NotNil(t, observation)
		require.Equal(t, "cn_quota", observation.Source)
		require.False(t, observation.Partial)
		require.Len(t, observation.Windows, 3)
		require.Equal(t, []string{"5h", "weekly", "monthly"}, []string{
			observation.Windows[0].Window, observation.Windows[1].Window, observation.Windows[2].Window,
		})
		require.Equal(t, 1.0, observation.Windows[0].UsedPercent)
		require.Equal(t, 5.0, observation.Windows[1].UsedPercent)
		require.InDelta(t, 3.01, observation.Windows[2].UsedPercent, .01)
		require.Len(t, observation.Balances, 1)
		require.Equal(t, "USD", observation.Balances[0].Currency)
		require.Equal(t, 68.09, observation.Balances[0].Balance)
		require.False(t, *observation.BalanceLow)
		require.Len(t, upstream.requests, 4, "windows and balance share one provider probe")

		ops := &OpsService{accountRepo: repo}
		groupID := int64(7)
		availability, err := ops.GetAccountAvailability(context.Background(), "", &groupID)
		require.NoError(t, err)
		require.True(t, availability.Accounts[account.ID].QuotaMonitored)
		require.False(t, availability.Accounts[account.ID].QuotaUnsupported)
		require.NotNil(t, availability.Accounts[account.ID].Quota)
		require.Equal(t, int64(1), availability.Group.PoolQuota.Known)
		require.Zero(t, availability.Group.PoolQuota.Unsupported)
		encoded, err := json.Marshal(availability.Accounts[account.ID])
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "user_test_key")
	})

	t.Run("probe failures remain unknown", func(t *testing.T) {
		account := commandCodeUsageAccount()
		upstream := newCommandCodeAlphaUpstream(`{}`)
		upstream.responses["/alpha/whoami"] = commandCodeAlphaResponse{
			status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`,
		}
		repo := &poolMonitorAccountStub{current: account}
		monitor := &PoolQuotaMonitor{
			accounts: repo, instance: "test",
			cnQuota: NewCNProviderQuotaService(repo, nil, upstream, &config.Config{}),
		}
		require.True(t, poolAccountMonitored(account))
		monitor.collectOne(context.Background(), account.ID)
		require.Equal(t, 1, repo.writes)
		require.Nil(t, poolCurrentObservation(account, time.Now()))
		require.False(t, poolMonitorDue(account, time.Now()))
	})

	for _, test := range []struct{ name, accountType, baseURL string }{
		{"custom upstream", AccountTypeAPIKey, "https://relay.example/v1"},
		{"insecure upstream", AccountTypeAPIKey, "http://api.commandcode.ai"},
		{"unsupported credential type", AccountTypeOAuth, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := commandCodeUsageAccount()
			account.Type = test.accountType
			account.Credentials["base_url"] = test.baseURL
			upstream := newCommandCodeAlphaUpstream(`{}`)
			repo := &poolMonitorAccountStub{current: account}
			monitor := &PoolQuotaMonitor{
				accounts: repo, instance: "test",
				cnQuota: NewCNProviderQuotaService(repo, nil, upstream, &config.Config{}),
			}
			require.False(t, poolAccountMonitored(account))
			monitor.collectOne(context.Background(), account.ID)
			require.Empty(t, upstream.requests)
			require.Zero(t, repo.writes)
		})
	}
}
