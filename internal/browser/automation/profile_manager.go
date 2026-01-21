package automation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// ProfileInfo contains metadata about a Chrome profile for Whisk.
type ProfileInfo struct {
	Email     string    `json:"email"`
	Directory string    `json:"directory"`
	CreatedAt time.Time `json:"created_at"`
	LastLogin time.Time `json:"last_login"`
	Status    string    `json:"status"` // active, inactive
}

// ProfileIndex stores all Chrome profiles and default selection.
type ProfileIndex struct {
	Profiles []ProfileInfo `json:"profiles"`
	Default  string        `json:"default"` // directory name
}

// ProfileManager manages multiple Chrome profiles for different Whisk accounts.
type ProfileManager struct {
	baseDir   string // ~/.cli-proxy-api/chrome-profiles
	indexPath string // ~/.cli-proxy-api/chrome-profiles/.profile-index.json
}

// NewProfileManager creates a new profile manager instance.
func NewProfileManager() (*ProfileManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home dir: %w", err)
	}

	baseDir := filepath.Join(homeDir, ".cli-proxy-api", "chrome-profiles")
	indexPath := filepath.Join(baseDir, ".profile-index.json")

	pm := &ProfileManager{
		baseDir:   baseDir,
		indexPath: indexPath,
	}

	// Ensure base directory exists
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create profiles directory: %w", err)
	}

	// Migrate legacy chrome-data if it exists
	if err := pm.MigrateLegacyProfile(); err != nil {
		log.Warnf("Failed to migrate legacy profile: %v", err)
	}

	return pm, nil
}

// loadIndex loads the profile index from disk.
func (pm *ProfileManager) loadIndex() (*ProfileIndex, error) {
	if _, err := os.Stat(pm.indexPath); os.IsNotExist(err) {
		// Index doesn't exist yet, return empty
		return &ProfileIndex{
			Profiles: []ProfileInfo{},
			Default:  "",
		}, nil
	}

	data, err := os.ReadFile(pm.indexPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read index: %w", err)
	}

	var index ProfileIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse index: %w", err)
	}

	return &index, nil
}

// saveIndex saves the profile index to disk.
func (pm *ProfileManager) saveIndex(index *ProfileIndex) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal index: %w", err)
	}

	if err := os.WriteFile(pm.indexPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write index: %w", err)
	}

	return nil
}

// sanitizeEmail creates a safe directory name from an email address.
func sanitizeEmail(email string) string {
	// Remove invalid filesystem characters
	email = strings.ToLower(email)
	re := regexp.MustCompile(`[^a-z0-9@._-]`)
	email = re.ReplaceAllString(email, "_")
	return email
}

// GetProfilePath returns the Chrome user-data-dir path for a given email.
// If email is empty, returns the default profile.
func (pm *ProfileManager) GetProfilePath(email string) (string, error) {
	index, err := pm.loadIndex()
	if err != nil {
		return "", err
	}

	// If no email specified, use default
	if email == "" {
		if index.Default == "" {
			// No default set, create one
			return pm.GetOrCreateDefault()
		}
		return filepath.Join(pm.baseDir, index.Default), nil
	}

	// Find profile by email
	for _, p := range index.Profiles {
		if p.Email == email {
			return filepath.Join(pm.baseDir, p.Directory), nil
		}
	}

	return "", fmt.Errorf("profile not found for email: %s", email)
}

// CreateProfile creates a new Chrome profile for the given email.
func (pm *ProfileManager) CreateProfile(email string) (string, error) {
	if email == "" {
		return "", fmt.Errorf("email cannot be empty")
	}

	// Basic email validation
	if !strings.Contains(email, "@") {
		return "", fmt.Errorf("invalid email format: %s", email)
	}

	index, err := pm.loadIndex()
	if err != nil {
		return "", err
	}

	// Check if profile already exists
	for _, p := range index.Profiles {
		if p.Email == email {
			return "", fmt.Errorf("profile already exists for email: %s", email)
		}
	}

	// Create directory name
	dirName := "profile-" + sanitizeEmail(email)
	profilePath := filepath.Join(pm.baseDir, dirName)

	// Create profile directory
	if err := os.MkdirAll(profilePath, 0700); err != nil {
		return "", fmt.Errorf("failed to create profile directory: %w", err)
	}

	// Add to index
	now := time.Now()
	profile := ProfileInfo{
		Email:     email,
		Directory: dirName,
		CreatedAt: now,
		LastLogin: now,
		Status:    "active",
	}

	index.Profiles = append(index.Profiles, profile)

	// Set as default if it's the first profile
	if len(index.Profiles) == 1 {
		index.Default = dirName
	}

	if err := pm.saveIndex(index); err != nil {
		return "", err
	}

	log.Infof("Created new Chrome profile for %s at %s", email, profilePath)
	return profilePath, nil
}

