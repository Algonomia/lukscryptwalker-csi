package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// TestCSIDriverDeployment tests if the CSI driver is properly deployed
func TestCSIDriverDeployment(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test - set INTEGRATION_TEST=1 to run")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		t.Skipf("Not running in cluster: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	// Check if CSI driver pods are running
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=lukscryptwalker-csi",
	})
	require.NoError(t, err)

	assert.NotEmpty(t, pods.Items, "CSI driver pods should be deployed")

	for _, pod := range pods.Items {
		assert.Equal(t, "Running", string(pod.Status.Phase),
			fmt.Sprintf("Pod %s should be running", pod.Name))
	}
}

// TestStorageClassCreation tests if StorageClass is properly created
func TestStorageClassCreation(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") == "" {
		t.Skip("Skipping integration test - set INTEGRATION_TEST=1 to run")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		t.Skipf("Not running in cluster: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	storageClasses, err := clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	var found bool
	for _, sc := range storageClasses.Items {
		if sc.Provisioner == "lukscryptwalker.csi.k8s.io" {
			found = true
			break
		}
	}

	assert.True(t, found, "LUKSCryptWalker StorageClass should exist")
}