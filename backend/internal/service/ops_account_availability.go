package service

import (
	"context"
	"errors"
	"time"
)

// GetAccountAvailabilityStats returns current account availability stats.
//
// Query-level filtering is intentionally limited to platform/group to match the dashboard scope.
func (s *OpsService) GetAccountAvailabilityStats(ctx context.Context, platformFilter string, groupIDFilter *int64) (
	map[string]*PlatformAvailability,
	map[int64]*GroupAvailability,
	map[int64]*AccountAvailability,
	*time.Time,
	error,
) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, nil, nil, nil, err
	}

	accounts, err := s.listAllAccountsForOps(ctx, platformFilter, groupIDFilter)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if groupIDFilter != nil && *groupIDFilter > 0 {
		filtered := make([]Account, 0, len(accounts))
		for _, acc := range accounts {
			for _, grp := range acc.Groups {
				if grp != nil && grp.ID == *groupIDFilter {
					filtered = append(filtered, acc)
					break
				}
			}
		}
		accounts = filtered
	}

	now := time.Now()
	collectedAt := now

	platform := make(map[string]*PlatformAvailability)
	group := make(map[int64]*GroupAvailability)
	account := make(map[int64]*AccountAvailability)
	platformPoolAccounts := make(map[string]map[string]*AccountAvailability)
	groupPoolAccounts := make(map[int64]map[string]*AccountAvailability)
	parentIDs := make([]int64, 0)
	seenParents := make(map[int64]bool)
	for i := range accounts {
		if id := accounts[i].ParentAccountID; id != nil && !seenParents[*id] {
			seenParents[*id] = true
			parentIDs = append(parentIDs, *id)
		}
	}
	parents := make(map[int64]*Account)
	if len(parentIDs) > 0 && s.accountRepo != nil {
		if loaded, err := s.accountRepo.GetByIDs(ctx, parentIDs); err == nil {
			for _, parent := range loaded {
				if parent != nil {
					parents[parent.ID] = parent
				}
			}
		}
	}

	for _, acc := range accounts {
		if acc.ID <= 0 {
			continue
		}

		isTempUnsched := false
		if acc.TempUnschedulableUntil != nil && now.Before(*acc.TempUnschedulableUntil) {
			isTempUnsched = true
		}

		isRateLimited := acc.RateLimitResetAt != nil && now.Before(*acc.RateLimitResetAt)
		isOverloaded := acc.OverloadUntil != nil && now.Before(*acc.OverloadUntil)
		hasError := acc.Status == StatusError

		// Normalize exclusive status flags so the UI doesn't show conflicting badges.
		if hasError {
			isRateLimited = false
			isOverloaded = false
		}

		isAvailable := acc.Status == StatusActive && acc.Schedulable && !isRateLimited && !isOverloaded && !isTempUnsched
		var parent *Account
		if acc.ParentAccountID != nil {
			parent = parents[*acc.ParentAccountID]
		}
		quota := poolCurrentObservation(&acc, now, parent)
		localQuota := poolLocalQuota(&acc)
		var quotaLastAttemptAt *time.Time
		if snapshot := poolSnapshotFromAccount(&acc); snapshot != nil && snapshot.Identity != "" &&
			snapshot.Identity == poolAccountIdentity(&acc, parent) && !snapshot.AttemptedAt.IsZero() {
			quotaLastAttemptAt = &snapshot.AttemptedAt
		}
		accountCopy := acc
		quotaEntry := &AccountAvailability{
			AccountID: acc.ID, QuotaMonitored: poolAccountMonitored(&acc),
			Quota: quota, LocalQuota: localQuota, QuotaPoolKey: poolAccountKey(&acc, parent), codexAccount: &accountCopy,
		}

		if acc.Platform != "" {
			if _, ok := platform[acc.Platform]; !ok {
				platform[acc.Platform] = &PlatformAvailability{
					Platform: acc.Platform,
				}
			}
			p := platform[acc.Platform]
			p.TotalAccounts++
			if !quotaEntry.QuotaMonitored {
				p.PoolQuota.Unsupported++
			}
			if platformPoolAccounts[acc.Platform] == nil {
				platformPoolAccounts[acc.Platform] = make(map[string]*AccountAvailability)
			}
			poolMergeAccount(platformPoolAccounts[acc.Platform], quotaEntry)
			if isAvailable {
				p.AvailableCount++
			}
			if isRateLimited {
				p.RateLimitCount++
			}
			if hasError {
				p.ErrorCount++
			}
		}

		for _, grp := range acc.Groups {
			if grp == nil || grp.ID <= 0 {
				continue
			}
			if _, ok := group[grp.ID]; !ok {
				group[grp.ID] = &GroupAvailability{
					GroupID:   grp.ID,
					GroupName: grp.Name,
					Platform:  grp.Platform,
				}
			}
			g := group[grp.ID]
			g.TotalAccounts++
			if !quotaEntry.QuotaMonitored {
				g.PoolQuota.Unsupported++
			}
			if groupPoolAccounts[grp.ID] == nil {
				groupPoolAccounts[grp.ID] = make(map[string]*AccountAvailability)
			}
			poolMergeAccount(groupPoolAccounts[grp.ID], quotaEntry)
			if isAvailable {
				g.AvailableCount++
			}
			if isRateLimited {
				g.RateLimitCount++
			}
			if hasError {
				g.ErrorCount++
			}
		}

		displayGroupID := int64(0)
		displayGroupName := ""
		if len(acc.Groups) > 0 && acc.Groups[0] != nil {
			displayGroupID = acc.Groups[0].ID
			displayGroupName = acc.Groups[0].Name
		}

		item := &AccountAvailability{
			AccountID:   acc.ID,
			AccountName: acc.Name,
			Platform:    acc.Platform,
			GroupID:     displayGroupID,
			GroupName:   displayGroupName,
			Status:      acc.Status,

			IsAvailable:        isAvailable,
			IsRateLimited:      isRateLimited,
			IsOverloaded:       isOverloaded,
			HasError:           hasError,
			QuotaMonitored:     poolAccountMonitored(&acc),
			QuotaUnsupported:   !quotaEntry.QuotaMonitored,
			Quota:              quota,
			LocalQuota:         localQuota,
			QuotaLastAttemptAt: quotaLastAttemptAt,
			QuotaPoolKey:       quotaEntry.QuotaPoolKey,

			ErrorMessage: acc.ErrorMessage,
		}

		if isRateLimited && acc.RateLimitResetAt != nil {
			item.RateLimitResetAt = acc.RateLimitResetAt
			remainingSec := int64(time.Until(*acc.RateLimitResetAt).Seconds())
			if remainingSec > 0 {
				item.RateLimitRemainingSec = &remainingSec
			}
		}
		if isOverloaded && acc.OverloadUntil != nil {
			item.OverloadUntil = acc.OverloadUntil
			remainingSec := int64(time.Until(*acc.OverloadUntil).Seconds())
			if remainingSec > 0 {
				item.OverloadRemainingSec = &remainingSec
			}
		}
		if isTempUnsched && acc.TempUnschedulableUntil != nil {
			item.TempUnschedulableUntil = acc.TempUnschedulableUntil
		}

		account[acc.ID] = item
	}
	for key, entries := range platformPoolAccounts {
		health := poolHealthFromUniqueAccounts(entries)
		health.Unsupported = platform[key].PoolQuota.Unsupported
		platform[key].PoolQuota = health
		for _, entry := range entries {
			addCodexPoolQuota(&platform[key].CodexQuota, entry.codexAccount, now, entry.Quota)
		}
	}
	for key, entries := range groupPoolAccounts {
		health := poolHealthFromUniqueAccounts(entries)
		health.Unsupported = group[key].PoolQuota.Unsupported
		group[key].PoolQuota = health
		for _, entry := range entries {
			addCodexPoolQuota(&group[key].CodexQuota, entry.codexAccount, now, entry.Quota)
		}
	}

	return platform, group, account, &collectedAt, nil
}

type OpsAccountAvailability struct {
	Group       *GroupAvailability
	Accounts    map[int64]*AccountAvailability
	CollectedAt *time.Time
}

func (s *OpsService) GetAccountAvailability(ctx context.Context, platformFilter string, groupIDFilter *int64) (*OpsAccountAvailability, error) {
	if s == nil {
		return nil, errors.New("ops service is nil")
	}

	if s.getAccountAvailability != nil {
		return s.getAccountAvailability(ctx, platformFilter, groupIDFilter)
	}

	_, groupStats, accountStats, collectedAt, err := s.GetAccountAvailabilityStats(ctx, platformFilter, groupIDFilter)
	if err != nil {
		return nil, err
	}

	var group *GroupAvailability
	if groupIDFilter != nil && *groupIDFilter > 0 {
		group = groupStats[*groupIDFilter]
	}

	if accountStats == nil {
		accountStats = map[int64]*AccountAvailability{}
	}

	return &OpsAccountAvailability{
		Group:       group,
		Accounts:    accountStats,
		CollectedAt: collectedAt,
	}, nil
}