// ListProfiles returns all registered profiles.
func (pm *ProfileManager) ListProfiles() ([]ProfileInfo, error) {
	index, err := pm.loadIndex()
	if err != nil {
		return nil, err
	}

	return index.Profiles, nil
}

// DeleteProfile removes a profile by email.
func (pm *ProfileManager) DeleteProfile(email string) error {
	index, err := pm.loadIndex()
	if err != nil {
		return err
	}

	// Find and remove profile
	found := false
	var newProfiles []ProfileInfo
	var deletedDir string

	for _, p := range index.Profiles {
		if p.Email == email {
			found = true
			deletedDir = p.Directory
		} else {
			newProfiles = append(newProfiles, p)
		}
	}

	if !found {
		return fmt.Errorf("profile not found for email: %s", email)
	}

	// Update default if we're deleting it
	if index.Default == deletedDir {
		if len(newProfiles) > 0 {
			index.Default = newProfiles[0].Directory
		} else {
			index.Default = ""
		}
	}

	index.Profiles = newProfiles

	// Save updated index
	if err := pm.saveIndex(index); err != nil {
		return err
	}

	// Remove profile directory
	profilePath := filepath.Join(pm.baseDir, deletedDir)
	if err := os.RemoveAll(profilePath); err != nil {
		log.Warnf("Failed to remove profile directory %s: %v", profilePath, err)
	}

	log.Infof("Deleted profile for %s", email)
	return nil
}

// GetOrCreateDefault returns the default profile path, creating it if necessary.
func (pm *ProfileManager) GetOrCreateDefault() (string, error) {
	index, err := pm.loadIndex()
	if err != nil {
		return "", err
	}

	// Check if default exists
	if index.Default != "" {
		defaultPath := filepath.Join(pm.baseDir, index.Default)
		if _, err := os.Stat(defaultPath); err == nil {
			return defaultPath, nil
		}
	}

	// Create default profile
	defaultDir := "default"
	defaultPath := filepath.Join(pm.baseDir, defaultDir)

	if err := os.MkdirAll(defaultPath, 0700); err != nil {
		return "", fmt.Errorf("failed to create default profile: %w", err)
	}

	// Add to index
	now := time.Now()
	profile := ProfileInfo{
		Email:     "default",
		Directory: defaultDir,
		CreatedAt: now,
		LastLogin: now,
		Status:    "active",
	}

	// Add or replace default
	found := false
	for i, p := range index.Profiles {
		if p.Directory == defaultDir {
			index.Profiles[i] = profile
			found = true
			break
		}
	}

	if !found {
		index.Profiles = append(index.Profiles, profile)
	}

	index.Default = defaultDir

	if err := pm.saveIndex(index); err != nil {
		return "", err
	}

	return defaultPath, nil
}

// MigrateLegacyProfile migrates old ~/.cli-proxy-api/chrome-data to new structure.
func (pm *ProfileManager) MigrateLegacyProfile() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	legacyPath := filepath.Join(homeDir, ".cli-proxy-api", "chrome-data")

	// Check if legacy profile exists
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return nil // No migration needed
	}

	// Check if it's already a symlink (already migrated)
	if info, err := os.Lstat(legacyPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // Already migrated
		}
	}

	defaultPath := filepath.Join(pm.baseDir, "default")

	// Check if default already exists
	if _, err := os.Stat(defaultPath); err == nil {
		// Default exists, don't overwrite
		log.Warnf("Default profile already exists, skipping migration")
		return nil
	}

	// Move legacy to default
	log.Infof("Migrating legacy chrome-data to chrome-profiles/default")
	if err := os.Rename(legacyPath, defaultPath); err != nil {
		return fmt.Errorf("failed to move legacy profile: %w", err)
	}

	// Create symlink for backwards compatibility
	if err := os.Symlink(defaultPath, legacyPath); err != nil {
		log.Warnf("Failed to create symlink chrome-data -> default: %v", err)
	}

	// Update index
	index, err := pm.loadIndex()
	if err != nil {
		return err
	}

	now := time.Now()
	profile := ProfileInfo{
		Email:     "default",
		Directory: "default",
		CreatedAt: now,
		LastLogin: now,
		Status:    "active",
	}

	index.Profiles = append(index.Profiles, profile)
	index.Default = "default"

	if err := pm.saveIndex(index); err != nil {
		return err
	}

	log.Info("Migration completed successfully")
	return nil
}

// UpdateLastLogin updates the last login timestamp for a profile.
func (pm *ProfileManager) UpdateLastLogin(email string) error {
	index, err := pm.loadIndex()
	if err != nil {
		return err
	}

	for i, p := range index.Profiles {
		if p.Email == email {
			index.Profiles[i].LastLogin = time.Now()
			return pm.saveIndex(index)
		}
	}

	return nil
}
