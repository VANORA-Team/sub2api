package repository

import (
	"context"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Keep fork-only assertions out of upstream's probe tests so upstream can
// evolve its SQL fixtures without repeatedly conflicting with quota coverage.
func TestPoolQuotaProbeEditUsesCurrentSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name              string
		identityUnchanged bool
		currentExtra      []byte
		wantSnapshot      bool
	}{
		{"same identity preserves database snapshot", true, []byte(`{"pool_quota_monitor_snapshot":{"identity":"fresh"}}`), true},
		{"changed identity removes snapshot", false, []byte(`{"pool_quota_monitor_snapshot":{"identity":"fresh"}}`), false},
		{"deleted snapshot is not resurrected", true, []byte(`{}`), false},
		{"null snapshot is not resurrected", true, []byte(`{"pool_quota_monitor_snapshot":null}`), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(29), service.PlatformAnthropic, service.AccountTypeAPIKey, `{"api_key":"key","base_url":"https://ollama.com"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{
					"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged",
					"enabled", "rate_sync_enabled", "snapshot",
					"ollama_session", "ollama_auto", "ollama_snapshot",
					"opencode_group_unchanged", "opencode_auto", "opencode_snapshot",
					"current_extra", "concurrency",
				}).AddRow(tt.identityUnchanged, tt.identityUnchanged, true,
					nil, nil, nil, nil, nil, nil, false, nil, nil, tt.currentExtra, 3))

			account := &service.Account{
				ID: 29, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "key", "base_url": "https://ollama.com"},
				Extra: map[string]any{
					service.PoolQuotaSnapshotExtraKey: map[string]any{"identity": "forged"},
				},
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			if tt.wantSnapshot {
				require.Equal(t, map[string]any{"identity": "fresh"}, got[service.PoolQuotaSnapshotExtraKey])
			} else {
				require.NotContains(t, got, service.PoolQuotaSnapshotExtraKey)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
