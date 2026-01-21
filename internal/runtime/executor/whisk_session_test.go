package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestWhiskSessionManager_Singleflight(t *testing.T) {
	manager := NewWhiskSessionManager()
	var refreshCalls int32

	// Refresher that sleeps to simulate network and counts calls
	refresher := func() (string, time.Time, error) {
		atomic.AddInt32(&refreshCalls, 1)
		time.Sleep(100 * time.Millisecond) // Simulate network delay
		return "new_access_token", time.Now().Add(1 * time.Hour), nil
	}

	// Launch 50 concurrent requests
	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := manager.GetAccessToken(ctx, refresher)
			if err != nil {
				t.Errorf("GetAccessToken failed: %v", err)
			}
			if token != "new_access_token" {
				t.Errorf("expected token 'new_access_token', got %s", token)
			}
		}()
	}

	wg.Wait()

	if atomic.LoadInt32(&refreshCalls) != 1 {
		t.Errorf("expected EXACTLY 1 refresh call, got %d", refreshCalls)
	}
}

func TestWhiskExecutor_MultiAccountIsolation(t *testing.T) {
	exec := NewWhiskExecutor(nil)

	// We can't easily mock private `getOrRefreshAuthToken` network call without changing structure significantly,
	// BUT we can test `getSessionManager` to verify isolation.

	mgr1 := exec.getSessionManager("token1")
	mgr2 := exec.getSessionManager("token2")
	mgr1Again := exec.getSessionManager("token1")

	if mgr1 == mgr2 {
		t.Error("expected different managers for different accounts")
	}
	if mgr1 != mgr1Again {
		t.Error("expected stable manager for same account")
	}

	// Manually inject state to verify they don't leak
	mgr1.mu.Lock()
	mgr1.accessToken = "SECRET_1"
	mgr1.tokenExpiry = time.Now().Add(1 * time.Hour)
	mgr1.mu.Unlock()

	mgr2.mu.Lock()
	mgr2.accessToken = "SECRET_2"
	mgr2.tokenExpiry = time.Now().Add(1 * time.Hour)
	mgr2.mu.Unlock()

	ctx := context.Background()
	refresherDummy := func() (string, time.Time, error) { return "fail", time.Now(), nil }

	tok1, _ := mgr1.GetAccessToken(ctx, refresherDummy)
	tok2, _ := mgr2.GetAccessToken(ctx, refresherDummy)

	if tok1 != "SECRET_1" {
		t.Errorf("Manager 1 leaked or corrupted: got %s", tok1)
	}
	if tok2 != "SECRET_2" {
		t.Errorf("Manager 2 leaked or corrupted: got %s", tok2)
	}
}

// Mock auth struct usage for context if needed, but not strictly required for this unit test
var _ = cliproxyauth.Auth{}

func TestWhiskSessionManager_JitterValidation(t *testing.T) {
	// This test is tricky because Jitter is random.
	// We verify that a token slightly before expiry is considered invalid/valid appropriately.
	// Since jitter is 30-90s, if we set expiry to Now + 10s, it MUST trigger refresh (because 10s < 30s jitter).

	manager := NewWhiskSessionManager()
	manager.mu.Lock()
	manager.accessToken = "old_token"
	manager.tokenExpiry = time.Now().Add(10 * time.Second) // Only 10s left
	manager.mu.Unlock()

	var refreshed bool
	refresher := func() (string, time.Time, error) {
		refreshed = true
		return "new_token", time.Now().Add(1 * time.Hour), nil
	}

	manager.GetAccessToken(context.Background(), refresher)

	if !refreshed {
		t.Error("expected refresh to trigger due to aggressive jitter margin (10s remaining < 30s jitter)")
	}
}
