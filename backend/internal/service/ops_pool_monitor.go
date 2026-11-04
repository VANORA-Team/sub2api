package service

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
)

const (
	poolMonitorCycleInterval = time.Minute
	poolMonitorSuccessRetry  = 5 * time.Minute
	poolMonitorFailureRetry  = 2 * time.Minute
	poolMonitorCycleTimeout  = 90 * time.Second
	poolMonitorProbeTimeout  = 45 * time.Second
	poolMonitorLockTTL       = 2 * time.Minute
	poolMonitorMaxPerCycle   = 40
	poolMonitorConcurrency   = 4
)

type poolUsageSource interface {
	GetUsageForMonitoring(context.Context, *Account) (*UsageInfo, error)
}

type poolGrokSource interface {
	ProbeBillingForMonitoring(context.Context, int64) (*GrokQuotaProbeResult, error)
}

type poolOpenAISource interface {
	QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error)
}

type poolCNQuotaSource interface {
	QueryUsageForMonitoring(context.Context, *Account) (*CNProviderQuotaProbeResult, error)
}

type poolCNBalanceSource interface {
	QueryBalanceForMonitoring(context.Context, *Account) (*CNProviderBalanceResult, error)
}

type poolOllamaSource interface {
	GetState(context.Context, int64) (*OllamaCloudUsageState, error)
}

// PoolQuotaMonitor collects provider-specific observations in bounded batches.
// It does not modify scheduling or treat an unsuccessful query as zero quota.
type PoolQuotaMonitor struct {
	ops       *OpsService
	accounts  AccountRepository
	usage     poolUsageSource
	openai    poolOpenAISource
	grok      poolGrokSource
	cnQuota   poolCNQuotaSource
	cnBalance poolCNBalanceSource
	ollama    poolOllamaSource
	cfg       *config.Config
	locks     LeaderLockCache
	db        *sql.DB
	instance  string

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func NewPoolQuotaMonitor(ops *OpsService, accounts AccountRepository, usage *AccountUsageService,
	openai *OpenAIQuotaService, grok *GrokQuotaService, cnQuota *CNProviderQuotaService, cnBalance *CNProviderBalanceService,
	ollama *OllamaCloudUsageService, cfg *config.Config, locks LeaderLockCache, db *sql.DB,
) *PoolQuotaMonitor {
	monitor := &PoolQuotaMonitor{
		ops: ops, accounts: accounts, cfg: cfg, locks: locks, db: db,
		instance: uuid.NewString(),
	}
	if usage != nil {
		monitor.usage = usage
	}
	if openai != nil {
		monitor.openai = openai
	}
	if grok != nil {
		monitor.grok = grok
	}
	if cnQuota != nil {
		monitor.cnQuota = cnQuota
	}
	if cnBalance != nil {
		monitor.cnBalance = cnBalance
	}
	if ollama != nil {
		monitor.ollama = ollama
	}
	return monitor
}

func (m *PoolQuotaMonitor) Start() {
	if m == nil {
		return
	}
	m.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.done = make(chan struct{})
		go m.run(ctx)
	})
}

func (m *PoolQuotaMonitor) Stop() {
	if m == nil || m.cancel == nil {
		return
	}
	m.cancel()
	<-m.done
}

func (m *PoolQuotaMonitor) run(ctx context.Context) {
	defer close(m.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.runCycle(ctx)
			timer.Reset(poolMonitorCycleInterval)
		}
	}
}

func (m *PoolQuotaMonitor) runCycle(parentCtx context.Context) {
	if m == nil || m.ops == nil || m.accounts == nil || !m.ops.IsMonitoringEnabled(parentCtx) {
		return
	}
	ctx, cancel := context.WithTimeout(parentCtx, poolMonitorCycleTimeout)
	defer cancel()
	accounts, err := m.ops.listAllAccountsForOps(ctx, "", nil)
	if err != nil {
		slog.Warn("pool_quota_monitor_list_failed", "error", err)
		return
	}
	now := time.Now()
	due := make([]Account, 0, len(accounts))
	for i := range accounts {
		if poolAccountMonitored(&accounts[i]) && poolMonitorDue(&accounts[i], now) {
			due = append(due, accounts[i])
		}
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := poolSnapshotFromAccount(&due[i]), poolSnapshotFromAccount(&due[j])
		if a == nil {
			return b != nil || due[i].ID < due[j].ID
		}
		if b == nil {
			return false
		}
		if a.AttemptedAt.Equal(b.AttemptedAt) {
			return due[i].ID < due[j].ID
		}
		return a.AttemptedAt.Before(b.AttemptedAt)
	})
	if len(due) > poolMonitorMaxPerCycle {
		due = due[:poolMonitorMaxPerCycle]
	}
	jobs := make(chan int64)
	var wg sync.WaitGroup
	for i := 0; i < poolMonitorConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				m.collectOne(ctx, id)
			}
		}()
	}
