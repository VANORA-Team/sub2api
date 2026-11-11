//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type templateAccountAdminServiceStub struct {
	service.AdminService
	calls, recoverCalls int
	input               service.CreateAccountFromTemplateInput
	accountID           int64
	channelID           int64
	actorScope, key     string
}

func (s *templateAccountAdminServiceStub) CreateAccountFromTemplate(ctx context.Context, id int64, input *service.CreateAccountFromTemplateInput, actorScope, key string) (*service.Account, error) {
	s.calls++
	s.input, s.accountID, s.actorScope, s.key = *input, id, actorScope, key
	s.channelID = service.InitialUpstreamChannel(ctx)
	return &service.Account{ID: 43, Name: input.Name, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": input.APIKey}, Schedulable: input.Schedulable}, nil
}

func (s *templateAccountAdminServiceStub) RecoverAccountFromTemplate(ctx context.Context, id int64, input *service.CreateAccountFromTemplateInput, actorScope, key string) (*service.Account, error) {
	s.recoverCalls++
	return &service.Account{ID: 43, Name: input.Name, Type: service.AccountTypeAPIKey}, nil
}

type templateChannelRepoStub struct {
	service.ChannelRepository
	service.UpstreamChannelRepository
	err error
}

func (r *templateChannelRepoStub) ReadUpstreamChannelBindings(context.Context, []int64) ([]service.UpstreamChannelBinding, error) {
	return []service.UpstreamChannelBinding{{AccountID: 42, ChannelID: 9}}, r.err
}

func setupTemplateAccountRouter(t *testing.T, svc *templateAccountAdminServiceStub, channelRepo *templateChannelRepoStub) *gin.Engine {
	router := setupDuplicateAccountRouter(t, svc)
	handler := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if channelRepo != nil {
		handler.SetUpstreamChannelService(service.NewChannelService(channelRepo, nil, nil, nil, nil))
	}
	router.POST("/api/v1/admin/accounts/:id/create-from-template", handler.CreateFromTemplate)
	return router
}

const templateRequestBody = `{"name":"new account","api_key":"new-secret","schedulable":true}`

func callTemplateAccount(router *gin.Engine, body, key string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/create-from-template", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAccountTemplateHandlerCreatesWithRedactedKeyAndInheritedChannel(t *testing.T) {
	svc := &templateAccountAdminServiceStub{}
	router := setupTemplateAccountRouter(t, svc, &templateChannelRepoStub{})
	recorder := callTemplateAccount(router, templateRequestBody, "template-operation")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, svc.calls)
	require.Equal(t, int64(42), svc.accountID)
	require.Equal(t, int64(9), svc.channelID)
	require.Equal(t, "admin:77", svc.actorScope)
	require.Equal(t, "template-operation", svc.key)
	require.Equal(t, "new-secret", svc.input.APIKey)
	require.True(t, svc.input.Schedulable)
	require.NotContains(t, recorder.Body.String(), "new-secret")
}

func TestAccountTemplateHandlerReplaysAndRejectsChangedPayload(t *testing.T) {
	svc := &templateAccountAdminServiceStub{}
	router := setupTemplateAccountRouter(t, svc, nil)
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(newMemoryIdempotencyRepoStub(), service.DefaultIdempotencyConfig()))
	first := callTemplateAccount(router, templateRequestBody, "template-operation")
	second := callTemplateAccount(router, templateRequestBody, "template-operation")
	changed := callTemplateAccount(router, strings.Replace(templateRequestBody, "new-secret", "other-secret", 1), "template-operation")
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, "true", second.Header().Get("X-Idempotency-Replayed"))
	require.Equal(t, http.StatusConflict, changed.Code)
	require.Equal(t, 1, svc.calls)
	require.NotContains(t, second.Body.String(), "new-secret")
}

func TestAccountTemplateHandlerRecoversCommittedCreate(t *testing.T) {
	svc := &templateAccountAdminServiceStub{}
	router := setupTemplateAccountRouter(t, svc, nil)
	repo := &failOnceMarkSucceededRepo{memoryIdempotencyRepoStub: newMemoryIdempotencyRepoStub(), failNext: true}
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(repo, service.DefaultIdempotencyConfig()))
	first := callTemplateAccount(router, templateRequestBody, "template-operation")
	second := callTemplateAccount(router, templateRequestBody, "template-operation")
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, "true", first.Header().Get("X-Idempotency-Recovered"))
	require.Equal(t, 1, svc.calls)
	require.Equal(t, 2, svc.recoverCalls)
}

func TestAccountTemplateHandlerRejectsInvalidRequestBeforeCreating(t *testing.T) {
	for _, body := range []string{"not-json", `{"api_key":42}`} {
		svc := &templateAccountAdminServiceStub{}
		router := setupTemplateAccountRouter(t, svc, nil)
		recorder := callTemplateAccount(router, body, "operation")
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Zero(t, svc.calls)
	}
	svc := &templateAccountAdminServiceStub{}
	router := setupTemplateAccountRouter(t, svc, nil)
	recorder := callTemplateAccount(router, templateRequestBody, "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, svc.calls)
}

func TestAccountTemplateHandlerChannelLookupFailureDoesNotCreate(t *testing.T) {
	svc := &templateAccountAdminServiceStub{}
	router := setupTemplateAccountRouter(t, svc, &templateChannelRepoStub{err: errors.New("database unavailable")})
	recorder := callTemplateAccount(router, templateRequestBody, "operation")
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Zero(t, svc.calls)
}
