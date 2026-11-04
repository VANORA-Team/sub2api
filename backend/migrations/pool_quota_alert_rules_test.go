package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPoolQuotaAlertRulesMigration(t *testing.T) {
	content, err := FS.ReadFile("239_pool_quota_alert_rules.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "'pool_quota_low_accounts', '>', 0")
	require.Contains(t, sql, "'pool_balance_buffer_ratio', '<=', 1")
	require.Contains(t, sql, "'pool_quota_unknown_accounts', '>', 0")
	require.Contains(t, sql, "ON CONFLICT (name) DO NOTHING")
}
