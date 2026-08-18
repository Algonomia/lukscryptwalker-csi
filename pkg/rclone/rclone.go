package rclone

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lukscryptwalker-csi/pkg/asynclog"
	"github.com/rclone/rclone/librclone/librclone"
	"k8s.io/klog"

	// Import rclone backends we need
	_ "github.com/rclone/rclone/backend/crypt"
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/s3"

	// Import mount command to register mount/mount RPC
	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/mount"
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
		setServerModTime()
	})

	initDone = true
	return nil
}

// setServerModTime makes the VFS read mtimes from the S3 listing instead of
// each object's metadata. S3 keeps rclone's mtime in object metadata, which a
// listing does not return, so serving one cold readdir costs a HEAD per file —
// the whole reason `ls` stalls for seconds once the dir cache expires. Uploads
// still record the true mtime, so this only changes what we read back.
func setServerModTime() {
	if os.Getenv("RCLONE_USE_SERVER_MODTIME") == "false" {
		klog.Info("UseServerModTime disabled by env: cold directory listings will HEAD every object")
		return
	}
	params := map[string]interface{}{"main": map[string]interface{}{"UseServerModTime": true}}
	if _, err := RPCWithTimeout("options/set", params, RPCDefaultTimeout); err != nil {
		klog.Warningf("Could not enable UseServerModTime (%v); cold directory listings will HEAD every object", err)
		return
	}
	klog.Info("Enabled UseServerModTime: file mtimes come from the S3 listing, not a HEAD per object")
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

// librclone calls take no context, so a wedged VFS blocks the caller forever —
// and mount/* hold rclone's global mount mutex while they do, freezing every
// later mount on the node. Every call is bounded; one stuck past the fatal
// threshold ends the process so kubelet restarts it.
const (
	// RPCDefaultTimeout bounds calls that have no explicit budget.
	RPCDefaultTimeout = 60 * time.Second
	// rpcStuckFatalAfter is how long a mount-lock-holding call may stay in
	// flight before the process is unrecoverable and must be restarted.
	rpcStuckFatalAfter = 5 * time.Minute
	// rpcStuckCheckEvery is the watchdog's polling period.
	rpcStuckCheckEvery = 30 * time.Second
)

// ErrRPCTimeout marks an RPC past its budget. The call is still running:
// librclone offers no cancellation, only outliving.
var ErrRPCTimeout = errors.New("librclone RPC timed out")

// IsRPCTimeout reports whether err came from an RPC exceeding its budget.
func IsRPCTimeout(err error) bool { return errors.Is(err, ErrRPCTimeout) }

// inFlightRPC records a librclone call that has not returned yet.
type inFlightRPC struct {
	method  string
	started time.Time
	budget  time.Duration
}

// overdueBy measures against the call's own budget: a long call given a long
// budget is not stuck.
func (c inFlightRPC) overdueBy(now time.Time) time.Duration {
	limit := max(c.budget, rpcStuckFatalAfter)
	return now.Sub(c.started) - limit
}

var (
	rpcMu        sync.Mutex
	rpcInFlight  = map[uint64]inFlightRPC{}
	rpcSeq       uint64
	rpcWatchOnce sync.Once
)

// rpcAbort ends the process when librclone is permanently wedged. Overridable
// in tests.
var rpcAbort = func(reason string) {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	// Straight to stderr, bounded: a queued dump is dropped exactly when it is
	// the only explanation left, and a stalled pipe must not stop the exit.
	asynclog.WriteBounded(os.Stderr,
		fmt.Sprintf("FATAL-RPC-STUCK: %s; goroutine dump follows\n%s\n", reason, buf[:n]), 10*time.Second)
	klog.Errorf("FATAL: %s — exiting so kubelet restarts the plugin", reason)
	os.Exit(1)
}

// holdsMountLock reports whether the method serializes on rclone's global mount
// mutex; once one is stuck, nothing on the node can mount again.
func holdsMountLock(method string) bool {
	return strings.HasPrefix(method, "mount/")
}

// stuckRPCs returns calls past their own budget and the fatal floor.
func stuckRPCs(now time.Time) []inFlightRPC {
	rpcMu.Lock()
	defer rpcMu.Unlock()

	var stuck []inFlightRPC
	for _, call := range rpcInFlight {
		if call.overdueBy(now) >= 0 {
			stuck = append(stuck, call)
		}
	}
	return stuck
}

// checkStuckRPCs warns about calls that will never return, aborting when one
// holds the global mount lock.
func checkStuckRPCs(now time.Time) {
	for _, call := range stuckRPCs(now) {
		age := now.Sub(call.started).Round(time.Second)
		if holdsMountLock(call.method) {
			rpcAbort(fmt.Sprintf("librclone %s has been stuck for %s holding rclone's global mount lock; "+
				"no volume on this node can be mounted or unmounted again", call.method, age))
			return
		}
		klog.Errorf("librclone %s has been stuck for %s (leaked goroutine; the wedged VFS will never answer)",
			call.method, age)
	}
}

func startRPCWatchdog() {
	rpcWatchOnce.Do(func() {
		go func() {
			for range time.Tick(rpcStuckCheckEvery) {
				checkStuckRPCs(time.Now())
			}
		}()
	})
}

// callLibrclone runs one call with a hard timeout, releasing the caller while
// the call keeps running; the leaked goroutine is tracked for the watchdog.
func callLibrclone(method, input string, timeout time.Duration) (string, int, error) {
	startRPCWatchdog()

	rpcMu.Lock()
	rpcSeq++
	id := rpcSeq
	rpcInFlight[id] = inFlightRPC{method: method, started: time.Now(), budget: timeout}
	rpcMu.Unlock()

	type reply struct {
		output string
		status int
	}
	done := make(chan reply, 1)
	go func() {
		output, status := librclone.RPC(method, input)
		rpcMu.Lock()
		delete(rpcInFlight, id)
		rpcMu.Unlock()
		done <- reply{output, status}
	}()

	select {
	case r := <-done:
		return r.output, r.status, nil
	case <-time.After(timeout):
		klog.Errorf("RPC %s exceeded its %s budget; releasing the caller (the call keeps running)", method, timeout)
		return "", 0, fmt.Errorf("%w: %s after %s", ErrRPCTimeout, method, timeout)
	}
}

// RPCResult represents the result of an RPC call
type RPCResult struct {
	Output map[string]interface{}
	Status int
	Error  error
}

// RPC executes an rclone RPC method with JSON input, bounded by
// RPCDefaultTimeout. Use RPCWithTimeout for calls needing a different budget.
func RPC(method string, params interface{}) (*RPCResult, error) {
	return RPCWithTimeout(method, params, RPCDefaultTimeout)
}

// RPCWithTimeout executes an rclone RPC method with an explicit time budget.
func RPCWithTimeout(method string, params interface{}, timeout time.Duration) (*RPCResult, error) {
	// Ensure librclone is initialized
	if err := Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize librclone: %w", err)
	}

	inputJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal RPC params: %w", err)
	}

	klog.V(5).Infof("RPC call: %s", method)

	output, status, err := callLibrclone(method, string(inputJSON), timeout)
	if err != nil {
		return nil, err
	}

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
		result.Error = fmt.Errorf("RPC %s failed with status %d: %s", method, status, sanitizeErrorMessage(errMsg))
		klog.Errorf("RPC %s failed: status=%d, error=%s", method, status, sanitizeErrorMessage(errMsg))
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

	output, status, err := callLibrclone(method, string(inputJSON), RPCDefaultTimeout)
	if err != nil {
		return "", 0, err
	}

	if status >= 400 {
		return sanitizeErrorMessage(output), status, fmt.Errorf("RPC %s failed with status %d: %s", method, status, sanitizeErrorMessage(output))
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

	// Purging a large volume walks every object; generous, but never unbounded.
	_, err = RPCWithTimeout("operations/purge", params, 30*time.Minute)
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

// sanitizeErrorMessage removes sensitive information from error messages
// This prevents credentials from being logged
func sanitizeErrorMessage(msg string) string {
	// Patterns to redact (case-insensitive matching, but preserve structure)
	patterns := []struct {
		prefix string
		suffix string
	}{
		{"access_key_id=", ","},
		{"access_key_id=", ":"},
		{"secret_access_key=", ","},
		{"secret_access_key=", ":"},
		{"password=", ","},
		{"password=", ":"},
		{"password2=", ","},
		{"password2=", ":"},
	}

	result := msg
	for _, p := range patterns {
		result = redactBetween(result, p.prefix, p.suffix)
	}

	return result
}

// redactBetween redacts content between a prefix and suffix
func redactBetween(s, prefix, suffix string) string {
	result := s
	lowerResult := strings.ToLower(result)
	lowerPrefix := strings.ToLower(prefix)

	for {
		startIdx := strings.Index(lowerResult, lowerPrefix)
		if startIdx == -1 {
			break
		}

		valueStart := startIdx + len(prefix)
		remaining := result[valueStart:]
		lowerRemaining := strings.ToLower(remaining)

		// Find the end of the value
		endIdx := strings.Index(lowerRemaining, suffix)
		if endIdx == -1 {
			// No suffix found, redact to end of string
			result = result[:valueStart] + "[REDACTED]"
			break
		}

		// Replace the value with [REDACTED]
		result = result[:valueStart] + "[REDACTED]" + result[valueStart+endIdx:]
		lowerResult = strings.ToLower(result)
	}

	return result
}
