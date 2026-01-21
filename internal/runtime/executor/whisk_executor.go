package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	whiskInternal "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/whisk"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"golang.org/x/sync/singleflight"
)

const (
	// Standard Whisk models
	WhiskModelImagen35 = "IMAGEN_3_5"
	WhiskModelGemPix   = "GEM_PIX"
)

var (
	// Default aspect ratios
	AspectRatioLandscape = "IMAGE_ASPECT_RATIO_LANDSCAPE"
	AspectRatioPortrait  = "IMAGE_ASPECT_RATIO_PORTRAIT"
	AspectRatioSquare    = "IMAGE_ASPECT_RATIO_SQUARE"
)

// WhiskSessionManager manages the access token for a specific Whisk account session.
// It ensures thread-safe token refreshing using singleflight to prevent "thundering herd".
type WhiskSessionManager struct {
	mu           sync.RWMutex
	accessToken  string
	tokenExpiry  time.Time
	requestGroup singleflight.Group
}

func NewWhiskSessionManager() *WhiskSessionManager {
	return &WhiskSessionManager{}
}

// isTokenValid checks if the current access token is valid with a randomized jitter.
// Jitter (30-90s) is added to prevent synchronized expiration across multiple instances.
func (sm *WhiskSessionManager) isTokenValid() bool {
	// Safety margin = jitter (30-90s)
	jitter := 30 + rand.Intn(60)
	return sm.accessToken != "" && time.Now().Before(sm.tokenExpiry.Add(-time.Duration(jitter)*time.Second))
}

// GetAccessToken returns a valid access token, performing a refresh if necessary.
// It uses singleflight and double-checked locking to ensure only one refresh request is made per expiry.
func (sm *WhiskSessionManager) GetAccessToken(ctx context.Context, refresher func() (string, time.Time, error)) (string, error) {
	// 1. Fast path: Read lock check
	sm.mu.RLock()
	if sm.isTokenValid() {
		token := sm.accessToken
		sm.mu.RUnlock()
		return token, nil
	}
	sm.mu.RUnlock()

	// 2. Slow path: Singleflight with double-check
	// We use a constant key "refresh" because this manager instance is already scoped to a specific account
	val, err, _ := sm.requestGroup.Do("refresh", func() (interface{}, error) {
		// 2a. Double-check inside lock (mutex upgrade simulation)
		// Another goroutine might have finished refreshing while we were waiting for singleflight lock
		sm.mu.RLock()
		if sm.isTokenValid() {
			token := sm.accessToken
			sm.mu.RUnlock()
			return token, nil
		}
		sm.mu.RUnlock()

		// 2b. Perform actual refresh
		token, expiry, err := refresher()
		if err != nil {
			return nil, err
		}

		// 2c. Update state
		sm.mu.Lock()
		sm.accessToken = token
		sm.tokenExpiry = expiry
		sm.mu.Unlock()

		return token, nil
	})

	if err != nil {
		return "", err
	}
	return val.(string), nil
}

type WhiskExecutor struct {
	cfg *config.Config

	// sessionManagers maps account hash -> SessionManager
	// ensuring isolation between different Whisk accounts
	sessionManagers map[string]*WhiskSessionManager
	managersMu      sync.RWMutex
}

func NewWhiskExecutor(cfg *config.Config) *WhiskExecutor {
	return &WhiskExecutor{
		cfg:             cfg,
		sessionManagers: make(map[string]*WhiskSessionManager),
	}
}

// getSessionManager returns or creates a session manager for the given session token.
func (e *WhiskExecutor) getSessionManager(sessionToken string) *WhiskSessionManager {
	// Identify account by hash of its session token (the long-lived secret)
	hash := sha256.Sum256([]byte(sessionToken))
	key := hex.EncodeToString(hash[:])

	e.managersMu.RLock()
	if mgr, ok := e.sessionManagers[key]; ok {
		e.managersMu.RUnlock()
		return mgr
	}
	e.managersMu.RUnlock()

	e.managersMu.Lock()
	defer e.managersMu.Unlock()
	// Double-check after write lock
	if mgr, ok := e.sessionManagers[key]; ok {
		return mgr
	}

	mgr := NewWhiskSessionManager()
	e.sessionManagers[key] = mgr
	return mgr
}

