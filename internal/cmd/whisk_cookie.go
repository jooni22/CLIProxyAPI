package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/whisk"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// WhiskSessionResponse matches the JSON structure from /fx/api/auth/session
type WhiskSessionResponse struct {
	User struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
	Expires     string `json:"expires"`
	AccessToken string `json:"access_token"`
}

func DoWhiskPasteJsonLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	promptFn := options.Prompt
	if promptFn == nil {
		reader := bufio.NewReader(os.Stdin)
		promptFn = func(prompt string) (string, error) {
			fmt.Print(prompt)
			value, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(value), nil
		}
	}

	fmt.Println("=== Whisk Remote JSON Login ===")
	fmt.Println("1. Log in to https://labs.google/fx/tools/whisk in your browser")
	fmt.Println("2. Visit https://labs.google/fx/api/auth/session")
	fmt.Println("3. Copy the entire JSON output")
	fmt.Println("")
	fmt.Println("NOTE: This session will expire in ~1 hour because it lacks the refresh cookie.")
	fmt.Println("To enable auto-refresh, please use --whisk-login or --whisk-cookie instead.")
	fmt.Println("")

	fmt.Println("Paste the JSON below and press Enter:")
	// Reading valid JSON might span multiple lines if pasted formatted.
	// Since bufio.Reader.ReadString reads until delimiter, we might need a better way.
	// But usually users paste a single line or we can ask for single line.
	// Alternatively, we can read until EOF or look for closing brace?
	// Let's just ask the user to paste it as a single line or handle simply.
	// Actually, prompts usually handle single line.
	// Let's use a loop or assume single line paste. Most terminals paste with newlines.
	// Let's read until we see a valid JSON or timeout?
	// Simpler: Just prompt "Paste JSON (ensure it is on one line if possible, or paste and press Enter):"

	// Actually, ReadString('\n') stops at first newline. JSON pretty printed has newlines.
	// Better approach: ReadAll from stdin if piped, or loop read until valid JSON?
	// Let's stick to simple single line for now, or just read everything until a specific sentinel?
	// Let's try reading a few lines.

	var sb strings.Builder
	fmt.Println("(End input with an empty line)")
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		sb.WriteString(line)
	}
	input := sb.String()

	// Clean input to find JSON start/end?
	// Just unmarshal directly.

	var session WhiskSessionResponse
	if err := json.Unmarshal([]byte(input), &session); err != nil {
		fmt.Printf("Failed to parse JSON: %v\n", err)
		return
	}

	if session.AccessToken == "" {
		fmt.Println("Error: JSON does not contain access_token")
		return
	}
	if session.User.Email == "" {
		fmt.Println("Error: JSON does not contain user email")
		return
	}

	expiryTime, err := time.Parse(time.RFC3339, session.Expires)
	if err != nil {
		fmt.Printf("Warning: Failed to parse expiry time: %v. Defaulting to 1 hour.\n", err)
		expiryTime = time.Now().Add(1 * time.Hour)
	}

	// Create Auth Record
	now := time.Now()
	metadata := map[string]interface{}{
		"type":               "whisk",
		"email":              session.User.Email,
		"auth_token":         session.AccessToken,
		"auth_token_expires": expiryTime.Unix(),
		"session_token":      "", // No persistent cookie
		"csrf_token":         "",
		"workflow_id":        uuid.New().String(),
		"created_at":         now.UnixMilli(),
	}

	sanitizedEmail := whisk.SanitizeWhiskFileName(session.User.Email)
	fileName := fmt.Sprintf("whisk-%s-%d.json", sanitizedEmail, now.Unix())

	// we aren't using tokenStorage.SaveTokenToFile because we want to save full metadata including access_token
	// and WhiskTokenStorage usually only saves specific fields.

	authFilePath := fmt.Sprintf("%s/%s", cfg.AuthDir, fileName)

	fileBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		fmt.Printf("Failed to marshal auth data: %v\n", err)
		return
	}

	if err := os.WriteFile(authFilePath, fileBytes, 0600); err != nil {
		fmt.Printf("Failed to save auth file: %v\n", err)
		return
	}

	fmt.Printf("Whisk remote login successful (Temporary Session)!\n")
	fmt.Printf("Expires: %s\n", session.Expires)
	fmt.Printf("Saved to: %s\n", authFilePath)
}

func DoWhiskCookieAuth(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	promptFn := options.Prompt
	if promptFn == nil {
		reader := bufio.NewReader(os.Stdin)
		promptFn = func(prompt string) (string, error) {
			fmt.Print(prompt)
			value, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(value), nil
		}
	}

	fmt.Println("=== Whisk Cookie Authentication ===")
	fmt.Println("1. Go to https://labs.google/ and log in with your Google account")
	fmt.Println("2. Open DevTools (F12) -> Application -> Cookies")
	fmt.Println("3. Copy the value of '__Secure-next-auth.session-token'")
	fmt.Println("")

	sessionToken, err := promptFn("Enter __Secure-next-auth.session-token: ")
	if err != nil {
		fmt.Printf("Failed to get session token: %v\n", err)
		return
	}

	csrfToken, err := promptFn("Enter __Host-next-auth.csrf-token (optional, press Enter to skip): ")
	if err != nil {
		fmt.Printf("Failed to get CSRF token: %v\n", err)
		return
	}

	auth := whisk.NewWhiskAuth(cfg)
	ctx := context.Background()

	tokenData, err := auth.AuthenticateWithCookie(ctx, sessionToken, csrfToken, "unknown")
	if err != nil {
		fmt.Printf("Whisk cookie authentication failed: %v\n", err)
		return
	}

	tokenStorage := auth.CreateCookieTokenStorage(tokenData)

	fileName := whisk.SanitizeWhiskFileName(tokenData.Email)
	authFilePath := fmt.Sprintf("%s/whisk-%s-%d.json", cfg.AuthDir, fileName, time.Now().Unix())

	if err := tokenStorage.SaveTokenToFile(authFilePath); err != nil {
		fmt.Printf("Failed to save authentication: %v\n", err)
		return
	}

	fmt.Printf("Whisk authentication successful!\n")
	fmt.Printf("Workflow ID: %s\n", tokenData.WorkflowID)
	fmt.Printf("Authentication saved to: %s\n", authFilePath)
}
