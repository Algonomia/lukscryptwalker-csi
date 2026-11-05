package driver

import (
	"context"
	"testing"

	"github.com/lukscryptwalker-csi/pkg/secrets"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The LUKS secret reference comes from the PV when it has one, not from a
// StorageClass that may have been recreated since the volume was provisioned.
func TestGetVolumeParametersSecretFromPV(t *testing.T) {
	clientset := fake.NewClientset(&storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "sc"},
		Parameters: map[string]string{
			secrets.SecretNameKey:      "sc-secret",
			secrets.SecretNamespaceKey: "sc-ns",
			"fsType":                   "ext4",
		},
	})
	pv := func(ref *corev1.SecretReference) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{
			StorageClassName: "sc",
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{NodeStageSecretRef: ref},
			},
		}}
	}

	params, err := getVolumeParameters(context.Background(), clientset, pv(&corev1.SecretReference{Name: "pv-secret", Namespace: "pv-ns"}))
	if err != nil || params[secrets.SecretNameKey] != "pv-secret" || params[secrets.SecretNamespaceKey] != "pv-ns" || params["fsType"] != "ext4" {
		t.Fatalf("PV with secret ref: got %v, %v", params, err)
	}

	// Without one (e.g. a static PV), the StorageClass reference still applies.
	params, err = getVolumeParameters(context.Background(), clientset, pv(nil))
	if err != nil || params[secrets.SecretNameKey] != "sc-secret" || params[secrets.SecretNamespaceKey] != "sc-ns" {
		t.Fatalf("PV without secret ref: got %v, %v", params, err)
	}
}
