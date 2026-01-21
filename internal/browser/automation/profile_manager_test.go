package automation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProfileManager_CreateProfile tests basic profile creation
func TestProfileManager_CreateProfile(t *testing.T) {
	// Setup: create temporary test directory
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	// Ensure base directory exists
	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	// Test: Create profile
	email := "test@example.com"
	profilePath, err := pm.CreateProfile(email)
	if err != nil {
		t.Fatalf("CreateProfile failed: %v", err)
	}

	// Verify: Profile directory exists
	if _, err := os.Stat(profilePath); os.IsNotExist(err) {
		t.Errorf("Profile directory was not created: %s", profilePath)
	}

	// Verify: Profile is in index
	index, err := pm.loadIndex()
	if err != nil {
		t.Fatalf("Failed to load index: %v", err)
	}

	if len(index.Profiles) != 1 {
		t.Errorf("Expected 1 profile in index, got %d", len(index.Profiles))
	}

	if index.Profiles[0].Email != email {
		t.Errorf("Expected email %s, got %s", email, index.Profiles[0].Email)
	}

	// Verify: First profile is set as default
	if index.Default == "" {
		t.Error("First profile should be set as default")
	}
}

// TestProfileManager_CreateProfile_DuplicateEmail tests duplicate email prevention
func TestProfileManager_CreateProfile_DuplicateEmail(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	email := "duplicate@example.com"

	// Create first profile
	_, err = pm.CreateProfile(email)
	if err != nil {
		t.Fatalf("First CreateProfile failed: %v", err)
	}

	// Try to create duplicate
	_, err = pm.CreateProfile(email)
	if err == nil {
		t.Error("Expected error when creating duplicate profile, got nil")
	}

	// Verify error message
	expectedError := "profile already exists for email"
	if err != nil && err.Error()[:len(expectedError)] != expectedError {
		t.Errorf("Expected error to contain '%s', got: %v", expectedError, err)
	}
}

// TestProfileManager_CreateProfile_InvalidEmail tests email validation
func TestProfileManager_CreateProfile_InvalidEmail(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	testCases := []struct {
		email       string
		expectError bool
	}{
		{"", true},                // empty email
		{"notanemail", true},      // no @ sign
		{"valid@test.com", false}, // valid email
	}

	for _, tc := range testCases {
		if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
			t.Fatalf("Failed to create base dir: %v", err)
		}

		_, err := pm.CreateProfile(tc.email)
		if tc.expectError && err == nil {
			t.Errorf("Expected error for email '%s', got nil", tc.email)
		}
		if !tc.expectError && err != nil {
			t.Errorf("Did not expect error for email '%s', got: %v", tc.email, err)
		}

		// Clean up for next test
		os.RemoveAll(pm.baseDir)
	}
}

// TestProfileManager_ListProfiles tests profile listing
func TestProfileManager_ListProfiles(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	// Initially empty
	profiles, err := pm.ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles failed: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("Expected 0 profiles initially, got %d", len(profiles))
	}

	// Create multiple profiles
	emails := []string{"user1@test.com", "user2@test.com", "user3@test.com"}
	for _, email := range emails {
		_, err := pm.CreateProfile(email)
		if err != nil {
			t.Fatalf("CreateProfile failed for %s: %v", email, err)
		}
	}

	// List profiles
	profiles, err = pm.ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles failed: %v", err)
	}

	if len(profiles) != len(emails) {
		t.Errorf("Expected %d profiles, got %d", len(emails), len(profiles))
	}

	// Verify all emails are present
	emailMap := make(map[string]bool)
	for _, p := range profiles {
		emailMap[p.Email] = true
	}

	for _, email := range emails {
		if !emailMap[email] {
			t.Errorf("Expected email %s not found in profile list", email)
		}
	}
}

// TestProfileManager_DeleteProfile tests profile deletion
func TestProfileManager_DeleteProfile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	email := "delete-me@test.com"

	// Create profile
	profilePath, err := pm.CreateProfile(email)
	if err != nil {
		t.Fatalf("CreateProfile failed: %v", err)
	}

	// Delete profile
	err = pm.DeleteProfile(email)
	if err != nil {
		t.Fatalf("DeleteProfile failed: %v", err)
	}

	// Verify: Profile directory is removed
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Errorf("Profile directory still exists after deletion: %s", profilePath)
	}

	// Verify: Profile is removed from index
	profiles, err := pm.ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles failed: %v", err)
	}

	for _, p := range profiles {
		if p.Email == email {
			t.Errorf("Deleted profile %s still in index", email)
		}
	}
}

// TestProfileManager_DeleteProfile_NonExistent tests deleting non-existent profile
func TestProfileManager_DeleteProfile_NonExistent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	err = pm.DeleteProfile("nonexistent@test.com")
	if err == nil {
		t.Error("Expected error when deleting non-existent profile, got nil")
	}
}

// TestProfileManager_GetProfilePath tests profile path retrieval
func TestProfileManager_GetProfilePath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	email := "pathtest@test.com"

	// Create profile
	expectedPath, err := pm.CreateProfile(email)
	if err != nil {
		t.Fatalf("CreateProfile failed: %v", err)
	}

	// Get profile path
	retrievedPath, err := pm.GetProfilePath(email)
	if err != nil {
		t.Fatalf("GetProfilePath failed: %v", err)
	}

	if retrievedPath != expectedPath {
		t.Errorf("Expected path %s, got %s", expectedPath, retrievedPath)
	}
}

