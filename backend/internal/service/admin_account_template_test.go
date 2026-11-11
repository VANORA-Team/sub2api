//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAccountTemplateCopiesSettingsWithIndependentKeys(t *testing.T) {
	ctx := context.Background()
	repo := newDuplicateAccountRepoStub()
	svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
	proxyID, loadFactor := int64(8), 3
	rate, groupRate := 1.5, 0.75
	notes := "template settings"
	expires := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	source := &Account{
		Name: "source", Notes: &notes, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "source-secret", "base_url": "https://upstream.example",
			"model_mapping": map[string]any{"client-model": "provider-model"},
		},
		Extra:   map[string]any{"quota_limit": 100, "quota_used": 50, "base_rpm": 30},
		ProxyID: &proxyID, Concurrency: 6, Priority: 42, LoadFactor: &loadFactor,
		RateMultiplier: &rate, GroupRateMultiplier: &groupRate,
		ExpiresAt: &expires, AutoPauseOnExpired: true,
		GroupIDs:      []int64{7},
		AccountGroups: []AccountGroup{{GroupID: 7, Priority: 13, AllowedModels: []string{"client-model"}}},
	}
	require.NoError(t, repo.Create(ctx, source))

	for i, key := range []string{"new-secret-1", "new-secret-2"} {
		input := &CreateAccountFromTemplateInput{Name: "new account", APIKey: key, Schedulable: i == 0}
		created, err := svc.CreateAccountFromTemplate(ctx, source.ID, input, "admin:1", key+"-operation")
		require.NoError(t, err)
		require.Equal(t, input.Name, created.Name)
		require.Equal(t, key, created.GetCredential("api_key"))
		require.Equal(t, source.GetCredential("base_url"), created.GetCredential("base_url"))
		require.Equal(t, source.Credentials["model_mapping"], created.Credentials["model_mapping"])
		require.Equal(t, source.Concurrency, created.Concurrency)
		require.Equal(t, source.Priority, created.Priority)
		require.Equal(t, source.ProxyID, created.ProxyID)
		require.Equal(t, source.Notes, created.Notes)
		require.Equal(t, source.RateMultiplier, created.RateMultiplier)
		require.Equal(t, source.GroupRateMultiplier, created.GroupRateMultiplier)
		require.Equal(t, source.LoadFactor, created.LoadFactor)
		require.Equal(t, source.ExpiresAt, created.ExpiresAt)
		require.Equal(t, source.AutoPauseOnExpired, created.AutoPauseOnExpired)
		require.Equal(t, input.Schedulable, created.Schedulable)
		require.Equal(t, float64(100), created.Extra["quota_limit"])
		require.Equal(t, float64(30), created.Extra["base_rpm"])
		require.NotContains(t, created.Extra, "quota_used")
		require.Equal(t, []AccountGroup{{AccountID: created.ID, GroupID: 7, Priority: 13, AllowedModels: []string{"client-model"}}}, created.AccountGroups)
		created.AccountGroups[0].AllowedModels[0] = "changed"
		require.Equal(t, []string{"client-model"}, source.AccountGroups[0].AllowedModels)
	}
	require.Equal(t, "source-secret", source.GetCredential("api_key"))
	require.Len(t, repo.accounts, 3)
}

func TestAccountTemplateRetryRecoveryIsBoundToReplacementAndActor(t *testing.T) {
	ctx := context.Background()
	repo := newDuplicateAccountRepoStub()
	svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
	source := &Account{Name: "source", Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "source-secret"}}
	require.NoError(t, repo.Create(ctx, source))
	input := &CreateAccountFromTemplateInput{Name: "new", APIKey: "new-secret", Schedulable: true}

	first, err := svc.CreateAccountFromTemplate(ctx, source.ID, input, "admin:1", "operation")
	require.NoError(t, err)
	second, err := svc.CreateAccountFromTemplate(ctx, source.ID, input, "admin:1", "operation")
	require.NoError(t, err)
	recovered, err := svc.RecoverAccountFromTemplate(ctx, source.ID, input, "admin:1", "operation")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.ID, recovered.ID)
	require.Len(t, repo.accounts, 2)
	otherActor, err := svc.RecoverAccountFromTemplate(ctx, source.ID, input, "admin:2", "operation")
	require.NoError(t, err)
	require.Nil(t, otherActor)
	changed := *input
	changed.APIKey = "other-secret"
	wrongPayload, err := svc.RecoverAccountFromTemplate(ctx, source.ID, &changed, "admin:1", "operation")
	require.NoError(t, err)
	require.Nil(t, wrongPayload)
	oldDuplicate, err := svc.RecoverDuplicateAccount(ctx, source.ID, "admin:1", "operation")
	require.NoError(t, err)
	require.Nil(t, oldDuplicate)
}

func TestAccountTemplateRejectsInvalidInputWithoutCreating(t *testing.T) {
	for _, input := range []*CreateAccountFromTemplateInput{
		nil,
		{Name: "", APIKey: "key"},
		{Name: strings.Repeat("名", 101), APIKey: "key"},
		{Name: "bad\nname", APIKey: "key"},
		{Name: "new", APIKey: ""},
		{Name: "new", APIKey: "two keys"},
		{Name: "new", APIKey: strings.Repeat("k", 16385)},
		{Name: "new", APIKey: "source-secret"},
	} {
		repo := newDuplicateAccountRepoStub()
		svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
		source := &Account{Name: "source", Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "source-secret"}}
		require.NoError(t, repo.Create(context.Background(), source))
		_, err := svc.CreateAccountFromTemplate(context.Background(), source.ID, input, "admin:1", "operation")
		require.Error(t, err)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Len(t, repo.accounts, 1)
	}
}

func TestAccountTemplateCredentialModes(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeUpstream, AccountTypeBedrock, AccountTypeServiceAccount, AccountTypeOAuth} {
		for _, mode := range []string{"apikey", "sigv4", ""} {
			t.Run(accountType+"/"+mode, func(t *testing.T) {
				repo := newDuplicateAccountRepoStub()
				svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
				source := &Account{
					Name: "source", Platform: PlatformAnthropic, Type: accountType,
					Credentials: map[string]any{"api_key": "source-secret", "auth_mode": mode},
				}
				require.NoError(t, repo.Create(context.Background(), source))
				_, err := svc.CreateAccountFromTemplate(context.Background(), source.ID, &CreateAccountFromTemplateInput{Name: "new", APIKey: "new-secret"}, "admin:1", "operation")
				supported := accountType == AccountTypeAPIKey || accountType == AccountTypeUpstream ||
					((accountType == AccountTypeBedrock || accountType == AccountTypeServiceAccount) && mode == "apikey")
				if supported {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					require.Len(t, repo.accounts, 1)
				}
			})
		}
	}
}