// Identifier returns the provider key.
func (e *WhiskExecutor) Identifier() string {
	return "whisk"
}

func (e *WhiskExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("whisk executor does not support generic Execute (use specific image/video methods)")
}

func (e *WhiskExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	return nil, fmt.Errorf("whisk executor does not support streaming")
}

func (e *WhiskExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	// Trigger a token refresh via GetAccessToken logic
	token, err := e.getOrRefreshAuthToken(ctx, auth)
	if err != nil {
		return auth, err
	}

	// Update metadata atomically-ish (copy-on-write pattern for safety if needed, but here simple assignment)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["auth_token"] = token
	// Note: We don't have exact expiry here easily without returning it from GetAccessToken,
	// but the manager holds the truth. We update metadata just for visibility/serialization.
	auth.Metadata["auth_token_expires"] = time.Now().Add(55 * time.Minute).Unix() // Approximation

	// NEW: Update LastRefreshedAt for record keeping
	auth.LastRefreshedAt = time.Now()

	// NEW: Persist to file
	if err := e.saveAuthToFile(auth); err != nil {
		fmt.Printf("Warning: Failed to persist refreshed Whisk token: %v\n", err)
	}

	return auth, nil
}

// getOrRefreshAuthToken orchestrates the token acquisition using WhiskSessionManager
func (e *WhiskExecutor) getOrRefreshAuthToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	// 1. Check if we have a valid cached access token in metadata (especially for cookie-less JSON paste flows)
	if token, ok := auth.Metadata["auth_token"].(string); ok && token != "" {
		// Check expiry
		var expires int64
		if exp, ok := auth.Metadata["auth_token_expires"]; ok {
			switch v := exp.(type) {
			case float64:
				expires = int64(v)
			case int64:
				expires = v
			case int:
				expires = int64(v)
			}
		}

		// Fallback to bearer_token_expiry from file (RFC3339 string)
		if expires == 0 {
			if expStr, ok := auth.Metadata["bearer_token_expiry"].(string); ok && expStr != "" {
				if t, err := time.Parse(time.RFC3339, expStr); err == nil {
					expires = t.Unix()
				}
			}
		}

		// If we have an expiry and it's in the future (plus buffer), use it
		if expires > time.Now().Add(1*time.Minute).Unix() {
			return token, nil
		}

		// If expired and NO cookie, fail fast
		cookie := whiskFullCookie(auth)
		if cookie == "" {
			return "", fmt.Errorf("whisk: session expired and no cookie available to refresh")
		}
	}

	cookie := whiskFullCookie(auth)
	if cookie == "" {
		cookie = buildWhiskCookieFromToken(auth)
	}
	if cookie == "" {
		return "", fmt.Errorf("whisk: no cookie available for auth")
	}

	// Extract session token part for keying
	sessionToken := extractSessionToken(cookie)
	if sessionToken == "" {
		// Fallback: use whole cookie string as key if parsing fails (unlikely)
		sessionToken = cookie
	}

	manager := e.getSessionManager(sessionToken)

	// Define the refresh function that will be called if needed
	refresher := func() (string, time.Time, error) {
		return e.performDirectRefresh(ctx, cookie, auth)
	}

	token, err := manager.GetAccessToken(ctx, refresher)
	if err != nil {
		return "", err
	}

	// Update auth metadata with the valid token to ensure downstream handlers see it
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["auth_token"] = token
	// We should probably update the expiry here too if we got it from the manager?
	// The manager has it, but it doesn't return it in GetAccessToken (only returns token).
	// For now, this is acceptable as the manager handles the authoritative expiry state.

	return token, nil
}

