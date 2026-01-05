package secrets

import (
	"k8s.io/client-go/kubernetes"
)

// Secret key constants for Kubernetes CSI
const (
	// LUKS Passphrase Management
	SecretNamespaceKey   = "csi.storage.k8s.io/node-stage-secret-namespace"
	SecretNameKey        = "csi.storage.k8s.io/node-stage-secret-name"
	PassphraseKeyParam   = "passphraseKey"
	DefaultPassphraseKey = "passphrase"

	// S3 Credentials Secret Reference (in StorageClass parameters)
	S3CredentialsSecretName      = "s3-access-key-id-secret-name"
	S3CredentialsSecretNamespace = "s3-access-key-id-secret-namespace"

	// S3 Secret Keys (within the secret data)
	S3AccessKeyIDKey     = "s3-access-key-id"
	S3SecretAccessKeyKey = "s3-secret-access-key"
	S3BucketKey          = "s3-bucket"
	S3RegionKey          = "s3-region"
	S3EndpointKey        = "s3-endpoint"
	S3PathPrefixKey      = "s3-path-prefix"
)

// VolumeSecrets holds all secrets needed for volume operations
type VolumeSecrets struct {
	// LUKS encryption
	Passphrase    string
	PassphraseKey string // The key name used to retrieve passphrase

	// S3 credentials (optional, only for S3 backend)
	S3AccessKeyID     string
	S3SecretAccessKey string

	// Metadata for tracking
	LUKSSecretName      string
	LUKSSecretNamespace string
	S3SecretName        string
	S3SecretNamespace   string
}

// SecretReference identifies a Kubernetes secret
type SecretReference struct {
	Name      string
	Namespace string
}

// SecretsManager handles fetching secrets from Kubernetes
type SecretsManager struct {
	clientset kubernetes.Interface
}

// NewSecretsManager creates a new SecretsManager instance
func NewSecretsManager(clientset kubernetes.Interface) *SecretsManager {
	return &SecretsManager{
		clientset: clientset,
	}
}

// S3ConfigFromSecrets extracts S3 configuration from a CSI secrets map
// This is used when secrets are passed directly by the CSI provisioner (e.g., DeleteVolume)
func S3ConfigFromSecrets(secretData map[string]string) *S3Credentials {
	if secretData == nil {
		return nil
	}

	// Check for bucket - if not present, this isn't an S3 volume
	bucket := secretData[S3BucketKey]
	if bucket == "" {
		return nil
	}

	accessKeyID := secretData[S3AccessKeyIDKey]
	secretAccessKey := secretData[S3SecretAccessKeyKey]

	// Both credentials are required
	if accessKeyID == "" || secretAccessKey == "" {
		return nil
	}

	return &S3Credentials{
		Bucket:          bucket,
		Region:          secretData[S3RegionKey],
		Endpoint:        secretData[S3EndpointKey],
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		PathPrefix:      secretData[S3PathPrefixKey],
	}
}

// S3Credentials holds S3 connection credentials extracted from secrets
type S3Credentials struct {
	Bucket          string
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	PathPrefix      string // Optional custom path prefix in S3
}