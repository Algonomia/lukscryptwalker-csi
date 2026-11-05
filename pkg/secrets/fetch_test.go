package secrets

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLoadEnvPassphrase(t *testing.T) {
	defer func() { envPassphrase = "" }()

	t.Setenv(PassphraseEnvVar, "from-env")
	if p, err := LoadEnvPassphrase(); err != nil || p != "from-env" {
		t.Fatalf("got %q, %v; want from-env", p, err)
	}
	if _, set := os.LookupEnv(PassphraseEnvVar); set {
		t.Fatalf("%s still set after loading", PassphraseEnvVar)
	}

	// Unresolved bank-vaults references must be refused, not used as the key.
	for _, ref := range []string{"vault:secret/data/x#p", ">>vault:secret/data/x#p", "${vault:secret/data/x#p}"} {
		envPassphrase = ""
		t.Setenv(PassphraseEnvVar, ref)
		if _, err := LoadEnvPassphrase(); err == nil || envPassphrase != "" {
			t.Fatalf("%q: want an error and no passphrase, got err=%v", ref, err)
		}
	}
}

func TestFetchVolumeSecretsPassphraseSource(t *testing.T) {
	envPassphrase = "from-env"
	defer func() { envPassphrase = "" }()

	sm := NewSecretsManager(fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "luks", Namespace: "ns"},
		Data:       map[string][]byte{DefaultPassphraseKey: []byte("from-secret")},
	}))

	// No secret reference: the environment passphrase is used.
	vs, err := sm.FetchVolumeSecrets(context.Background(), SecretParams{})
	if err != nil || vs.Passphrase != "from-env" {
		t.Fatalf("got %+v, %v; want from-env", vs, err)
	}

	// An explicit secret reference wins over the environment.
	vs, err = sm.FetchVolumeSecrets(context.Background(), SecretParams{
		LUKSSecret:    SecretReference{Name: "luks", Namespace: "ns"},
		PassphraseKey: DefaultPassphraseKey,
	})
	if err != nil || vs.Passphrase != "from-secret" {
		t.Fatalf("got %+v, %v; want from-secret", vs, err)
	}

	// A half-set reference is an error, never a silent fallback to the environment.
	if _, err := sm.FetchVolumeSecrets(context.Background(), SecretParams{
		LUKSSecret: SecretReference{Name: "luks"},
	}); err == nil {
		t.Fatal("half-set secret reference: want an error")
	}
}