// performDirectRefresh performs the actual HTTP call to Google's session endpoint
func (e *WhiskExecutor) performDirectRefresh(ctx context.Context, cookie string, auth *cliproxyauth.Auth) (string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://labs.google/fx/api/auth/session", nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("whisk refresh: %w", err)
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	// Necessary headers to look legit
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/115.0")
	req.Header.Set("Referer", "https://labs.google/fx/tools/whisk")
	req.Header.Set("Origin", "https://labs.google")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("whisk refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("whisk refresh failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var sessionResp struct {
		AccessToken string `json:"access_token"`
		Expires     string `json:"expires"`
		Error       string `json:"error"`
		User        struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(body, &sessionResp); err != nil {
		// Try to parse as raw generic map if specific struct fails, for debugging
		return "", time.Time{}, fmt.Errorf("whisk refresh parse: %w, body: %s", err, string(body))
	}

	if sessionResp.Error == "ACCESS_TOKEN_REFRESH_NEEDED" {
		return "", time.Time{}, fmt.Errorf("whisk: new cookie is required - session expired (ACCESS_TOKEN_REFRESH_NEEDED)")
	}
	if sessionResp.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("whisk: no access_token in session response: %s", string(body))
	}

	expiry, err := time.Parse(time.RFC3339, sessionResp.Expires)
	if err != nil {
		// If parse fails, assume 1 hour default
		expiry = time.Now().Add(1 * time.Hour)
	}

	// CRITICAL: Persist the expiry to auth metadata for file serialization
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["bearer_token_expiry"] = expiry.Format(time.RFC3339)
	auth.Metadata["auth_token"] = sessionResp.AccessToken
	auth.Metadata["auth_token_expires"] = expiry.Unix()

	// Update cookies in metadata if present to ensure rolling session
	if len(sessionResp.Cookies) > 0 {
		for _, c := range sessionResp.Cookies {
			if c.Name == "__Secure-next-auth.session-token" {
				auth.Metadata["session_token"] = c.Value
				// Also update legacy key if used
				auth.Metadata["sessionToken"] = c.Value
			}
			if c.Name == "__Host-next-auth.csrf-token" {
				auth.Metadata["csrf_token"] = c.Value
			}
		}
	}

	// NEW: Persist to file immediately after successful refresh
	if err := e.saveAuthToFile(auth); err != nil {
		fmt.Printf("Warning: Failed to persist Whisk token during direct refresh: %v\n", err)
	}

	return sessionResp.AccessToken, expiry, nil
}

// NEW: Helper to get auth file path
func (e *WhiskExecutor) getWhiskAuthFilePath(auth *cliproxyauth.Auth) (string, error) {
	if auth.FileName != "" {
		// If provided directly in auth record
		if filepath.IsAbs(auth.FileName) {
			return auth.FileName, nil
		}
		if e.cfg != nil && e.cfg.AuthDir != "" {
			return filepath.Join(e.cfg.AuthDir, auth.FileName), nil
		}
		// Fallback to current dir if no auth dir
		return auth.FileName, nil
	}

	// Construct if missing filename but have email
	email, _ := auth.Metadata["email"].(string)
	if email == "" {
		// Try to parse from filename if it was just loaded?
		return "", fmt.Errorf("no filename or email in auth to construct path")
	}

	// But without timestamp it is hard.
	// Ideally we use the ID if it matches?
	if strings.HasSuffix(auth.ID, ".json") {
		return filepath.Join(e.cfg.AuthDir, auth.ID), nil
	}

	return "", fmt.Errorf("cannot determine auth file path")
}

// NEW: Helper to save auth to file
func (e *WhiskExecutor) saveAuthToFile(auth *cliproxyauth.Auth) error {
	filePath, err := e.getWhiskAuthFilePath(auth)
	if err != nil {
		return err
	}

	// Map metadata back to WhiskTokenStorage structure
	sessionToken, _ := auth.Metadata["session_token"].(string)
	csrfToken, _ := auth.Metadata["csrf_token"].(string)
	bearerToken, _ := auth.Metadata["auth_token"].(string)
	bearerExpiry, _ := auth.Metadata["bearer_token_expiry"].(string)
	workflowID, _ := auth.Metadata["workflow_id"].(string)
	email, _ := auth.Metadata["email"].(string)

	ts := &whiskInternal.WhiskTokenStorage{
		SessionToken:      sessionToken,
		CsrfToken:         csrfToken,
		BearerToken:       bearerToken,
		BearerTokenExpiry: bearerExpiry,
		WorkflowID:        workflowID,
		Email:             email,
		Type:              "whisk",
	}

	return ts.SaveTokenToFile(filePath)
}

// Helpers

func extractSessionToken(cookieHeader string) string {
	parts := strings.Split(cookieHeader, ";")
	for _, part := range parts {
		check := strings.TrimSpace(part)
		if strings.HasPrefix(check, "__Secure-next-auth.session-token=") {
			return strings.TrimPrefix(check, "__Secure-next-auth.session-token=")
		}
	}
	return ""
}

func whiskFullCookie(auth *cliproxyauth.Auth) string {
	if auth.Metadata == nil {
		return ""
	}
	sessionToken, _ := auth.Metadata["session_token"].(string)
	csrfToken, _ := auth.Metadata["csrf_token"].(string)

	if sessionToken == "" {
		// Try alternate keys
		sessionToken, _ = auth.Metadata["sessionToken"].(string)
	}

	if sessionToken == "" {
		return ""
	}

	cookie := fmt.Sprintf("__Secure-next-auth.session-token=%s", sessionToken)
	if csrfToken != "" {
		cookie += fmt.Sprintf("; __Host-next-auth.csrf-token=%s", csrfToken)
	}
	return cookie
}

func buildWhiskCookieFromToken(auth *cliproxyauth.Auth) string {
	// Legacy or simpler method
	if auth.Runtime != nil {
		// Try to extract from runtime if possible
	}
	return whiskFullCookie(auth)
}

// --- Request/Response structs ---

type WhiskImageRequest struct {
	Prompt      string
	Model       string
	AspectRatio string
	NumImages   int
	Seed        int
}

type WhiskImageResponse struct {
	Data []WhiskImageData
}

type WhiskImageData struct {
	B64JSON string
	Prompt  string
}

// WhiskVideoRequest and WhiskVideoResponse removed - not implemented for GEM_PIX focus

// --- API Methods ---

func (e *WhiskExecutor) GenerateImage(ctx context.Context, auth *cliproxyauth.Auth, req WhiskImageRequest) (*WhiskImageResponse, error) {
	authToken, err := e.getOrRefreshAuthToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	sessionID := ";" + fmt.Sprint(time.Now().UnixMilli())
	if auth.Metadata != nil {
		if s, ok := auth.Metadata["session_id"].(string); ok && s != "" {
			sessionID = s
		}
	}

	model := req.Model
	if model == "" {
		model = "IMAGEN_3_1" // Default to 3.1 as per comfyui_whisk.py
	}
	aspectRatio := req.AspectRatio
	if aspectRatio == "" {
		aspectRatio = AspectRatioLandscape
	}

	// Payload construction matching v1:runImageFx
	userInput := map[string]any{
		"candidatesCount": 1,
		"prompts":         []string{req.Prompt},
		"seed":            req.Seed,
	}

	payload := map[string]any{
		"userInput": userInput,
		"clientContext": map[string]any{
			"sessionId": sessionID,
			"tool":      "BACKBONE",
		},
		"aspectRatio": aspectRatio,
		"modelInput": map[string]any{
			"modelNameType": model,
		},
	}

	payloadBytes, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://aisandbox-pa.googleapis.com/v1:runImageFx",
		bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+authToken)
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("whisk generate failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whisk generate error %d: %s", resp.StatusCode, string(body))
	}

	var respData struct {
		ImagePanels []struct {
			GeneratedImages []struct {
				EncodedImage string `json:"encodedImage"`
				Prompt       string `json:"prompt"`
			} `json:"generatedImages"`
			Prompt string `json:"prompt"`
		} `json:"imagePanels"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return nil, fmt.Errorf("whisk response decode: %w", err)
	}

	result := &WhiskImageResponse{}
	for _, panel := range respData.ImagePanels {
		for _, img := range panel.GeneratedImages {
			b64 := img.EncodedImage
			if strings.Contains(b64, ",") {
				b64 = strings.Split(b64, ",")[1]
			}
			result.Data = append(result.Data, WhiskImageData{
				B64JSON: b64,
				Prompt:  img.Prompt,
			})
		}
	}

	return result, nil
}

// UploadImage uploads an image to Whisk and returns the mediaGenerationId
func (e *WhiskExecutor) UploadImage(ctx context.Context, auth *cliproxyauth.Auth, imageB64, caption, workflowID string) (string, error) {
	authToken, err := e.getOrRefreshAuthToken(ctx, auth)
	if err != nil {
		return "", err
	}

	// Ensure imageB64 has the data prefix
	b64WithPrefix := imageB64
	if !strings.HasPrefix(b64WithPrefix, "data:image") {
		b64WithPrefix = "data:image/png;base64," + b64WithPrefix
	}

	sessionID := fmt.Sprintf(";%d", time.Now().UnixMilli())

	payload := map[string]any{
		"json": map[string]any{
			"clientContext": map[string]any{
				"workflowId": workflowID,
				"sessionId":  sessionID,
			},
			"uploadMediaInput": map[string]any{
				"mediaCategory": "MEDIA_CATEGORY_SCENE",
				"rawBytes":      b64WithPrefix,
				"caption":       caption,
			},
		},
	}

	payloadBytes, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://labs.google/fx/api/trpc/backbone.uploadImage",
		bytes.NewReader(payloadBytes))
	if err != nil {
		return "", err
	}

	cookie := whiskFullCookie(auth)
	httpReq.Header.Set("Authorization", "Bearer "+authToken)
	if cookie != "" {
		httpReq.Header.Set("Cookie", cookie)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("whisk uploadImage failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("whisk uploadImage error %d: %s", resp.StatusCode, string(body))
	}

	var trpcResp struct {
		Result struct {
			Data struct {
				JSON struct {
					Result struct {
						UploadMediaGenerationID string `json:"uploadMediaGenerationId"`
					} `json:"result"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&trpcResp); err != nil {
		return "", fmt.Errorf("whisk uploadImage decode: %w", err)
	}

	if trpcResp.Result.Data.JSON.Result.UploadMediaGenerationID == "" {
		return "", fmt.Errorf("whisk uploadImage: no uploadMediaGenerationId in response")
	}

	return trpcResp.Result.Data.JSON.Result.UploadMediaGenerationID, nil
}

