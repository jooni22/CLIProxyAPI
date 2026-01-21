package whisk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	labsGoogleBaseURL = "https://labs.google/fx/api/trpc"
	aisandboxBaseURL  = "https://aisandbox-pa.googleapis.com/v1"

	// Endpoints
	createWorkflowEndpoint = "media.createOrUpdateWorkflow"
)

// WhiskAuth encapsulates the HTTP client helpers for Whisk authentication.
type WhiskAuth struct {
	httpClient *http.Client
}

// NewWhiskAuth constructs a new WhiskAuth with proxy-aware transport.
func NewWhiskAuth(cfg *config.Config) *WhiskAuth {
	client := &http.Client{Timeout: 60 * time.Second}
	return &WhiskAuth{httpClient: util.SetProxy(&cfg.SDKConfig, client)}
}

// WhiskTokenData holds the authenticated token information.
type WhiskTokenData struct {
	SessionToken string
	CsrfToken    string
	BearerToken  string
	WorkflowID   string
	Email        string
	Expire       string
}

// FullSessionInfo contains all data from session endpoint
type FullSessionInfo struct {
	Email       string
	AccessToken string
	Expires     string // RFC3339
}

// AuthenticateWithCookie authenticates using session cookie from browser.
func (wa *WhiskAuth) AuthenticateWithCookie(ctx context.Context, sessionToken, csrfToken, email string) (*WhiskTokenData, error) {
	log.Debug("AuthenticateWithCookie: starting validation")

	// Normalize cookie string if needed
	sessionToken, err := NormalizeSessionCookie(sessionToken)
	if err != nil {
		return nil, fmt.Errorf("whisk cookie authentication: %w", err)
	}

	var accessToken string
	var accessExpire string

	// Always fetch full user info to get the proper Bearer Token and Expiry from the session
	log.Debug("AuthenticateWithCookie: fetching full session info...")
	info, err := wa.FetchUserInfo(ctx, sessionToken, csrfToken)
	if err != nil {
		log.Warnf("Failed to fetch user info: %v", err)
		// If we don't have an email, we can't proceed safely
		if email == "" || email == "unknown" {
			return nil, fmt.Errorf("failed to identify user from session: %w", err)
		}
	} else {
		log.Debugf("AuthenticateWithCookie: success, email=%s", info.Email)
		if email == "" || email == "unknown" {
			email = info.Email
		}
		accessToken = info.AccessToken
		accessExpire = info.Expires
	}

	// Validate the session by creating (or trying to create) a workflow
	// This confirms the session is truly valid for API operations
	log.Debug("AuthenticateWithCookie: validating with API request...")
	workflowID := uuid.New().String()
	if err := wa.validateSession(ctx, sessionToken, csrfToken, workflowID); err != nil {
		// We log this as a warning because sometimes session access is fine but specific APIs fail
		// However, for initial login, this is a strong signal of failure.
		log.Warnf("whisk cookie authentication: session validation request failed: %v", err)
	} else {
		log.Debug("AuthenticateWithCookie: API validation successful")
	}

	// Use the expiry from the API if available, otherwise default to a reasonable fallback for the Bearer token
	// Note: The SessionToken itself is usually valid for much longer (30 days)
	if accessExpire == "" {
		accessExpire = time.Now().Add(1 * time.Hour).Format(time.RFC3339)
	}

	return &WhiskTokenData{
		SessionToken: sessionToken,
		CsrfToken:    csrfToken,
		WorkflowID:   workflowID,
		Email:        email,
		BearerToken:  accessToken,
		Expire:       accessExpire,
	}, nil
}

// validateSession validates the session by making a test request.
func (wa *WhiskAuth) validateSession(ctx context.Context, sessionToken, csrfToken, workflowID string) error {
	// sessionId format: ;{timestamp_ms} (with leading semicolon)
	sessionID := fmt.Sprintf(";%d", time.Now().UnixMilli())

	payload := map[string]any{
		"json": map[string]any{
			"clientContext": map[string]any{
				"tool":      "BACKBONE",
				"sessionId": sessionID,
			},
			"mediaGenerationIdsToCopy": []any{},
			"workflowMetadata": map[string]any{
				"workflowName": "CLIProxyAPI Whisk Project",
			},
		},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		labsGoogleBaseURL+createWorkflowEndpoint,
		strings.NewReader(string(payloadBytes)))
	if err != nil {
		return fmt.Errorf("create request failed: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", BuildCookieHeader(sessionToken, csrfToken))

	resp, err := wa.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		log.Debugf("whisk session validation failed: status=%d body=%s", resp.StatusCode, string(body))
		return fmt.Errorf("session validation failed with status %d", resp.StatusCode)
	}

	log.Debug("Whisk session validated successfully")
	return nil
}

// FetchUserInfo attempts to retrieve the user's email, access token and expiry from the session endpoint.
func (wa *WhiskAuth) FetchUserInfo(ctx context.Context, sessionToken, csrfToken string) (*FullSessionInfo, error) {
	// Try the fx-prefixed endpoint first as it matches the other API paths
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://labs.google/fx/api/auth/session", nil)
	if err != nil {
		return nil, err
	}

	// Manually build cookie header to ensure correct format
	req.Header.Set("Cookie", BuildCookieHeader(sessionToken, csrfToken))
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	// Use client with timeout
	resp, err := wa.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		AccessToken string `json:"access_token"`
		Expires     string `json:"expires"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if data.Error != "" {
		return nil, fmt.Errorf("session api returned error: %s", data.Error)
	}

	if data.Expires != "" {
		expiry, err := time.Parse(time.RFC3339, data.Expires)
		if err == nil {
			if time.Now().After(expiry) {
				return nil, fmt.Errorf("retrieved token is already expired (expires: %s)", data.Expires)
			}
		}
	}

	if data.User.Email == "" {
		return nil, fmt.Errorf("empty email in response")
	}

	return &FullSessionInfo{
		Email:       data.User.Email,
		AccessToken: data.AccessToken,
		Expires:     data.Expires,
	}, nil
}

// CreateCookieTokenStorage creates token storage from validated token data.
func (wa *WhiskAuth) CreateCookieTokenStorage(data *WhiskTokenData) *WhiskTokenStorage {
	return &WhiskTokenStorage{
		SessionToken:      data.SessionToken,
		CsrfToken:         data.CsrfToken,
		WorkflowID:        data.WorkflowID,
		Email:             data.Email,
		Type:              "whisk",
		BearerToken:       data.BearerToken, // Now using the token from API
		BearerTokenExpiry: data.Expire,
	}
}

// BuildCookieHeader constructs the Cookie header value
func BuildCookieHeader(sessionToken, csrfToken string) string {
	var sb strings.Builder
	sb.WriteString("__Secure-next-auth.session-token=")
	sb.WriteString(sessionToken)
	if csrfToken != "" {
		sb.WriteString("; __Host-next-auth.csrf-token=")
		sb.WriteString(csrfToken)
	}
	return sb.String()
}

// NormalizeSessionCookie ensures the session token is clean
func NormalizeSessionCookie(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("empty session token")
	}
	return token, nil
}

// SanitizeWhiskFileName is a helper to clean email for filenames
func SanitizeWhiskFileName(email string) string {
	return strings.ReplaceAll(email, "@", "_at_")
}
