package rclone

import (
	"fmt"
	"strings"

	"k8s.io/klog"
)

// S3Config holds S3 connection configuration
type S3Config struct {
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	ForcePathStyle  bool
	Provider        string // S3 provider: AWS, Minio, Ceph, Other, etc.
}

// CryptConfig holds crypt encryption configuration
type CryptConfig struct {
	Password  string // Primary encryption password
	Password2 string // Salt password for additional security
}

// BuildS3RemoteString builds an inline S3 remote connection string
// Format: :s3,param1=val1,param2=val2:bucket/path
func BuildS3RemoteString(config *S3Config, path string) (string, error) {
	if config.Bucket == "" {
		return "", fmt.Errorf("S3 bucket is required")
	}
	if config.Region == "" {
		return "", fmt.Errorf("S3 region is required")
	}

	var parts []string

	// Set provider - default to "Other" for custom endpoints, "AWS" otherwise
	provider := config.Provider
	if provider == "" {
		if config.Endpoint != "" {
			provider = "Other"
		} else {
			provider = "AWS"
		}
	}
	parts = append(parts, fmt.Sprintf("provider=%s", provider))

	parts = append(parts, "env_auth=false")
	parts = append(parts, fmt.Sprintf("access_key_id=%s", quoteValue(config.AccessKeyID)))
	parts = append(parts, fmt.Sprintf("secret_access_key=%s", quoteValue(config.SecretAccessKey)))
	parts = append(parts, fmt.Sprintf("region=%s", quoteValue(config.Region)))

	if config.Endpoint != "" {
		parts = append(parts, fmt.Sprintf("endpoint=%s", quoteValue(config.Endpoint)))
	}
	if config.ForcePathStyle {
		parts = append(parts, "force_path_style=true")
	}

	// Format: :s3,param1=val1,param2=val2:bucket/path
	fullPath := config.Bucket
	if path != "" {
		fullPath = fmt.Sprintf("%s/%s", config.Bucket, path)
	}

	remote := fmt.Sprintf(":s3,%s:%s", strings.Join(parts, ","), fullPath)
	return remote, nil
}

// quoteValue wraps a value in single quotes if it contains special characters
func quoteValue(value string) string {
	// Check if value contains characters that need quoting
	if strings.ContainsAny(value, ":,=' \t") {
		// Escape existing single quotes by doubling them
		value = strings.ReplaceAll(value, "'", "''")
		return "'" + value + "'"
	}
	return value
}

// CreateNamedS3Config creates a named rclone S3 config via the config/create RPC.
// Using named configs (instead of inline remotes) ensures the VFS cache directory
// name is deterministic across restarts.
func CreateNamedS3Config(name string, config *S3Config) error {
	// Delete first to handle pre-existing entries from incomplete previous mounts
	_, _ = RPC("config/delete", map[string]interface{}{"name": name})

	provider := config.Provider
	if provider == "" {
		if config.Endpoint != "" {
			provider = "Other"
		} else {
			provider = "AWS"
		}
	}

	params := map[string]interface{}{
		"name": name,
		"type": "s3",
		"parameters": map[string]interface{}{
			"provider":          provider,
			"env_auth":          "false",
			"access_key_id":     config.AccessKeyID,
			"secret_access_key": config.SecretAccessKey,
			"region":            config.Region,
		},
	}

	if config.Endpoint != "" {
		params["parameters"].(map[string]interface{})["endpoint"] = config.Endpoint
	}
	if config.ForcePathStyle {
		params["parameters"].(map[string]interface{})["force_path_style"] = "true"
	}

	_, err := RPC("config/create", params)
	if err != nil {
		return fmt.Errorf("failed to create S3 config %s: %w", name, err)
	}
	klog.V(4).Infof("Created named S3 config: %s", name)
	return nil
}

// CreateNamedCryptConfig creates a named rclone crypt config via the config/create RPC.
// s3RemotePath is the full remote reference including bucket/path (e.g., "myS3Config:bucket/path").
// Passwords are passed as plain text; opt.obscure=true tells rclone to obscure them.
func CreateNamedCryptConfig(name string, s3RemotePath string, cryptConfig *CryptConfig) error {
	if cryptConfig.Password == "" {
		return fmt.Errorf("crypt password is required")
	}

	// Delete first to handle pre-existing entries
	_, _ = RPC("config/delete", map[string]interface{}{"name": name})

	parameters := map[string]interface{}{
		"remote":                    s3RemotePath,
		"password":                  cryptConfig.Password,
		"filename_encryption":       "standard",
		"directory_name_encryption": "true",
	}
	if cryptConfig.Password2 != "" {
		parameters["password2"] = cryptConfig.Password2
	}

	params := map[string]interface{}{
		"name":       name,
		"type":       "crypt",
		"parameters": parameters,
		"opt": map[string]interface{}{
			"obscure": true,
		},
	}

	_, err := RPC("config/create", params)
	if err != nil {
		return fmt.Errorf("failed to create crypt config %s: %w", name, err)
	}
	klog.V(4).Infof("Created named crypt config: %s", name)
	return nil
}

// DeleteNamedConfigs deletes named rclone configs, ignoring errors.
func DeleteNamedConfigs(names ...string) {
	for _, name := range names {
		_, err := RPC("config/delete", map[string]interface{}{"name": name})
		if err != nil {
			klog.V(4).Infof("Failed to delete config %s (may not exist): %v", name, err)
		} else {
			klog.V(4).Infof("Deleted named config: %s", name)
		}
	}
}

// DeriveRcloneCryptConfig derives rclone crypt passwords from LUKS passphrase
// password: The main encryption password (derived from LUKS passphrase)
// password2: Additional salt (derived from LUKS passphrase + salt for uniqueness)
// salt: Used for password2 derivation - typically s3PathPrefix if set, otherwise volumeID
func DeriveRcloneCryptConfig(luksPassphrase, salt string) *CryptConfig {
	// Use LUKS passphrase as primary password
	// Use salt as additional component for password2
	// This ensures different volumes/paths have different encryption even with same passphrase
	return &CryptConfig{
		Password:  luksPassphrase,
		Password2: fmt.Sprintf("%s-%s", luksPassphrase, salt),
	}
}
