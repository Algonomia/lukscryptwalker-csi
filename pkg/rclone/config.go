package rclone

import (
	"fmt"
	"strings"

	"github.com/rclone/rclone/fs/config/obscure"
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

// BuildCryptRemoteString builds an inline crypt remote wrapping an S3 remote
// The crypt remote provides client-side encryption using rclone's crypt backend
func BuildCryptRemoteString(s3Config *S3Config, cryptConfig *CryptConfig, s3Path string) (string, error) {
	if cryptConfig.Password == "" {
		return "", fmt.Errorf("crypt password is required")
	}

	// Obscure the passwords using rclone's obscure package
	obscuredPassword, err := obscure.Obscure(cryptConfig.Password)
	if err != nil {
		return "", fmt.Errorf("failed to obscure password: %w", err)
	}

	obscuredPassword2 := ""
	if cryptConfig.Password2 != "" {
		obscuredPassword2, err = obscure.Obscure(cryptConfig.Password2)
		if err != nil {
			return "", fmt.Errorf("failed to obscure password2: %w", err)
		}
	}

	// Build the underlying S3 remote string
	s3Remote, err := BuildS3RemoteString(s3Config, s3Path)
	if err != nil {
		return "", fmt.Errorf("failed to build S3 remote: %w", err)
	}

	// Escape the S3 remote for nesting within the crypt remote
	escapedS3Remote := escapeRemoteForNesting(s3Remote)

	// Build crypt remote parts
	var cryptParts []string
	cryptParts = append(cryptParts, fmt.Sprintf("remote=%s", escapedS3Remote))
	cryptParts = append(cryptParts, fmt.Sprintf("password=%s", obscuredPassword))
	if obscuredPassword2 != "" {
		cryptParts = append(cryptParts, fmt.Sprintf("password2=%s", obscuredPassword2))
	}
	// Encrypt file names for additional security
	cryptParts = append(cryptParts, "filename_encryption=standard")
	// Encrypt directory names as well
	cryptParts = append(cryptParts, "directory_name_encryption=true")

	// Format: :crypt,remote=...,password=...,password2=...:
	cryptRemote := fmt.Sprintf(":crypt,%s:", strings.Join(cryptParts, ","))

	return cryptRemote, nil
}

// escapeRemoteForNesting escapes a remote string for use inside another remote's parameters
// For librclone RPC, we use single quotes to wrap values containing special characters
func escapeRemoteForNesting(remote string) string {
	// Escape any existing single quotes by doubling them
	remote = strings.ReplaceAll(remote, "'", "''")
	// Wrap in single quotes to handle colons and commas
	return "'" + remote + "'"
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
