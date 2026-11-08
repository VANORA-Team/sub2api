//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
	latestRepo     string
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.latestRepo = repo
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called when no update is available")
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	githubClient := &updateServiceGitHubClientStub{
		release: &GitHubRelease{
			TagName: "v0.1.132",
			Name:    "v0.1.132",
		},
	}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		githubClient,
		"0.1.132",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
	require.Equal(t, "VANORA-Team/sub2api", githubClient.latestRepo)
}

func TestUpdateServiceContainerModeBypassesReleaseLookupsAndCache(t *testing.T) {
	t.Setenv("SUB2API_UPDATE_MODE", "external")
	// Nil dependencies ensure even a cache read or a forced release lookup fails
	// the test. Image deployments only report their own installed version.
	svc := NewUpdateService(nil, nil, "0.1.147", "release")

	for _, force := range []bool{false, true} {
		info, err := svc.CheckUpdate(context.Background(), force)
		require.NoError(t, err)
		require.Equal(t, "0.1.147", info.CurrentVersion)
		require.Equal(t, "external", info.UpdateMode)
		require.Equal(t, "https://github.com/VANORA-Team/sub2api", info.RepositoryURL)
		require.False(t, info.HasUpdate)
		require.False(t, info.Cached)
		require.Nil(t, info.ReleaseInfo)
	}

	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.NotNil(t, versions)
	require.Empty(t, versions)
}

func TestUpdateServiceContainerModeRejectsBinaryMutations(t *testing.T) {
	t.Setenv("SUB2API_UPDATE_MODE", "external")
	svc := NewUpdateService(nil, nil, "0.1.147", "release")

	for _, mutation := range []struct {
		name string
		run  func() error
	}{
		{"update", func() error { return svc.PerformUpdate(context.Background()) }},
		{"local rollback", svc.Rollback},
		{"release rollback", func() error { return svc.RollbackToVersion(context.Background(), "0.1.146") }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			err := mutation.run()
			require.ErrorIs(t, err, ErrSelfUpdateDisabled)
			require.Equal(t, http.StatusForbidden, infraerrors.Code(err))
			require.Contains(t, err.Error(), "https://github.com/VANORA-Team/sub2api")
		})
	}
}

func TestUpdateServiceRejectsUpstreamCacheAndCachesForkRelease(t *testing.T) {
	t.Setenv("SUB2API_UPDATE_MODE", "")
	for _, repository := range []string{"", "ranxi2001/sub2api", "Wei-Shaw/sub2api"} {
		t.Run(repository, func(t *testing.T) {
			legacy, err := json.Marshal(map[string]any{
				"repository": repository,
				"latest":     "99.0.0",
				"timestamp":  time.Now().Unix(),
				"release_info": ReleaseInfo{
					HTMLURL: "https://github.com/ranxi2001/sub2api/releases",
				},
			})
			require.NoError(t, err)
			cache := &updateServiceCacheStub{data: string(legacy)}
			client := &updateServiceGitHubClientStub{
				release: &GitHubRelease{
					TagName: "v0.1.148",
					HTMLURL: "https://github.com/VANORA-Team/sub2api/releases/tag/v0.1.148",
				},
			}
			svc := NewUpdateService(cache, client, "0.1.147", "release")

			info, err := svc.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.False(t, info.Cached)
			require.Equal(t, "0.1.148", info.LatestVersion)
			require.Equal(t, "VANORA-Team/sub2api", client.latestRepo)
			require.Equal(t, "inline", info.UpdateMode)
			require.Equal(t, client.release.HTMLURL, info.ReleaseInfo.HTMLURL)

			// A second request must reuse the new fork cache without a lookup.
			client.latestRepo = ""
			info, err = svc.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.True(t, info.Cached)
			require.Empty(t, client.latestRepo)
			require.Equal(t, "0.1.148", info.LatestVersion)
			require.Equal(t, "https://github.com/VANORA-Team/sub2api", info.RepositoryURL)
		})
	}
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}
