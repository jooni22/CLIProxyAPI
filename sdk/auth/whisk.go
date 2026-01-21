package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/whisk"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser/automation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// WhiskAuthenticator implements the Authenticator interface for Whisk (Google Labs).
type WhiskAuthenticator struct{}

// NewWhiskAuthenticator returns a new instance.
func NewWhiskAuthenticator() Authenticator {
	return &WhiskAuthenticator{}
}

func (WhiskAuthenticator) Provider() string {
	return "whisk"
}

// RefreshLead returns nil as Whisk manages its own keep-alive via requests,
// or we can set a dummy value. The manager uses this to pre-emptively refresh.
func (WhiskAuthenticator) RefreshLead() *time.Duration {
	// 24 hours lead time for 30 day cookie?
	d := 24 * time.Hour
	return &d
}

func (wa WhiskAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("whisk auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	var sessionToken, csrfToken string
	var err error

	// Check if headless mode is requested
	headless := false
	if opts.Metadata != nil {
		if val, ok := opts.Metadata["headless"]; ok && val == "true" {
			headless = true
		}
	}

	// Get profile directory if specified
	profileDir := ""
	if opts.Metadata != nil {
		if val, ok := opts.Metadata["profile_dir"]; ok && val != "" {
			profileDir = val
		}
	}

	// 1. Try Automated Flow if not disabled
	if !opts.NoBrowser {
		if headless {
			fmt.Println("Attempting automated headless browser login...")
			fmt.Println("Chrome will run in headless mode (no GUI window).")
		} else {
			fmt.Println("Attempting automated browser login...")
			fmt.Println("A Chrome window will open. Please log in to Google/Whisk.")
		}

		targetURL := "https://labs.google/fx/tools/whisk"
		res, errFlow := automation.Flow(ctx, targetURL, headless, profileDir)
		if errFlow == nil && res != nil {
			log.Info("Successfully extracted Whisk tokens from browser!")
			sessionToken = res.SessionToken
			csrfToken = res.CsrfToken
		} else {
			log.Warnf("Automated login failed: %v. Falling back to manual mode.", errFlow)
		}
	}

	// 2. Manual Fallback
	if sessionToken == "" {
		fmt.Println("\n=== Whisk Manual Login Required ===")
		fmt.Println("1. Open https://labs.google/fx/tools/whisk in your browser")
		fmt.Println("2. Open Developer Tools (F12) -> Application -> Cookies")
		fmt.Println("3. Find cookies for labs.google")
		fmt.Println("4. Paste the value of '__Secure-next-auth.session-token' OR the entire 'Cookie' header string")

		promptFn := opts.Prompt
		if promptFn == nil {
			return nil, fmt.Errorf("interaction required but no prompt function provided")
		}

		input, errPrompt := promptFn("Paste Token or Cookie String: ")
		if errPrompt != nil {
			return nil, errPrompt
		}

		sessionToken, csrfToken = parseWhiskInput(input)
		if sessionToken == "" {
			return nil, fmt.Errorf("could not find session token in input")
		}
	}

	// 3. Validate
	fmt.Println("Validating session...")
	whiskInternal := whisk.NewWhiskAuth(cfg)
	// Pass "unknown" initially; AuthenticateWithCookie will attempt to fetch it via API.
	tokenData, err := whiskInternal.AuthenticateWithCookie(ctx, sessionToken, csrfToken, "unknown")
	if err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	// 4. Construct Auth Record
	// We need to match what whisk_executor expects in Metadata.
	// whisk_executor expects: session_token, csrf_token, bearer_token, bearer_token_expiry, auth_token_expires

	now := time.Now()

	// Parse expiry to int64 for auth_token_expires
	var expiresUnix int64
	if t, err := time.Parse(time.RFC3339, tokenData.Expire); err == nil {
		expiresUnix = t.Unix()
	} else {
		// Fallback if parse fails
		expiresUnix = now.Add(1 * time.Hour).Unix()
	}

	metadata := map[string]interface{}{
		"type":                "whisk",
		"email":               tokenData.Email,
		"session_token":       tokenData.SessionToken,
		"csrf_token":          tokenData.CsrfToken,
		"workflow_id":         tokenData.WorkflowID,
		"bearer_token":        tokenData.BearerToken, // Add this
		"bearer_token_expiry": tokenData.Expire,      // Add this (RFC3339)
		"auth_token":          tokenData.BearerToken, // Add this (alias for convenience)
		"auth_token_expires":  expiresUnix,           // Add this (int64)
		"created_at":          now.UnixMilli(),
	}

	// Correct filename generation
	sanitizedEmail := whisk.SanitizeWhiskFileName(tokenData.Email)
	fileName := fmt.Sprintf("whisk-%s-%d.json", sanitizedEmail, now.Unix())

	return &coreauth.Auth{
		ID:       fileName,
		Provider: "whisk",
		FileName: fileName,
		Label:    tokenData.Email,
		Metadata: metadata,
	}, nil
}

func parseWhiskInput(input string) (session, csrf string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}

	// Heuristic: if it looks like a long specific token (JWT-like or just random chars), assume it's the session token directly.
	// Session tokens usually don't have equals signs or semicolons in the middle if they are bare,
	// BUT header strings DO.
	if !strings.Contains(input, "=") && !strings.Contains(input, ";") {
		return input, ""
	}

	// Parse as cookie string
	parts := strings.Split(input, ";")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])

		if key == "__Secure-next-auth.session-token" {
			session = val
		} else if key == "__Host-next-auth.csrf-token" {
			csrf = val
		}
	}

	// If parsing failed but input has no semicolons, maybe it was a raw token that just happened to have an equals sign?
	// unlikely for these specific tokens.

	return
}
