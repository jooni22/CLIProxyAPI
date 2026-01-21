package whisk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
)

// WhiskTokenStorage persists Whisk authentication credentials.
// Supports both Session Cookie (labs.google) and Bearer Token (aisandbox-pa).
type WhiskTokenStorage struct {
	// SessionToken is the __Secure-next-auth.session-token cookie value
	SessionToken string `json:"session_token"`

	// CsrfToken is the optional __Host-next-auth.csrf-token cookie value
	CsrfToken string `json:"csrf_token,omitempty"`

	// BearerToken is the OAuth2 access token for aisandbox-pa.googleapis.com
	BearerToken string `json:"bearer_token,omitempty"`

	// BearerTokenExpiry is the RFC3339 formatted expiry time for bearer token
	BearerTokenExpiry string `json:"bearer_token_expiry,omitempty"`

	// WorkflowID is the current workflow UUID for the session
	WorkflowID string `json:"workflow_id,omitempty"`

	// Email is the authenticated user's email
	Email string `json:"email,omitempty"`

	// Type is always "whisk" for this storage
	Type string `json:"type"`
}

// SaveTokenToFile serializes the token storage to disk.
func (ts *WhiskTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "whisk"
	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("whisk token: create directory failed: %w", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("whisk token: create file failed: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err = json.NewEncoder(f).Encode(ts); err != nil {
		return fmt.Errorf("whisk token: encode token failed: %w", err)
	}
	return nil
}

// LoadTokenFromFile deserializes the token storage from disk.
func LoadTokenFromFile(authFilePath string) (*WhiskTokenStorage, error) {
	f, err := os.Open(authFilePath)
	if err != nil {
		return nil, fmt.Errorf("whisk token: open file failed: %w", err)
	}
	defer func() { _ = f.Close() }()

	var ts WhiskTokenStorage
	if err = json.NewDecoder(f).Decode(&ts); err != nil {
		return nil, fmt.Errorf("whisk token: decode token failed: %w", err)
	}
	return &ts, nil
}
