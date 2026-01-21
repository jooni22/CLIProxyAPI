package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser/automation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoWhiskLogin triggers the login flow for Whisk and saves tokens.
// headless controls whether Chrome runs in headless mode (no GUI).
// profileEmail specifies which Chrome profile to use (empty = default).
// newProfile indicates whether to create a new profile.
func DoWhiskLogin(cfg *config.Config, options *LoginOptions, headless bool, profileEmail string, newProfile bool) {
	if options == nil {
		options = &LoginOptions{}
	}

	// Initialize ProfileManager
	profileMgr, err := automation.NewProfileManager()
	if err != nil {
		log.Errorf("Failed to initialize ProfileManager: %v", err)
		return
	}

	// Handle --whisk-new-profile
	if newProfile {
		email := promptEmail()
		if email == "" {
			log.Error("Email cannot be empty")
			return
		}

		profilePath, err := profileMgr.CreateProfile(email)
		if err != nil {
			log.Errorf("Failed to create profile: %v", err)
			return
		}

		fmt.Printf("✓ Created new profile for %s\n", email)
		fmt.Printf("  Profile path: %s\n", profilePath)
		profileEmail = email
	}

	// Get profile path
	var profilePath string
	if profileEmail != "" {
		profilePath, err = profileMgr.GetProfilePath(profileEmail)
		if err != nil {
			log.Errorf("Failed to get profile path: %v", err)
			return
		}
		fmt.Printf("Using profile: %s\n", profileEmail)
	} else {
		profilePath, err = profileMgr.GetOrCreateDefault()
		if err != nil {
			log.Errorf("Failed to get default profile: %v", err)
			return
		}
		fmt.Println("Using default profile")
	}

	promptFn := options.Prompt
	if promptFn == nil {
		promptFn = defaultProjectPrompt()
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata: map[string]string{
			"headless":    fmt.Sprintf("%v", headless),
			"profile_dir": profilePath,
		},
		Prompt: promptFn,
	}

	record, savedPath, err := manager.Login(context.Background(), "whisk", cfg, authOpts)
	if err != nil {
		log.Errorf("Whisk authentication failed: %v", err)
		return
	}

	// Update last login timestamp
	if profileEmail != "" {
		_ = profileMgr.UpdateLastLogin(profileEmail)
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Whisk authentication successful!")
}

// DoWhiskListProfiles lists all Whisk Chrome profiles.
func DoWhiskListProfiles(cfg *config.Config) {
	profileMgr, err := automation.NewProfileManager()
	if err != nil {
		log.Errorf("Failed to initialize ProfileManager: %v", err)
		return
	}

	profiles, err := profileMgr.ListProfiles()
	if err != nil {
		log.Errorf("Failed to list profiles: %v", err)
		return
	}

	if len(profiles) == 0 {
		fmt.Println("No Whisk profiles found.")
		fmt.Println("Create one with: --whisk-new-profile")
		return
	}

	fmt.Println("\nAvailable Whisk profiles:")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	for i, p := range profiles {
		isDefault := ""
		if p.Directory == "default" {
			isDefault = " (default)"
		}

		fmt.Printf("%d. %s%s\n", i+1, p.Email, isDefault)
		fmt.Printf("   Last login: %s\n", p.LastLogin.Format("2006-01-02 15:04"))
		fmt.Printf("   Status: %s\n", p.Status)
		if i < len(profiles)-1 {
			fmt.Println()
		}
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("\nUsage:")
	fmt.Println("  Login with profile:  --whisk-login --whisk-profile=EMAIL")
	fmt.Println("  Create new profile:  --whisk-new-profile")
	fmt.Println("  Delete profile:      --whisk-delete-profile=EMAIL")
}

// DoWhiskDeleteProfile deletes a Whisk Chrome profile.
func DoWhiskDeleteProfile(cfg *config.Config, email string) {
	if email == "" {
		log.Error("Email cannot be empty")
		return
	}

	profileMgr, err := automation.NewProfileManager()
	if err != nil {
		log.Errorf("Failed to initialize ProfileManager: %v", err)
		return
	}

	// Confirm deletion
	fmt.Printf("⚠️  Delete profile for %s?\n", email)
	fmt.Print("This will remove the Chrome profile directory (y/N): ")

	reader := bufio.NewReader(os.Stdin)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))

	if response != "y" && response != "yes" {
		fmt.Println("Cancelled.")
		return
	}

	if err := profileMgr.DeleteProfile(email); err != nil {
		log.Errorf("Failed to delete profile: %v", err)
		return
	}

	fmt.Printf("✓ Profile deleted for %s\n", email)
	fmt.Println("Note: Auth files (whisk-*.json) are NOT deleted automatically.")
}

// promptEmail prompts the user to enter an email address.
func promptEmail() string {
	fmt.Print("Enter email for new profile: ")
	reader := bufio.NewReader(os.Stdin)
	email, _ := reader.ReadString('\n')
	return strings.TrimSpace(email)
}