func (e *WhiskExecutor) RefineImage(ctx context.Context, auth *cliproxyauth.Auth, imageB64, instruction, originalPrompt, model, aspectRatio string, enhanceContext bool) (*WhiskImageResponse, error) {
	authToken, err := e.getOrRefreshAuthToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	// Get or create workflowID from auth metadata or generate new
	workflowID := uuid.New().String()
	if auth.Metadata != nil {
		if w, ok := auth.Metadata["workflow_id"].(string); ok && w != "" {
			workflowID = w
		}
	}

	// Smart Refine: If enhanceContext is true, generate caption and inject it into instruction
	finalInstruction := instruction
	if enhanceContext {
		captions, err := e.GenerateCaption(ctx, auth, imageB64, 1)
		if err == nil && len(captions) > 0 && captions[0] != "" {
			finalInstruction = fmt.Sprintf("%s. [Context: %s]", instruction, captions[0])
			// Log for debugging/verification
			fmt.Printf("Smart Refine: Context injected. Caption: %s\n", captions[0])
		}
	}

	// CRITICAL: First upload the image to get the real mediaGenerationId
	if originalPrompt == "" {
		originalPrompt = "An image to be edited"
	}
	mediaID, err := e.UploadImage(ctx, auth, imageB64, originalPrompt, workflowID)
	if err != nil {
		return nil, fmt.Errorf("uploadImage failed: %w", err)
	}

	if model == "" || model == "gemini-2.5-flash-image" {
		model = WhiskModelGemPix
	}
	if aspectRatio == "" {
		aspectRatio = AspectRatioLandscape
	}

	// Ensure imageB64 has the data prefix for backbone.editImage
	b64WithPrefix := imageB64
	if !strings.HasPrefix(b64WithPrefix, "data:image") {
		b64WithPrefix = "data:image/png;base64," + b64WithPrefix
	}

	sessionID := fmt.Sprintf(";%d", time.Now().UnixMilli())

	// TRPC Payload with REAL mediaGenerationId from upload
	payload := map[string]any{
		"json": map[string]any{
			"clientContext": map[string]any{
				"workflowId": workflowID,
				"tool":       "BACKBONE",
				"sessionId":  sessionID,
			},
			"imageModelSettings": map[string]any{
				"imageModel":  model,
				"aspectRatio": aspectRatio,
			},
			"flags": map[string]any{},
			"editInput": map[string]any{
				"caption":                   originalPrompt,
				"userInstruction":           finalInstruction,
				"seed":                      nil,
				"safetyMode":                nil,
				"originalMediaGenerationId": mediaID,
				"mediaInput": map[string]any{
					"mediaCategory": "MEDIA_CATEGORY_BOARD",
					"rawBytes":      b64WithPrefix,
				},
			},
		},
		"meta": map[string]any{
			"values": map[string]any{
				"editInput.seed":       []string{"undefined"},
				"editInput.safetyMode": []string{"undefined"},
			},
		},
	}

	payloadBytes, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://labs.google/fx/api/trpc/backbone.editImage",
		bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}

	cookie := whiskFullCookie(auth)
	httpReq.Header.Set("Authorization", "Bearer "+authToken)
	if cookie != "" {
		httpReq.Header.Set("Cookie", cookie)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("whisk refine failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whisk refine error %d: %s", resp.StatusCode, string(body))
	}

	var trpcResp struct {
		Result struct {
			Data struct {
				JSON struct {
					Result struct {
						ImagePanels []struct {
							GeneratedImages []struct {
								EncodedImage string `json:"encodedImage"`
								Prompt       string `json:"prompt"`
							} `json:"generatedImages"`
						} `json:"imagePanels"`
					} `json:"result"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&trpcResp); err != nil {
		return nil, fmt.Errorf("whisk refine decode: %w", err)
	}

	result := &WhiskImageResponse{}
	panels := trpcResp.Result.Data.JSON.Result.ImagePanels
	if len(panels) > 0 {
		for _, img := range panels[0].GeneratedImages {
			b64 := img.EncodedImage
			if strings.Contains(b64, ",") {
				b64 = strings.Split(b64, ",")[1]
			}
			result.Data = append(result.Data, WhiskImageData{
				B64JSON: b64,
				Prompt:  img.Prompt,
			})
		}
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("whisk refine: no images in response")
	}

	return result, nil
}

func (e *WhiskExecutor) GenerateCaption(ctx context.Context, auth *cliproxyauth.Auth, imageB64 string, count int) ([]string, error) {
	authToken, err := e.getOrRefreshAuthToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	cookie := whiskFullCookie(auth)
	if cookie == "" {
		return nil, fmt.Errorf("whisk caption: no cookie available")
	}

	payload := map[string]any{
		"json": map[string]any{
			"imageBase64": "data:image/png;base64," + imageB64,
			"category":    "STYLE", // Default to STYLE as it often gives the most descriptive prompts
			"sessionId":   ";" + fmt.Sprint(time.Now().UnixMilli()),
		},
	}

	payloadBytes, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://labs.google/fx/api/trpc/backbone.generateCaption",
		bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+authToken)
	httpReq.Header.Set("Cookie", cookie)
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("whisk caption failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whisk caption error %d: %s", resp.StatusCode, string(body))
	}

	var trpcResp struct {
		Result struct {
			Data struct {
				JSON string `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&trpcResp); err != nil {
		return nil, fmt.Errorf("whisk caption decode: %w", err)
	}

	if trpcResp.Result.Data.JSON == "" {
		return nil, fmt.Errorf("whisk caption: empty response")
	}

	return []string{trpcResp.Result.Data.JSON}, nil
}

// GenerateVideo removed - focus on GEM_PIX editImage and caption only

func (e *WhiskExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Whisk image generation models do not expose token counting in the same way as LLMs.
	// Returning 0 or a stub value is appropriate here as this method is likely not used for image models.
	return cliproxyexecutor.Response{}, nil
}

func (e *WhiskExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	authToken, err := e.getOrRefreshAuthToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	client := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return client.Do(req)
}