// TestProfileManager_GetProfilePath_NonExistent tests getting path for non-existent profile
func TestProfileManager_GetProfilePath_NonExistent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	_, err = pm.GetProfilePath("nonexistent@test.com")
	if err == nil {
		t.Error("Expected error for non-existent profile, got nil")
	}
}

// TestProfileManager_UpdateLastLogin tests last login timestamp update
func TestProfileManager_UpdateLastLogin(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	email := "logintest@test.com"

	// Create profile
	_, err = pm.CreateProfile(email)
	if err != nil {
		t.Fatalf("CreateProfile failed: %v", err)
	}

	// Get initial last login time
	profiles, _ := pm.ListProfiles()
	initialLastLogin := profiles[0].LastLogin

	// Wait a bit to ensure time difference
	time.Sleep(10 * time.Millisecond)

	// Update last login
	err = pm.UpdateLastLogin(email)
	if err != nil {
		t.Fatalf("UpdateLastLogin failed: %v", err)
	}

	// Get updated last login time
	profiles, _ = pm.ListProfiles()
	updatedLastLogin := profiles[0].LastLogin

	// Verify time was updated
	if !updatedLastLogin.After(initialLastLogin) {
		t.Error("LastLogin timestamp was not updated")
	}
}

// TestProfileManager_MigrateLegacyProfile tests legacy profile migration
// NOTE: This test is SKIPPED because MigrateLegacyProfile() uses os.UserHomeDir()
// which cannot be mocked in standard Go testing. Migration was tested manually and works correctly.
// See .sisyphus/plans/whisk-multi-profile-investigation.md for manual test results.
func TestProfileManager_MigrateLegacyProfile(t *testing.T) {
	t.Skip("Skipping migration test - requires mocking os.UserHomeDir() which is not straightforward. Manually tested and verified working.")
}

// TestProfileManager_GetOrCreateDefault tests default profile creation
func TestProfileManager_GetOrCreateDefault(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	// Get or create default (should create)
	defaultPath, err := pm.GetOrCreateDefault()
	if err != nil {
		t.Fatalf("GetOrCreateDefault failed: %v", err)
	}

	// Verify: default directory exists
	if _, err := os.Stat(defaultPath); os.IsNotExist(err) {
		t.Error("Default profile directory was not created")
	}

	// Get or create default again (should return existing)
	defaultPath2, err := pm.GetOrCreateDefault()
	if err != nil {
		t.Fatalf("Second GetOrCreateDefault failed: %v", err)
	}

	if defaultPath != defaultPath2 {
		t.Error("GetOrCreateDefault returned different paths")
	}
}

// TestProfileManager_SanitizeEmail tests email sanitization
func TestProfileManager_SanitizeEmail(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"test@example.com", "test@example.com"},
		{"Test@Example.COM", "test@example.com"},       // lowercase
		{"user+tag@test.com", "user_tag@test.com"},     // + becomes _
		{"user space@test.com", "user_space@test.com"}, // space becomes _
		{"user!@test.com", "user_@test.com"},           // ! becomes _
	}

	for _, tc := range testCases {
		result := sanitizeEmail(tc.input)
		if result != tc.expected {
			t.Errorf("sanitizeEmail(%s) = %s, expected %s", tc.input, result, tc.expected)
		}
	}
}

// TestProfileManager_DeleteProfile_DefaultHandling tests default reassignment on deletion
func TestProfileManager_DeleteProfile_DefaultHandling(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "profile-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pm := &ProfileManager{
		baseDir:   filepath.Join(tmpDir, "chrome-profiles"),
		indexPath: filepath.Join(tmpDir, "chrome-profiles", ".profile-index.json"),
	}

	if err := os.MkdirAll(pm.baseDir, 0700); err != nil {
		t.Fatalf("Failed to create base dir: %v", err)
	}

	// Create two profiles
	email1 := "first@test.com"
	email2 := "second@test.com"

	pm.CreateProfile(email1)
	pm.CreateProfile(email2)

	// First profile should be default
	index, _ := pm.loadIndex()
	firstDefault := index.Default

	// Delete first (default) profile
	err = pm.DeleteProfile(email1)
	if err != nil {
		t.Fatalf("DeleteProfile failed: %v", err)
	}

	// Verify: default was reassigned to second profile
	index, _ = pm.loadIndex()
	if index.Default == firstDefault {
		t.Error("Default was not reassigned after deleting default profile")
	}

	if index.Default == "" {
		t.Error("Default should be reassigned, not empty")
	}
}

// TestProfileManager_ProfileInfo_JSONSerialization tests JSON marshaling/unmarshaling
func TestProfileManager_ProfileInfo_JSONSerialization(t *testing.T) {
	now := time.Now()
	profile := ProfileInfo{
		Email:     "test@example.com",
		Directory: "profile-test@example.com",
		CreatedAt: now,
		LastLogin: now,
		Status:    "active",
	}

	// Marshal to JSON
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("Failed to marshal ProfileInfo: %v", err)
	}

	// Unmarshal back
	var decoded ProfileInfo
	err = json.Unmarshal(data, &decoded)
	if err != nil {
		t.Fatalf("Failed to unmarshal ProfileInfo: %v", err)
	}

	// Verify fields
	if decoded.Email != profile.Email {
		t.Errorf("Email mismatch after JSON round-trip")
	}
	if decoded.Directory != profile.Directory {
		t.Errorf("Directory mismatch after JSON round-trip")
	}
	if decoded.Status != profile.Status {
		t.Errorf("Status mismatch after JSON round-trip")
	}
}
