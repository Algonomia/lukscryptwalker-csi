package driver

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

// K8sClient interface for Kubernetes operations
type K8sClient interface {
	GetPVByVolumeID(ctx context.Context, volumeID string) (*corev1.PersistentVolume, error)
	GetStorageClassParameters(ctx context.Context, name string) (map[string]string, error)
}

// StorageClassParamsRetriever interface for retrieving StorageClass parameters
type StorageClassParamsRetriever interface {
	GetStorageClassParametersByVolumeID(ctx context.Context, volumeID string) (map[string]string, error)
}

// getPVByVolumeID finds the PV that matches the CSI volume handle
// This is a shared implementation used by both NodeServer and ControllerServer
func getPVByVolumeID(ctx context.Context, clientset kubernetes.Interface, volumeID string) (*corev1.PersistentVolume, error) {
	if clientset == nil {
		return nil, fmt.Errorf("kubernetes clientset not available")
	}

	pvList, err := clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list PVs: %v", err)
	}

	for _, pv := range pvList.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle == volumeID {
			return &pv, nil
		}
	}

	return nil, fmt.Errorf("PV with volumeID %s not found", volumeID)
}

// getStorageClassParameters retrieves parameters from a StorageClass by name
// This is a shared implementation used by both NodeServer and ControllerServer
func getStorageClassParameters(ctx context.Context, clientset kubernetes.Interface, name string) (map[string]string, error) {
	if clientset == nil {
		return nil, fmt.Errorf("kubernetes clientset not available")
	}

	if name == "" {
		return nil, fmt.Errorf("StorageClass name is empty")
	}

	sc, err := clientset.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get StorageClass %s: %v", name, err)
	}

	return sc.Parameters, nil
}

// =============================================================================
// High-Level Helper Functions
// =============================================================================

// GetStorageClassParametersByVolumeID retrieves StorageClass parameters for a volume (NodeServer)
// This is a helper that combines getPVByVolumeID + getStorageClassParameters operations
func (ns *NodeServer) GetStorageClassParametersByVolumeID(ctx context.Context, volumeID string) (map[string]string, error) {
	pv, err := getPVByVolumeID(ctx, ns.clientset, volumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get PV for volume %s: %w", volumeID, err)
	}

	if pv.Spec.StorageClassName == "" {
		return nil, fmt.Errorf("PV %s has no StorageClass", volumeID)
	}

	scParams, err := getStorageClassParameters(ctx, ns.clientset, pv.Spec.StorageClassName)
	if err != nil {
		return nil, fmt.Errorf("failed to get StorageClass parameters for %s: %w", pv.Spec.StorageClassName, err)
	}

	klog.V(4).Infof("Retrieved StorageClass parameters for volume %s from StorageClass %s", volumeID, pv.Spec.StorageClassName)
	return scParams, nil
}

// GetStorageClassParametersByVolumeID retrieves StorageClass parameters for a volume (ControllerServer)
func (cs *ControllerServer) GetStorageClassParametersByVolumeID(ctx context.Context, volumeID string) (map[string]string, error) {
	pv, err := getPVByVolumeID(ctx, cs.clientset, volumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to get PV for volume %s: %w", volumeID, err)
	}

	if pv.Spec.StorageClassName == "" {
		return nil, fmt.Errorf("PV %s has no StorageClass", volumeID)
	}

	scParams, err := getStorageClassParameters(ctx, cs.clientset, pv.Spec.StorageClassName)
	if err != nil {
		return nil, fmt.Errorf("failed to get StorageClass parameters for %s: %w", pv.Spec.StorageClassName, err)
	}

	klog.V(4).Infof("Retrieved StorageClass parameters for volume %s from StorageClass %s", volumeID, pv.Spec.StorageClassName)
	return scParams, nil
}

// GetS3PathPrefixByVolumeID retrieves the S3 pathPrefix parameter for a volume with graceful fallback
func GetS3PathPrefixByVolumeID(ctx context.Context, retriever StorageClassParamsRetriever, volumeID string) string {
	scParams, err := retriever.GetStorageClassParametersByVolumeID(ctx, volumeID)
	if err != nil {
		klog.Warningf("Failed to get StorageClass parameters for volume %s, using default path: %v", volumeID, err)
		return ""
	}

	pathPrefix := scParams[S3PathPrefixParam]
	if pathPrefix != "" {
		klog.V(4).Infof("Using pathPrefix from StorageClass for volume %s: %s", volumeID, pathPrefix)
	} else {
		klog.V(4).Infof("No pathPrefix configured for volume %s, using default", volumeID)
	}

	return pathPrefix
}