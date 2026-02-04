package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
