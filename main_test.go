package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

type mockClient struct {
	whoisLogin  string
	statusLogin string
	whoisErr    error
	statusErr   error
}

func (m *mockClient) WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
	if m.whoisErr != nil {
		return nil, m.whoisErr
	}
	return &apitype.WhoIsResponse{
		UserProfile: &tailcfg.UserProfile{LoginName: m.whoisLogin},
	}, nil
}

func (m *mockClient) Status(ctx context.Context) (*ipnstate.Status, error) {
	if m.statusErr != nil {
		return nil, m.statusErr
	}
	selfID := tailcfg.UserID(1)
	return &ipnstate.Status{
		Self: &ipnstate.PeerStatus{UserID: selfID},
		User: map[tailcfg.UserID]tailcfg.UserProfile{
			selfID: {LoginName: m.statusLogin},
		},
	}, nil
}

// TestFormatBytes ensures our UI displays sizes correctly
func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"}, // 1.5 * 1024
		{1048576, "1.0 MB"},
	}

	for _, tt := range tests {
		result := formatBytes(tt.input)
		if result != tt.expected {
			t.Errorf("formatBytes(%d): expected %s, got %s", tt.input, tt.expected, result)
		}
	}
}

// TestGetSafeFilename verifies the auto-rename logic (test.bin -> test-1.bin)
// This test creates real temporary files to ensure os.Stat logic works.
func TestGetSafeFilename(t *testing.T) {
	// Create a temp directory for this test so we don't mess up your real files
	tmpDir := t.TempDir()

	// Helper to make full paths
	path := func(name string) string {
		return filepath.Join(tmpDir, name)
	}

	// Case 1: File doesn't exist yet. Should return original name.
	original := path("data.txt")
	safe := getSafeFilename(original)
	if safe != original {
		t.Errorf("Expected '%s', got '%s'", original, safe)
	}

	// Case 2: Create the file, then ask again. Should get data-1.txt
	os.Create(original) // Touch the file
	safe = getSafeFilename(original)
	expected := path("data-1.txt")
	if safe != expected {
		t.Errorf("Expected '%s', got '%s'", expected, safe)
	}

	// Case 3: Create data-1.txt too. Should get data-2.txt
	os.Create(expected) // Touch data-1.txt
	safe = getSafeFilename(original)
	expected2 := path("data-2.txt")
	if safe != expected2 {
		t.Errorf("Expected '%s', got '%s'", expected2, safe)
	}
}

func TestRegisterHandlersHTMLGet(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "hello.txt")
	content := []byte("hello world")
	if err := os.WriteFile(filePath, content, 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	fileName := filepath.Base(filePath)
	fileSize := formatBytes(int64(len(content)))
	targetUser := "target@example.com"

	shutdownSignal := make(chan string, 1)
	secretPath := "/secret"
	ackPath := secretPath + "/ack"

	mux := http.NewServeMux()
	registerHandlers(mux, &mockClient{whoisLogin: targetUser, statusLogin: "sender@example.com"},
		targetUser, filePath, fileName, fileSize, shutdownSignal, secretPath, ackPath)

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + secretPath)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Secure Drop") {
		t.Fatalf("expected HTML to contain title")
	}
	if !strings.Contains(bodyStr, fileName) || !strings.Contains(bodyStr, fileSize) {
		t.Fatalf("expected HTML to contain file name and size")
	}
	if !strings.Contains(bodyStr, "File Burned") {
		t.Fatalf("expected HTML to contain done state")
	}
}

