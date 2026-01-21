package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

// Cookie represents a browser cookie from CDP.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	Secure   bool    `json:"secure"`
	HttpOnly bool    `json:"httpOnly"`
}

// FlowResult contains the extracted tokens.
type FlowResult struct {
	SessionToken string
	CsrfToken    string
}

// TargetInfo represents a browser target (page, worker, etc).
type TargetInfo struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Flow launches a browser, waits for the user to login, and extracts tokens.
// If headless is true, runs Chrome in headless mode (no GUI window).
// If profileDir is specified, uses that directory as Chrome user-data-dir.
func Flow(ctx context.Context, url string, headless bool, profileDir string) (*FlowResult, error) {
	chromePath := findChrome()
	if chromePath == "" {
		return nil, fmt.Errorf("chrome not found")
	}

	var userDataDir string

	// Use specified profile directory or create default
	if profileDir != "" {
		userDataDir = profileDir
	} else {
		// Legacy: use old chrome-data location
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get user home dir: %w", err)
		}
		userDataDir = filepath.Join(homeDir, ".cli-proxy-api", "chrome-data")
	}

	// Ensure directory exists
	if err := os.MkdirAll(userDataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create user data dir: %w", err)
	}

	// Do NOT remove userDataDir on exit to persist login state
	// defer os.RemoveAll(userDataDir)

	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		"--disable-infobars",
		"--excludeSwitches=enable-automation",
		"--user-agent=Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}

	// Add headless mode flags if requested
	if headless {
		args = append(args,
			"--headless=new",
			"--disable-gpu",
			"--no-sandbox",
			"--disable-dev-shm-usage",
		)
		log.Info("Starting Chrome in headless mode")
	}

	args = append(args, url)

	cmd := exec.CommandContext(ctx, chromePath, args...)

	// Remove stale DevToolsActivePort file before starting (may exist from previous session)
	portFile := filepath.Join(userDataDir, "DevToolsActivePort")
	_ = os.Remove(portFile) // Ignore error if doesn't exist

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start chrome: %w", err)
	}
	// Wait for DevToolsActivePort file to appear
	var port string

	// Try for 15 seconds to read the port
	for i := 0; i < 30; i++ {
		time.Sleep(500 * time.Millisecond)
		data, err := os.ReadFile(portFile)
		if err == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 0 {
				port = lines[0]
				break
			}
		}
	}
	if port == "" {
		// Kill the process since we failed to connect
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, fmt.Errorf("failed to get remote debugging port")
	}

	// Ensure graceful cleanup now that we have the port
	defer func() {
		// Attempt graceful close via CDP first
		if err := closeBrowserGracefully(port); err != nil {
			log.Warnf("Graceful browser close failed: %v, killing process", err)
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		} else {
			// Give it a moment to shut down
			time.Sleep(1 * time.Second)
			if cmd.Process != nil {
				_ = cmd.Process.Kill() // Ensure it's really dead
			}
		}
	}()

	log.Infof("Chrome started on port %s", port)

	// Wait loop for cookies
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			res, err := checkCookies(port)
			if err != nil {
				log.Debugf("Cookie check error: %v", err)
				continue
			}
			if res != nil {
				return res, nil
			}
		}
	}
}

