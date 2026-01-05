package unit

import (
	"testing"

	"github.com/lukscryptwalker-csi/pkg/driver"
	"github.com/stretchr/testify/assert"
)

// TestGetLocalPath tests the GetLocalPath utility function
func TestGetLocalPath(t *testing.T) {
	tests := []struct {
		name     string
		volumeID string
		expected string
	}{
		{
			name:     "standard volume ID",
			volumeID: "pvc-123e4567-e89b-12d3-a456-426614174000",
			expected: "/opt/local-path-provisioner/pvc-123e4567-e89b-12d3-a456-426614174000",
		},
		{
			name:     "simple volume ID",
			volumeID: "test-volume",
			expected: "/opt/local-path-provisioner/test-volume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := driver.GetLocalPath(tt.volumeID)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestGenerateBackingFilePath tests the GenerateBackingFilePath utility function
func TestGenerateBackingFilePath(t *testing.T) {
	tests := []struct {
		name      string
		localPath string
		volumeID  string
		expected  string
	}{
		{
			name:      "standard paths",
			localPath: "/opt/local-path-provisioner/test-vol",
			volumeID:  "test-vol",
			expected:  "/opt/local-path-provisioner/test-vol/luks-test-vol.img",
		},
		{
			name:      "uuid volume ID",
			localPath: "/opt/local-path-provisioner/pvc-abc123",
			volumeID:  "pvc-abc123",
			expected:  "/opt/local-path-provisioner/pvc-abc123/luks-pvc-abc123.img",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := driver.GenerateBackingFilePath(tt.localPath, tt.volumeID)
			assert.Equal(t, tt.expected, result)
		})
	}
}