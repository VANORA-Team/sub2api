package admin

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CreateFromTemplate creates one batch item atomically with its own replacement key.
// POST /api/v1/admin/accounts/:id/create-from-template
func (h *AccountHandler) CreateFromTemplate(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var input service.CreateAccountFromTemplateInput
	if c.ShouldBindJSON(&input) != nil {
		response.BadRequest(c, "Invalid template account request")
		return
	}
	operationKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if operationKey == "" {
		response.BadRequest(c, "Idempotency-Key is required")
		return
	}
	actorScope := adminActorScope(c)
	var created *service.Account
	result, err := executeAdminIdempotent(c, "admin.accounts.create-from-template", struct {
		AccountID int64 `json:"account_id"`
		service.CreateAccountFromTemplateInput
	}{accountID, input}, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		// Bind the inherited upstream channel in the same transaction as the new account.
		if _, observer := service.ObserverGroupIDs(ctx); !observer && h.upstreamChannels != nil {
			repo, err := h.upstreamChannels.UpstreamChannelRepository()
			if err != nil {
				return nil, err
			}
			bindings, err := repo.ReadUpstreamChannelBindings(ctx, []int64{accountID})
			if err != nil {
				return nil, err
			}
			for _, binding := range bindings {
				if binding.AccountID == accountID && binding.ChannelID > 0 {
					ctx = service.WithInitialUpstreamChannel(ctx, binding.ChannelID)
				}
			}
		}
		account, err := h.adminService.CreateAccountFromTemplate(ctx, accountID, &input, actorScope, operationKey)
		if err != nil {
			return nil, err
		}
		created = account
		return h.buildAccountResponseWithRuntime(ctx, account), nil
	})
	if err != nil {
		reason := infraerrors.Reason(err)
		if reason == infraerrors.Reason(service.ErrIdempotencyInProgress) || reason == infraerrors.Reason(service.ErrIdempotencyStoreUnavail) {
			recovered, recoverErr := h.adminService.RecoverAccountFromTemplate(c.Request.Context(), accountID, &input, actorScope, operationKey)
			if recoverErr != nil {
				slog.Warn("account_template_recovery_failed", "account_id", accountID, "reason", reason)
			} else if recovered != nil {
				c.Header("X-Idempotency-Recovered", "true")
				response.Success(c, h.buildAccountResponseWithRuntime(c.Request.Context(), recovered))
				return
			}
		}
		response.ErrorFrom(c, err)
		return
	}
	if result != nil && result.Replayed {
		c.Header("X-Idempotency-Replayed", "true")
	}
	h.scheduleOpenAIResponsesProbe(created)
	h.scheduleGrokImportProbe(created)
	response.Success(c, result.Data)
}