// checkCookies finds the correct page target and asks for cookies
func checkCookies(port string) (*FlowResult, error) {
	// 1. List targets
	targets, err := listTargets(port)
	if err != nil {
		return nil, err
	}

	// 2. Find the Whisk/Google page
	var wsURL string
	for _, t := range targets {
		if t.Type == "page" && (strings.Contains(t.URL, "labs.google") || strings.Contains(t.URL, "accounts.google")) {
			if t.WebSocketDebuggerURL != "" {
				wsURL = t.WebSocketDebuggerURL
				break
			}
		}
	}
	// Fallback to any page if specific one not found yet (maybe redirecting)
	if wsURL == "" {
		for _, t := range targets {
			if t.Type == "page" && t.WebSocketDebuggerURL != "" {
				wsURL = t.WebSocketDebuggerURL
				break
			}
		}
	}

	if wsURL == "" {
		return nil, fmt.Errorf("no suitable page target found - waiting for user to navigate to labs.google")
	}

	// 3. Connect to page
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	defer conn.Close()

	// 4. First, get cookies to check if user is logged in
	cookieReq := map[string]interface{}{
		"id":     1,
		"method": "Network.getCookies",
		"params": map[string]interface{}{
			"urls": []string{"https://labs.google.com", "https://labs.google", "https://accounts.google.com"},
		},
	}
	if err := conn.WriteJSON(cookieReq); err != nil {
		return nil, err
	}

	var session, csrf string
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var resp struct {
			Id     int `json:"id"`
			Result struct {
				Cookies []Cookie `json:"cookies"`
			} `json:"result"`
		}
		if json.Unmarshal(message, &resp) == nil && resp.Id == 1 {
			for _, c := range resp.Result.Cookies {
				if c.Name == "__Secure-next-auth.session-token" {
					session = c.Value
				}
				if c.Name == "__Host-next-auth.csrf-token" && csrf == "" {
					csrf = c.Value
				}
			}
			break
		}
	}

	// If we don't have session token yet, return nil (user needs to log in)
	if session == "" {
		return nil, nil
	}

	// 5. Verify session is valid by checking /fx/api/auth/session endpoint
	log.Info("Session token found, verifying with /fx/api/auth/session...")

	// Build Cookie header
	cookieHeader := fmt.Sprintf("__Secure-next-auth.session-token=%s", session)
	if csrf != "" {
		cookieHeader += fmt.Sprintf("; __Host-next-auth.csrf-token=%s", csrf)
	}

	// Check session endpoint
	httpClient := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", "https://labs.google/fx/api/auth/session", nil)
	if err != nil {
		log.Warnf("Failed to create session check request: %v", err)
		// Continue anyway, cookie might still work
		return &FlowResult{SessionToken: session, CsrfToken: csrf}, nil
	}

	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warnf("Failed to check session endpoint: %v", err)
		// Continue anyway
		return &FlowResult{SessionToken: session, CsrfToken: csrf}, nil
	}
	// Read and parse body
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Warnf("Failed to read session response: %v", err)
		return nil, nil
	}

	if resp.StatusCode != 200 {
		log.Warnf("Session endpoint returned status %d - waiting...", resp.StatusCode)
		return nil, nil
	}

	// Check for semantic errors in JSON (e.g. ACCESS_TOKEN_REFRESH_NEEDED)
	var sessionResp struct {
		Error   string `json:"error"`
		Expires string `json:"expires"`
	}
	if err := json.Unmarshal(body, &sessionResp); err == nil {
		if sessionResp.Error != "" {
			log.Warnf("Session endpoint returned semantic error: %s - waiting...", sessionResp.Error)
			return nil, nil // Continue waiting
		}

		if sessionResp.Expires != "" {
			expiry, err := time.Parse(time.RFC3339, sessionResp.Expires)
			if err == nil {
				// Ensure token is valid for at least 5 minutes
				if time.Now().Add(5 * time.Minute).After(expiry) {
					log.Warnf("Session token is expired or expires too soon (%s) - waiting...", sessionResp.Expires)
					return nil, nil
				}
			}
		}
	}

	log.Info("✓ Session verified successfully!")
	return &FlowResult{SessionToken: session, CsrfToken: csrf}, nil
}

func listTargets(port string) ([]TargetInfo, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/json/list", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var targets []TargetInfo
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func findChrome() string {
	if runtime.GOOS == "darwin" {
		paths := []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	} else if runtime.GOOS == "linux" {
		bins := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
		for _, b := range bins {
			if path, err := exec.LookPath(b); err == nil {
				return path
			}
		}
	} else if runtime.GOOS == "windows" {
		paths := []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			os.Getenv("LOCALAPPDATA") + `\Google\Chrome\Application\chrome.exe`,
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func getWebSocketDebuggerURL(port string) (string, error) {
	// Not used in new flow
	return "", nil
}

// closeBrowserGracefully attempts to close the browser via CDP Browser.close command
func closeBrowserGracefully(port string) error {
	var wsURL string
	// Find specific type "browser" target if available, or just connect to the first page to send Browser.close?
	// Actually Browser.close is a browser-level command. We need the browser target specifically?
	// No, often we can send it to any target if we use Target.closeTarget?
	// Better: Use /json/version to get the webSocketDebuggerUrl for the BROWSER itself.

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/json/version", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var version struct {
		WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return err
	}
	wsURL = version.WebSocketDebuggerUrl

	if wsURL == "" {
		return fmt.Errorf("no browser websocket url found")
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := map[string]interface{}{
		"id":     999,
		"method": "Browser.close",
	}
	return conn.WriteJSON(req)
}
