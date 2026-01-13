package rclone

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/rclone/rclone/librclone/librclone"
	"k8s.io/klog"

	// Import rclone backends we need
	_ "github.com/rclone/rclone/backend/crypt"
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/s3"

	// Import mount command to register mount/mount RPC
	_ "github.com/rclone/rclone/cmd/mount"
	_ "github.com/rclone/rclone/cmd/cmount"
)

var (
	initOnce sync.Once
	initDone bool
	initMu   sync.Mutex
)

// Initialize initializes librclone (call once at startup)
func Initialize() error {
	initMu.Lock()
	defer initMu.Unlock()

	if initDone {
		return nil
	}

	initOnce.Do(func() {
		librclone.Initialize()
		klog.Info("librclone initialized successfully")
	})

	initDone = true
	return nil
}


// Finalize cleans up librclone resources
func Finalize() {
	initMu.Lock()
	defer initMu.Unlock()

	if initDone {
		librclone.Finalize()
		initDone = false
		klog.Info("librclone finalized")
	}
}

// RPCResult represents the result of an RPC call
type RPCResult struct {
	Output map[string]interface{}
	Status int
	Error  error
}

// RPC executes an rclone RPC method with JSON input
func RPC(method string, params interface{}) (*RPCResult, error) {
	// Ensure librclone is initialized
	if err := Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize librclone: %w", err)
	}

	inputJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal RPC params: %w", err)
	}

	klog.V(5).Infof("RPC call: %s", method)

	output, status := librclone.RPC(method, string(inputJSON))

	result := &RPCResult{Status: status}
	if output != "" {
		if err := json.Unmarshal([]byte(output), &result.Output); err != nil {
			// Some outputs may not be JSON, store as raw string in a special key
			klog.V(5).Infof("RPC output: non-JSON response received")
			result.Output = map[string]interface{}{"_raw": output}
		}
	}

	if status >= 400 {
		errMsg := ""
		if result.Output != nil {
			if errField, ok := result.Output["error"].(string); ok {
				errMsg = errField
			} else {
				errMsg = fmt.Sprintf("%v", result.Output)
			}
		}
		result.Error = fmt.Errorf("RPC %s failed with status %d: %s", method, status, errMsg)
		klog.Errorf("RPC %s failed: status=%d, error=%s", method, status, errMsg)
		return result, result.Error
	}

	klog.V(5).Infof("RPC %s succeeded with status %d", method, status)
	return result, nil
}

// RPCWithRaw executes an RPC and returns raw string output
func RPCWithRaw(method string, params interface{}) (string, int, error) {
	// Ensure librclone is initialized
	if err := Initialize(); err != nil {
		return "", 0, fmt.Errorf("failed to initialize librclone: %w", err)
	}

	inputJSON, err := json.Marshal(params)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal RPC params: %w", err)
	}

	output, status := librclone.RPC(method, string(inputJSON))

	if status >= 400 {
		return output, status, fmt.Errorf("RPC %s failed with status %d: %s", method, status, output)
	}

	return output, status, nil
}

// DeleteVolumeData deletes all data for a volume from S3
// s3PathPrefix is optional - if empty, defaults to "volumes/{volumeID}"
// If s3PathPrefix is provided, path becomes "{s3PathPrefix}/volumes/{volumeID}"
func DeleteVolumeData(s3Config *S3Config, volumeID string, s3PathPrefix string) error {
	if err := Initialize(); err != nil {
		return fmt.Errorf("failed to initialize librclone: %w", err)
	}

	// Build S3 remote path for the volume
	// Always include volumeID to ensure each volume has its own directory
	var volumePath string
	if s3PathPrefix == "" {
		volumePath = fmt.Sprintf("volumes/%s", volumeID)
	} else {
		volumePath = fmt.Sprintf("%s/volumes/%s", s3PathPrefix, volumeID)
	}
	s3Remote, err := BuildS3RemoteString(s3Config, volumePath)
	if err != nil {
		return fmt.Errorf("failed to build S3 remote string: %w", err)
	}

	klog.Infof("Deleting volume data from S3: %s (bucket: %s, path: %s)", volumeID, s3Config.Bucket, volumePath)

	// Use operations/purge to recursively delete all files
	params := map[string]interface{}{
		"fs":     s3Remote,
		"remote": "",
	}

	_, err = RPC("operations/purge", params)
	if err != nil {
		errStr := err.Error()
		// Check if it's a "directory not found" error - that's OK, nothing to delete
		if contains(errStr, "directory not found") || contains(errStr, "not found") || contains(errStr, "404") {
			klog.Infof("Volume data already deleted or never existed: %s", volumeID)
			return nil
		}
		return fmt.Errorf("failed to purge volume data: %w", err)
	}

	klog.Infof("Successfully deleted volume data from S3: %s", volumeID)
	return nil
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
