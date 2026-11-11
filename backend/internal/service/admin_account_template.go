package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// CreateAccountFromTemplateInput replaces the identity of an account while retaining
// its configuration. Each item in a batch has its own idempotency key.
type CreateAccountFromTemplateInput struct {
	Name        string `json:"name"`
	APIKey      string `json:"api_key"`
	Schedulable bool   `json:"schedulable"`
}

func normalizeAccountTemplateInput(input *CreateAccountFromTemplateInput) (*CreateAccountFromTemplateInput, error) {
	if input == nil {
		return nil, infraerrors.BadRequest("ACCOUNT_TEMPLATE_INPUT_INVALID", "account name and a new API key are required")
	}
	normalized := *input
	normalized.Name = strings.TrimSpace(input.Name)
	normalized.APIKey = strings.TrimSpace(input.APIKey)
	if !utf8.ValidString(normalized.Name) || normalized.Name == "" ||
		utf8.RuneCountInString(normalized.Name) > maxAccountNameRunes ||
		strings.IndexFunc(normalized.Name, unicode.IsControl) >= 0 ||
		normalized.APIKey == "" || len(normalized.APIKey) > 16384 ||
		strings.IndexFunc(normalized.APIKey, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, infraerrors.BadRequest("ACCOUNT_TEMPLATE_INPUT_INVALID", "provide a name of 1–100 characters and a single API key without whitespace")
	}
	return &normalized, nil
}

func supportsAccountKeyTemplate(source *Account) bool {
	if source.IsCredentialShadow() {
		return false
	}
	switch source.Type {
	case AccountTypeAPIKey, AccountTypeUpstream:
		return true
	case AccountTypeBedrock, AccountTypeServiceAccount:
		return source.GetCredential("auth_mode") == "apikey"
	default:
		return false
	}
}

func accountTemplateOperationKey(input *CreateAccountFromTemplateInput, operationKey string) string {
	if strings.TrimSpace(operationKey) == "" {
		return ""
	}
	// Bind durable recovery to the exact replacement identity, without storing the key.
	payload, _ := json.Marshal(input)
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("template:%x:%s", digest, operationKey)
}

func (s *adminServiceImpl) CreateAccountFromTemplate(ctx context.Context, id int64, input *CreateAccountFromTemplateInput, actorScope, operationKey string) (*Account, error) {
	normalized, err := normalizeAccountTemplateInput(input)
	if err != nil {
		return nil, err
	}
	return s.duplicateAccount(ctx, id, actorScope, accountTemplateOperationKey(normalized, operationKey), normalized)
}

func (s *adminServiceImpl) RecoverAccountFromTemplate(ctx context.Context, id int64, input *CreateAccountFromTemplateInput, actorScope, operationKey string) (*Account, error) {
	normalized, err := normalizeAccountTemplateInput(input)
	if err != nil {
		return nil, err
	}
	return s.RecoverDuplicateAccount(ctx, id, actorScope, accountTemplateOperationKey(normalized, operationKey))
}
