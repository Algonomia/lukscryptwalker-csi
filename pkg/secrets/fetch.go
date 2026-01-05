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

	// Fetch S3 credentials if specified
	if params.S3Secret.Name != "" && params.S3Secret.Namespace != "" {
		accessKeyID, secretAccessKey, err := sm.fetchS3Credentials(ctx, params.S3Secret)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch S3 credentials: %w", err)
		}
		secrets.S3AccessKeyID = accessKeyID
		secrets.S3SecretAccessKey = secretAccessKey
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

// fetchS3Credentials fetches S3 credentials from a Kubernetes secret
func (sm *SecretsManager) fetchS3Credentials(ctx context.Context, ref SecretReference) (string, string, error) {
	klog.V(4).Infof("Fetching S3 credentials from secret %s/%s", ref.Namespace, ref.Name)

	secret, err := sm.clientset.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to get secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	accessKeyID, ok := secret.Data[S3AccessKeyIDKey]
	if !ok {
		return "", "", fmt.Errorf("key '%s' not found in secret %s/%s", S3AccessKeyIDKey, ref.Namespace, ref.Name)
	}

	secretAccessKey, ok := secret.Data[S3SecretAccessKeyKey]
	if !ok {
		return "", "", fmt.Errorf("key '%s' not found in secret %s/%s", S3SecretAccessKeyKey, ref.Namespace, ref.Name)
	}

	return string(accessKeyID), string(secretAccessKey), nil
}