func TestRegisterHandlersSmartClientDownload(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "hello.txt")
	content := []byte("hello world")
	if err := os.WriteFile(filePath, content, 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	fileName := filepath.Base(filePath)
	fileSize := formatBytes(int64(len(content)))
	targetUser := "target@example.com"

	shutdownSignal := make(chan string, 1)
	secretPath := "/secret"
	ackPath := secretPath + "/ack"

	mux := http.NewServeMux()
	registerHandlers(mux, &mockClient{whoisLogin: targetUser, statusLogin: "sender@example.com"},
		targetUser, filePath, fileName, fileSize, shutdownSignal, secretPath, ackPath)

	server := httptest.NewServer(mux)
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL+secretPath, nil)
	req.Header.Set("X-Tail-Burn-Client", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, content) {
		t.Fatalf("expected body %q, got %q", string(content), string(body))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), fileName) {
		t.Fatalf("expected Content-Disposition to include filename")
	}

	select {
	case <-shutdownSignal:
		t.Fatalf("unexpected shutdown signal for smart client download")
	default:
	}

	// Ack should mark the link as burned for smart clients.
	ackResp, err := http.Post(server.URL+ackPath, "text/plain", nil)
	if err != nil {
		t.Fatalf("ACK failed: %v", err)
	}
	ackResp.Body.Close()

	// Second request should be rejected after ACK.
	req2, _ := http.NewRequest("GET", server.URL+secretPath, nil)
	req2.Header.Set("X-Tail-Burn-Client", "true")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second GET failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusGone {
		t.Fatalf("expected 410 after single-use, got %d", resp2.StatusCode)
	}
}

func TestRegisterHandlersBrowserBurnedHTML(t *testing.T) {
	oldDelay := browserShutdownDelay
	browserShutdownDelay = 10 * time.Millisecond
	defer func() {
		browserShutdownDelay = oldDelay
	}()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "hello.txt")
	content := []byte("hello world")
	if err := os.WriteFile(filePath, content, 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	fileName := filepath.Base(filePath)
	fileSize := formatBytes(int64(len(content)))
	targetUser := "target@example.com"

	shutdownSignal := make(chan string, 1)
	secretPath := "/secret"
	ackPath := secretPath + "/ack"

	mux := http.NewServeMux()
	registerHandlers(mux, &mockClient{whoisLogin: targetUser, statusLogin: "sender@example.com"},
		targetUser, filePath, fileName, fileSize, shutdownSignal, secretPath, ackPath)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Browser POST to download.
	resp, err := http.Post(server.URL+secretPath, "application/octet-stream", nil)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	// Browser GET after use should return burned HTML.
	resp2, err := http.Get(server.URL + secretPath)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusGone {
		t.Fatalf("expected 410, got %d", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "Link Burned") {
		t.Fatalf("expected burned HTML body")
	}
}

func TestRegisterHandlersAckShutdown(t *testing.T) {
	shutdownSignal := make(chan string, 1)
	secretPath := "/secret"
	ackPath := secretPath + "/ack"

	mux := http.NewServeMux()
	registerHandlers(mux, &mockClient{whoisLogin: "target@example.com", statusLogin: "sender@example.com"},
		"target@example.com", "unused", "unused", "0 B", shutdownSignal, secretPath, ackPath)

	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Post(server.URL+ackPath, "text/plain", nil)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	select {
	case <-shutdownSignal:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected shutdown signal after ACK")
	}
}

func TestRegisterHandlersForbiddenUser(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "hello.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	shutdownSignal := make(chan string, 1)
	secretPath := "/secret"
	ackPath := secretPath + "/ack"

	mux := http.NewServeMux()
	registerHandlers(mux, &mockClient{whoisLogin: "other@example.com", statusLogin: "sender@example.com"},
		"target@example.com", filePath, "hello.txt", "5 B", shutdownSignal, secretPath, ackPath)

	server := httptest.NewServer(mux)
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL+secretPath, nil)
	req.Header.Set("X-Tail-Burn-Client", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestReceiveContentLengthMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ack" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\"test.bin\"")
		w.Header().Set("Content-Length", "10")
		fmt.Fprint(w, "12345")
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(oldWD)

	if err := receive(server.URL); err == nil {
		t.Fatalf("expected error for short body, got nil")
	} else if !strings.Contains(err.Error(), "download incomplete") && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("expected short body error, got %v", err)
	}
}
