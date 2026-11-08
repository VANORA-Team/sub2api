package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

const (
	PoolQuotaSnapshotExtraKey = "pool_quota_monitor_snapshot"
	poolQuotaLowUsedPercent   = 90.0
)

// poolQuotaSnapshot is persisted on the account so the evaluator and admin
// dashboard see the same samples across instances. Identity is never sent to clients.
type poolQuotaSnapshot struct {
	Identity    string                `json:"identity"`
	AttemptedAt time.Time             `json:"attempted_at"`
	Observation *PoolQuotaObservation `json:"observation,omitempty"`
}

type PoolQuotaHealth struct {
	Known       int64 `json:"known"`
	Unknown     int64 `json:"unknown"`
	Low         int64 `json:"low"`
	BalanceLow  int64 `json:"balance_low"`
	Unsupported int64 `json:"unsupported"`
}

func poolAccountMonitored(account *Account) bool {
	if account == nil || account.ID <= 0 {
		return false
	}
	if IsOllamaCloudUsageAccount(account) {
		return true
	}
	switch account.Platform {
	case PlatformOpenAI:
		return account.Type == AccountTypeOAuth
	case PlatformAnthropic:
		return account.Type == AccountTypeOAuth || account.Type == AccountTypeSetupToken
	case PlatformGemini:
		return account.Type == AccountTypeOAuth || account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount
	case PlatformAntigravity:
		return account.Type == AccountTypeOAuth || account.GetCredential("access_token") != ""
	case PlatformGrok:
		return account.Type == AccountTypeOAuth || account.Type == AccountTypeAPIKey
	case PlatformOpenCodeGo:
		return account.IsOpenCodeGoPlan()
	case PlatformCommandCode:
		return account.commandCodeUsageSupported()
	case PlatformKimi, PlatformZhipu, PlatformMiniMax:
		if account.IsCodingPlan() {
			return account.GetCodingPlanProvider() != ""
		}
		if account.Platform != PlatformKimi {
			return false
		}
		// The Kimi balance service uses a fixed Moonshot endpoint. Do not send a
		// third-party key from a custom upstream to that endpoint automatically.
		rawBase := strings.TrimSpace(account.GetCredential("base_url"))
		if rawBase == "" {
			rawBase = DefaultKimiPayGBaseURL
		}
		base, err := url.Parse(rawBase)
		return err == nil && strings.EqualFold(base.Scheme, "https") &&
			strings.EqualFold(base.Hostname(), "api.moonshot.cn") && base.User == nil &&
			(base.Port() == "" || base.Port() == "443")
	case PlatformDeepseek:
		if account.IsCodingPlan() {
			return false
		}
		if account.IsAnthropicProtocol() && strings.TrimSpace(account.GetCredential("base_url")) != "" {
			base, err := url.Parse(account.GetCredential("base_url"))
			if err != nil || !strings.EqualFold(base.Scheme, "https") ||
				!strings.EqualFold(base.Hostname(), "api.deepseek.com") || base.User != nil ||
				(base.Port() != "" && base.Port() != "443") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func poolAccountIdentity(account *Account, parents ...*Account) string {
	if account == nil {
		return ""
	}
	var parentIdentity string
	if account.ParentAccountID != nil {
		if len(parents) == 0 || parents[0] == nil || parents[0].ID != *account.ParentAccountID {
			return ""
		}
		parentIdentity = poolAccountIdentity(parents[0])
		if parentIdentity == "" {
			return ""
		}
	}
	payload, err := json.Marshal(struct {
		ID             int64          `json:"id"`
		Platform       string         `json:"platform"`
		Type           string         `json:"type"`
		Credentials    map[string]any `json:"credentials"`
		ProxyID        *int64         `json:"proxy_id"`
		ParentID       *int64         `json:"parent_id"`
		QuotaDimension string         `json:"quota_dimension"`
		OllamaSession  any            `json:"ollama_session"`
		ParentIdentity string         `json:"parent_identity"`
	}{account.ID, account.Platform, account.Type, account.Credentials, account.ProxyID, account.ParentAccountID, account.QuotaDimensionOrDefault(), account.Extra[OllamaCloudUsageSessionExtraKey], parentIdentity})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func poolSnapshotFromAccount(account *Account) *poolQuotaSnapshot {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[PoolQuotaSnapshotExtraKey]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snapshot poolQuotaSnapshot
	if json.Unmarshal(encoded, &snapshot) != nil {
		return nil
	}
	return &snapshot
}

func poolCurrentObservation(account *Account, now time.Time, parents ...*Account) *PoolQuotaObservation {
	snapshot := poolSnapshotFromAccount(account)
	if snapshot == nil || snapshot.Identity == "" || snapshot.Identity != poolAccountIdentity(account, parents...) ||
		snapshot.Observation == nil || !poolObservationFresh(&snapshot.Observation.ObservedAt, now) {
		return nil
	}
	// Copy before filtering; stored snapshots may be shared by other readers.
	result := *snapshot.Observation
	result.Windows = nil
	for _, tier := range snapshot.Observation.Windows {
		if !math.IsNaN(tier.UsedPercent) && !math.IsInf(tier.UsedPercent, 0) &&
			tier.UsedPercent >= 0 && poolTierCurrent(tier, now) {
			result.Windows = append(result.Windows, tier)
		} else if tier.Window != "minute" {
			result.Partial = true
		}
	}
	return nonEmptyPoolObservation(&result)
}

// An RPM window describes burst rate, not stock that can be replenished.
func poolObservationLow(observation *PoolQuotaObservation) (bool, bool) {
	if observation == nil {
		return false, false
	}
	evaluable, low := false, false
	for _, tier := range observation.Windows {
		if tier.Window == "minute" {
			continue
		}
		evaluable = true
		low = low || tier.UsedPercent >= poolQuotaLowUsedPercent
	}
	if observation.BalanceLow != nil {
		evaluable = true
		low = low || *observation.BalanceLow
	}
	// Currency-specific thresholds are evaluated by the separate balance rules.
	if len(observation.Balances) > 0 {
		evaluable = true
	}
	for _, credit := range observation.Credits {
		if credit.MinimumBalanceKnown || credit.MinimumBalance > 0 {
			evaluable = true
			low = low || credit.Amount <= credit.MinimumBalance
		}
	}
	return evaluable, low
}

func poolObservationMetric(accounts map[int64]*AccountAvailability, metric string) (float64, bool) {
	var value float64
	found := false
	unique := make(map[string]*AccountAvailability)
	for _, account := range accounts {
		poolMergeAccount(unique, account)
	}
	for _, account := range unique {
		if account == nil {
			continue
		}
		if metric == "pool_max_usage_percent" {
			for _, tier := range account.LocalQuota {
				if !found || tier.UsedPercent > value {
					value, found = tier.UsedPercent, true
				}
			}
		}
		if !account.QuotaMonitored || account.Quota == nil {
			continue
		}
		if metric == "pool_balance_buffer_ratio" {
			strongest, hasBalance := poolBalanceBufferRatio(account.Quota)
			if hasBalance && (!found || strongest < value) {
				value, found = strongest, true
			}
			continue
		}
		if metric == "pool_max_usage_percent" {
			for _, tier := range account.Quota.Windows {
				if tier.Window == "minute" {
					continue
				}
				if !found || tier.UsedPercent > value {
					value, found = tier.UsedPercent, true
				}
			}
			continue
		}
		currency := ""
		switch metric {
		case "pool_min_balance_usd":
			currency = "USD"
		case "pool_min_balance_cny":
			currency = "CNY"
		default:
			return 0, false
		}
		for _, balance := range account.Quota.Balances {
			if !strings.EqualFold(strings.TrimSpace(balance.Currency), currency) ||
				math.IsNaN(balance.Balance) || math.IsInf(balance.Balance, 0) {
				continue
			}
			if !found || balance.Balance < value {
				value, found = balance.Balance, true
			}
		}
	}
	if metric == "pool_max_usage_percent" {
		value = math.Min(value, 100)
	}
	return value, found
}

func poolHealthFromAccounts(accounts map[int64]*AccountAvailability) PoolQuotaHealth {
	unique := make(map[string]*AccountAvailability)
	for _, account := range accounts {
		poolMergeAccount(unique, account)
	}
	return poolHealthFromUniqueAccounts(unique)
}

func poolHealthFromUniqueAccounts(accounts map[string]*AccountAvailability) PoolQuotaHealth {
	var health PoolQuotaHealth
	for _, account := range accounts {
		if account == nil {
			continue
		}
		known, low := poolObservationLow(account.Quota)
		if ratio, ok := poolBalanceBufferRatio(account.Quota); ok && ratio <= 1 {
			health.BalanceLow++
		}
		for _, tier := range account.LocalQuota {
			known = true
			low = low || tier.UsedPercent >= poolQuotaLowUsedPercent
		}
		if account.QuotaMonitored && (account.Quota == nil || account.Quota.Partial) {
			health.Unknown++
			if low {
				health.Low++
			}
			continue
		}
		if !known {
			health.Unknown++
		} else {
			health.Known++
			if low {
				health.Low++
			}
		}
	}
	return health
}

// Any one adequately funded supported currency is sufficient for this account.
func poolBalanceBufferRatio(observation *PoolQuotaObservation) (float64, bool) {
	if observation == nil {
		return 0, false
	}
	var strongest float64
	found := false
	for _, balance := range observation.Balances {
		if math.IsNaN(balance.Balance) || math.IsInf(balance.Balance, 0) {
			continue
		}
		var baseline float64
		switch strings.ToUpper(strings.TrimSpace(balance.Currency)) {
		case "USD":
			baseline = 5
		case "CNY":
			baseline = 30
		default:
			continue
		}
		ratio := balance.Balance / baseline
		if !found || ratio > strongest {
			strongest, found = ratio, true
		}
	}
	return strongest, found
}

func poolHasMissingObservation(accounts map[int64]*AccountAvailability) bool {
	unique := make(map[string]*AccountAvailability)
	for _, account := range accounts {
		poolMergeAccount(unique, account)
	}
	for _, account := range unique {
		if account != nil && account.QuotaMonitored && (account.Quota == nil || account.Quota.Partial) {
			return true
		}
	}
	return false
}

func poolAccountKey(account *Account, parents ...*Account) string {
	if IsOllamaCloudUsageAccount(account) {
		if fingerprint, ok := ollamaCloudUsageGroupFingerprint(account); ok {
			return "ollama:" + fingerprint
		}
	}
	credentialAccount := account
	if account.IsShadow() && len(parents) > 0 && parents[0] != nil {
		credentialAccount = parents[0]
	}
	dimension := "global"
	if account.IsShadow() {
		dimension = account.QuotaDimensionOrDefault()
	}
	var identity string
	if credentialAccount.Platform == PlatformOpenAI && credentialAccount.Type == AccountTypeOAuth {
		workspace := strings.TrimSpace(credentialAccount.GetCredential("chatgpt_account_id"))
		user := strings.TrimSpace(credentialAccount.GetCredential("chatgpt_user_id"))
		if workspace != "" && user != "" {
			identity = workspace + "\x00" + user
		}
	}
	if identity == "" {
		identity = strings.TrimSpace(credentialAccount.GetCredential("api_key"))
	}
	if identity == "" {
		identity = strings.TrimSpace(credentialAccount.GetCredential("refresh_token"))
	}
	if identity == "" {
		identity = strings.TrimSpace(credentialAccount.GetCredential("access_token"))
	}
	if identity != "" {
		digest := sha256.Sum256([]byte(credentialAccount.Platform + "\x00" +
			credentialAccount.Type + "\x00" + credentialAccount.GetCredential("base_url") + "\x00" +
			credentialAccount.GetAccountMode() + "\x00" +
			credentialAccount.GetCredential("chatgpt_account_is_fedramp") + "\x00" + dimension + "\x00" + identity))
		return "credential:" + hex.EncodeToString(digest[:])
	}
	return fmt.Sprintf("account:%d", account.ID)
}

func poolMergeAccount(unique map[string]*AccountAvailability, account *AccountAvailability) {
	if account == nil || (!account.QuotaMonitored && len(account.LocalQuota) == 0) {
		return
	}
	key := account.QuotaPoolKey
	if key == "" {
		key = fmt.Sprintf("account:%d", account.AccountID)
	}
	existing := unique[key]
	if existing == nil {
		copy := *account
		copy.LocalQuota = append([]domain.MonitorQuotaTier(nil), account.LocalQuota...)
		unique[key] = &copy
		return
	}
	allLocal := append(append([]domain.MonitorQuotaTier(nil), existing.LocalQuota...), account.LocalQuota...)
	if (account.Quota != nil &&
		(existing.Quota == nil || account.Quota.ObservedAt.After(existing.Quota.ObservedAt))) ||
		(existing.Quota == nil && account.Quota == nil && poolCodexSampleTime(account).After(poolCodexSampleTime(existing))) {
		copy := *account
		copy.LocalQuota = allLocal
		copy.QuotaMonitored = copy.QuotaMonitored || existing.QuotaMonitored
		unique[key] = &copy
	} else {
		existing.LocalQuota = allLocal
		existing.QuotaMonitored = existing.QuotaMonitored || account.QuotaMonitored
	}
}

// poolLocalQuota reads administrator-configured spending caps. These are
// local controls, not a claim about the provider's cash balance.
func poolLocalQuota(account *Account) []domain.MonitorQuotaTier {
	if account == nil || !account.IsAPIKeyOrBedrock() {
		return nil
	}
	items := []struct {
		window  string
		usedKey string
		limit   float64
		expired bool
	}{
		{"total", "quota_used", account.GetQuotaLimit(), false},
		{"daily", "quota_daily_used", account.GetQuotaDailyLimit(), account.IsDailyQuotaPeriodExpired()},
		{"weekly", "quota_weekly_used", account.GetQuotaWeeklyLimit(), account.IsWeeklyQuotaPeriodExpired()},
	}
	var tiers []domain.MonitorQuotaTier
	for _, item := range items {
		if item.limit <= 0 || math.IsNaN(item.limit) || math.IsInf(item.limit, 0) {
			continue
		}
		used := 0.0
		if raw, exists := account.Extra[item.usedKey]; exists && raw != nil && !item.expired {
			value, valid := poolFiniteNonnegative(raw)
			if !valid {
				continue
			}
			used = value
		}
		percent := used / item.limit * 100
		if math.IsInf(percent, 0) {
			percent = 100
		}
		tiers = append(tiers, domain.MonitorQuotaTier{
			Window: item.window, Label: "local_limit_usd",
			Used: used, Limit: item.limit, UsedPercent: percent,
		})
	}
	return tiers
}

func poolCodexSampleTime(account *AccountAvailability) time.Time {
	if account == nil || account.codexAccount == nil {
		return time.Time{}
	}
	at, _ := parseTime(stringValue(account.codexAccount.Extra["codex_usage_updated_at"]))
	return at
}

// poolAlertAccounts gives an operator enough context to identify accounts to
// inspect. It uses only admin-visible names and IDs, never credentials.
func poolAlertAccounts(accounts map[int64]*AccountAvailability, rule *OpsAlertRule) string {
	if rule == nil {
		return ""
	}
	unique := make(map[string]*AccountAvailability)
	for _, account := range accounts {
		poolMergeAccount(unique, account)
	}
	var affected []*AccountAvailability
	for _, account := range unique {
		known, low := poolObservationLow(account.Quota)
		for _, tier := range account.LocalQuota {
			known = true
			low = low || tier.UsedPercent >= poolQuotaLowUsedPercent
		}
		matches := false
		switch rule.MetricType {
		case "pool_quota_low_accounts":
			matches = low
		case "pool_quota_unknown_accounts":
			matches = (account.QuotaMonitored && (account.Quota == nil || account.Quota.Partial)) || !known
		default:
			value, ok := poolObservationMetric(map[int64]*AccountAvailability{account.AccountID: account}, rule.MetricType)
			matches = ok && compareMetric(value, rule.Operator, rule.Threshold)
		}
		if matches {
			affected = append(affected, account)
		}
	}
	sort.Slice(affected, func(i, j int) bool { return affected[i].AccountID < affected[j].AccountID })
	if len(affected) == 0 {
		return ""
	}
	items := make([]string, 0, min(len(affected), 5))
	for _, account := range affected[:min(len(affected), 5)] {
		items = append(items, fmt.Sprintf("%s (#%d)", truncateString(strings.TrimSpace(account.AccountName), 80), account.AccountID))
	}
	if len(affected) > len(items) {
		items = append(items, fmt.Sprintf("+%d", len(affected)-len(items)))
	}
	return strings.Join(items, ", ")
}
