package secrets

import "fmt"

// Validate checks if the VolumeSecrets has required fields populated
func (vs *VolumeSecrets) Validate(requireS3 bool) error {
	if vs.Passphrase == "" {
		return fmt.Errorf("LUKS passphrase is required but not set")
	}

	if requireS3 {
		if vs.S3AccessKeyID == "" {
			return fmt.Errorf("S3 access key ID is required but not set")
		}
		if vs.S3SecretAccessKey == "" {
			return fmt.Errorf("S3 secret access key is required but not set")
		}
	}

	return nil
}

// HasS3Credentials returns true if S3 credentials are populated
func (vs *VolumeSecrets) HasS3Credentials() bool {
	return vs.S3AccessKeyID != "" && vs.S3SecretAccessKey != ""
}

// GetPassphrase returns the LUKS passphrase
func (vs *VolumeSecrets) GetPassphrase() string {
	return vs.Passphrase
}

// GetS3AccessKeyID returns the S3 access key ID
func (vs *VolumeSecrets) GetS3AccessKeyID() string {
	return vs.S3AccessKeyID
}

// GetS3SecretAccessKey returns the S3 secret access key
func (vs *VolumeSecrets) GetS3SecretAccessKey() string {
	return vs.S3SecretAccessKey
}