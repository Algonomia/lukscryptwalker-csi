package secrets

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

// SecretParams holds parameters for fetching secrets
type SecretParams struct {
	LUKSSecret    SecretReference
	S3Secret      SecretReference
	PassphraseKey string // Key within the LUKS secret for passphrase
}

// FetchVolumeSecrets fetches all secrets needed for a volume operation
// Parameters come from StorageClass parameters and/or volume context
func (sm *SecretsManager) FetchVolumeSecrets(ctx context.Context, params SecretParams) (*VolumeSecrets, error) {
	if sm.clientset == nil {
		return nil, fmt.Errorf("kubernetes clientset not available")
	}

	secrets := &VolumeSecrets{
		PassphraseKey: params.PassphraseKey,
	}

	// Fetch LUKS passphrase secret
	if params.LUKSSecret.Name != "" && params.LUKSSecret.Namespace != "" {
		passphrase, err := sm.fetchPassphrase(ctx, params.LUKSSecret, params.PassphraseKey)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch LUKS passphrase: %w", err)
		}
		secrets.Passphrase = passphrase
		secrets.LUKSSecretName = params.LUKSSecret.Name
		secrets.LUKSSecretNamespace = params.LUKSSecret.Namespace
	}

	// Fetch S3 configuration if specified (all config is in a single secret)
	if params.S3Secret.Name != "" && params.S3Secret.Namespace != "" {
		s3Config, err := sm.fetchS3Config(ctx, params.S3Secret)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch S3 configuration: %w", err)
		}
		secrets.S3Bucket = s3Config.Bucket
		secrets.S3Region = s3Config.Region
		secrets.S3Endpoint = s3Config.Endpoint
		secrets.S3ForcePathStyle = s3Config.ForcePathStyle
		secrets.S3AccessKeyID = s3Config.AccessKeyID
		secrets.S3SecretAccessKey = s3Config.SecretAccessKey
		secrets.S3SecretName = params.S3Secret.Name
		secrets.S3SecretNamespace = params.S3Secret.Namespace
	}

	return secrets, nil
}

// fetchPassphrase fetches the LUKS passphrase from a Kubernetes secret
func (sm *SecretsManager) fetchPassphrase(ctx context.Context, ref SecretReference, passphraseKey string) (string, error) {
	if passphraseKey == "" {
		passphraseKey = DefaultPassphraseKey
	}

	klog.V(4).Infof("Fetching LUKS passphrase from secret %s/%s, key: %s", ref.Namespace, ref.Name, passphraseKey)

	secret, err := sm.clientset.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	passphrase, ok := secret.Data[passphraseKey]
	if !ok {
		return "", fmt.Errorf("passphrase key '%s' not found in secret %s/%s", passphraseKey, ref.Namespace, ref.Name)
	}

	return string(passphrase), nil
}

// S3Config holds all S3 configuration fetched from a secret
type S3Config struct {
	Bucket          string
	Region          string
	Endpoint        string
	ForcePathStyle  bool
	AccessKeyID     string
	SecretAccessKey string
}

// fetchS3Config fetches all S3 configuration from a Kubernetes secret
func (sm *SecretsManager) fetchS3Config(ctx context.Context, ref SecretReference) (*S3Config, error) {
	klog.V(4).Infof("Fetching S3 configuration from secret %s/%s", ref.Namespace, ref.Name)

	secret, err := sm.clientset.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	config := &S3Config{}

	// Required fields
	bucket, ok := secret.Data[S3BucketKey]
	if !ok {
		return nil, fmt.Errorf("key '%s' not found in secret %s/%s", S3BucketKey, ref.Namespace, ref.Name)
	}
	config.Bucket = string(bucket)

	region, ok := secret.Data[S3RegionKey]
	if !ok {
		return nil, fmt.Errorf("key '%s' not found in secret %s/%s", S3RegionKey, ref.Namespace, ref.Name)
	}
	config.Region = string(region)

	accessKeyID, ok := secret.Data[S3AccessKeyIDKey]
	if !ok {
		return nil, fmt.Errorf("key '%s' not found in secret %s/%s", S3AccessKeyIDKey, ref.Namespace, ref.Name)
	}
	config.AccessKeyID = string(accessKeyID)

	secretAccessKey, ok := secret.Data[S3SecretAccessKeyKey]
	if !ok {
		return nil, fmt.Errorf("key '%s' not found in secret %s/%s", S3SecretAccessKeyKey, ref.Namespace, ref.Name)
	}
	config.SecretAccessKey = string(secretAccessKey)

	// Optional fields
	if endpoint, ok := secret.Data[S3EndpointKey]; ok {
		config.Endpoint = string(endpoint)
	}

	if forcePathStyle, ok := secret.Data[S3ForcePathStyleKey]; ok {
		config.ForcePathStyle = string(forcePathStyle) == "true"
	}

	klog.V(4).Infof("S3 config fetched: bucket=%s, region=%s, endpoint=%s, forcePathStyle=%v",
		config.Bucket, config.Region, config.Endpoint, config.ForcePathStyle)

	return config, nil
}