//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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