sendLoop:
	for i := range due {
		select {
		case jobs <- due[i].ID:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(jobs)
	wg.Wait()
}

func poolMonitorDue(account *Account, now time.Time, parents ...*Account) bool {
	snapshot := poolSnapshotFromAccount(account)
	if snapshot == nil || snapshot.AttemptedAt.IsZero() || snapshot.AttemptedAt.After(now.Add(time.Minute)) {
		return true
	}
	identity := poolAccountIdentity(account, parents...)
	if identity != "" && snapshot.Identity != identity {
		return true
	}
	delay := poolMonitorSuccessRetry
	if snapshot.Observation == nil || snapshot.Observation.Partial {
		delay = poolMonitorFailureRetry
	}
	return !now.Before(snapshot.AttemptedAt.Add(delay))
}

func (m *PoolQuotaMonitor) collectOne(cycleCtx context.Context, accountID int64) {
	ctx, cancel := context.WithTimeout(cycleCtx, poolMonitorProbeTimeout)
	defer cancel()
	key := fmt.Sprintf("ops:pool-quota:account:%d", accountID)
	release, acquired := m.acquireAccountLock(ctx, key)
	if !acquired {
		return
	}
	defer release()
	account, err := m.accounts.GetByID(ctx, accountID)
	if err != nil || !poolAccountMonitored(account) {
		return
	}
	var parent *Account
	if account.ParentAccountID != nil {
		parent, err = m.accounts.GetByID(ctx, *account.ParentAccountID)
		if err != nil {
			m.saveAttempt(account.ID, &poolQuotaSnapshot{AttemptedAt: time.Now().UTC()})
			return
		}
	}
	if !poolMonitorDue(account, time.Now(), parent) {
		return
	}
	identity := poolAccountIdentity(account, parent)
	if identity == "" {
		return
	}
	observation := m.observe(ctx, account)
	snapshot := &poolQuotaSnapshot{Identity: identity, AttemptedAt: time.Now().UTC(), Observation: observation}
	m.saveAttempt(account.ID, snapshot)
}

func (m *PoolQuotaMonitor) acquireAccountLock(ctx context.Context, key string) (func(), bool) {
	if m.locks == nil {
		return tryAcquireSingletonLeaderLock(ctx, nil, m.db, key, m.instance, poolMonitorLockTTL)
	}
	acquired, err := m.locks.TryAcquireLeaderLock(ctx, key, m.instance, poolMonitorLockTTL)
	if err != nil || !acquired {
		// Fail closed when Redis is configured: falling back to a separate DB
		// lock could race another instance that still holds the Redis lock.
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.locks.ReleaseLeaderLock(releaseCtx, key, m.instance)
	}, true
}

func (m *PoolQuotaMonitor) saveAttempt(accountID int64, snapshot *poolQuotaSnapshot) {
	// Persist the failed attempt even when the upstream probe timed out; otherwise
	// a few slow accounts can permanently starve the rest of the pool.
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()
	if err := m.accounts.UpdateExtra(saveCtx, accountID, map[string]any{PoolQuotaSnapshotExtraKey: snapshot}); err != nil {
		slog.Warn("pool_quota_monitor_persist_failed", "account_id", accountID, "error", err)
	}
}

func (m *PoolQuotaMonitor) observe(ctx context.Context, account *Account) *PoolQuotaObservation {
	now := time.Now()
	if account.IsSyntheticUITest() {
		// UI load-test credentials must never result in external provider calls.
		if account.Platform == PlatformOpenAI {
			return poolUsageObservation(account, &UsageInfo{UpdatedAt: &now}, now)
		}
		if account.Platform == PlatformAnthropic && m.usage != nil {
			usage, err := m.usage.GetUsageForMonitoring(ctx, account)
			if err == nil {
				return poolUsageObservation(account, usage, now)
			}
		}
		return nil
	}
	if IsOllamaCloudUsageAccount(account) {
		if m.ollama == nil {
			return nil
		}
		state, err := m.ollama.GetState(ctx, account.ID)
		if err != nil || state == nil {
			return nil
		}
		// Respect the existing per-account Ollama session and refresh settings.
		return poolOllamaObservation(state.Snapshot, now)
	}
	if account.Platform == PlatformOpenAI {
		if m.openai == nil {
			return nil
		}
		quota, err := m.openai.QueryUsage(ctx, account.ID)
		if err != nil || quota == nil || quota.FetchedAt <= 0 {
			return nil
		}
		return poolOpenAIObservation(account, quota, now)
	}
	if account.Platform == PlatformGrok {
		sample := *account
		sample.Extra = make(map[string]any, len(account.Extra)+1)
		for key, value := range account.Extra {
			sample.Extra[key] = value
		}
		billingFailed := false
		if account.Type == AccountTypeOAuth && m.grok != nil {
			if result, err := m.grok.ProbeBillingForMonitoring(ctx, account.ID); err == nil && result != nil && result.Billing != nil {
				sample.Extra[grokBillingExtraKey] = result.Billing
			} else {
				billingFailed = true
			}
		}
		observation := poolUsageObservation(&sample, NewGrokQuotaFetcher().BuildUsageInfo(&sample), now)
		if observation != nil && billingFailed {
			observation.Partial = true
		}
		return observation
	}
	if account.IsOpenCodeGoPlan() || (account.IsCNProvider() && account.IsCodingPlan()) {
		if m.cnQuota == nil {
			return nil
		}
		result, err := m.cnQuota.QueryUsageForMonitoring(ctx, account)
		if err != nil {
			return nil
		}
		return poolCNQuotaObservation(result, now)
	}
	if account.IsCNProvider() {
		if m.cnBalance == nil {
			return nil
		}
		result, err := m.cnBalance.QueryBalanceForMonitoring(ctx, account)
		if err != nil {
			return nil
		}
		return poolCNBalanceObservation(result, monitorBalanceThreshold(m.cfg), now)
	}
	if m.usage == nil {
		return nil
	}
	usage, err := m.usage.GetUsageForMonitoring(ctx, account)
	if err != nil {
		return nil
	}
	return poolUsageObservation(account, usage, now)
}
