package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lukscryptwalker-csi/pkg/driver"
	"github.com/lukscryptwalker-csi/pkg/metrics"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

var (
	endpoint            = flag.String("endpoint", "unix:///tmp/csi.sock", "CSI endpoint")
	nodeID              = flag.String("nodeid", "", "node id")
	version             = flag.Bool("version", false, "Print the version and exit.")
	metricsAddr         = flag.String("metrics-addr", ":9090", "Address to serve metrics on")
	vfsCacheSize        = flag.String("vfs-cache-size", "20G", "Size of the encrypted LUKS volume for VFS cache")
	luksSecretName      = flag.String("luks-secret-name", "luks-secret", "Name of the Kubernetes secret containing LUKS passphrase")
	luksSecretNamespace = flag.String("luks-secret-namespace", "kube-system", "Namespace of the LUKS secret")
	luksSecretKey       = flag.String("luks-secret-key", "passphrase", "Key within the secret containing the passphrase")
	// VFS cache cleanup settings
	vfsCacheCleanupInterval    = flag.Duration("vfs-cache-cleanup-interval", 5*time.Minute, "Interval for VFS cache directory cleanup")
	vfsCacheDiskUsageThreshold = flag.Float64("vfs-cache-disk-threshold", 0.85, "Disk usage threshold (0.0-1.0) to trigger aggressive cache cleanup")
)

// isControllerMode detects if we're running in controller mode based on the endpoint path
func isControllerMode(endpoint string) bool {
	// Controller uses: unix:///var/lib/csi/sockets/pluginproxy/csi.sock
	// Node uses: unix:///csi/csi.sock
	return strings.Contains(endpoint, "/var/lib/csi/sockets/pluginproxy/")
}

func main() {
	flag.Parse()

	if *version {
		fmt.Printf("lukscryptwalker-csi version: %s\n", driver.GetVersion())
		os.Exit(0)
	}

	if *nodeID == "" {
		klog.Fatal("NodeID cannot be empty")
	}

	// Set up encrypted VFS cache volume (only for nodes, skip for controller)
	if !isControllerMode(*endpoint) {
		// Create Kubernetes client to fetch secrets
		config, err := rest.InClusterConfig()
		if err != nil {
			klog.Fatalf("Failed to get in-cluster config: %v", err)
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			klog.Fatalf("Failed to create kubernetes client: %v", err)
		}

		// Fetch LUKS passphrase from Kubernetes secret
		secretsManager := secrets.NewSecretsManager(clientset)

		secretParams := secrets.SecretParams{
			LUKSSecret: secrets.SecretReference{
				Name:      *luksSecretName,
				Namespace: *luksSecretNamespace,
			},
			PassphraseKey: *luksSecretKey,
		}

		volSecrets, err := secretsManager.FetchVolumeSecrets(context.Background(), secretParams)
		if err != nil {
			klog.Fatalf("Failed to fetch LUKS passphrase from secret: %v", err)
		}

		if volSecrets.Passphrase == "" {
			klog.Fatal("LUKS passphrase is empty in secret")
		}

		// Combine with node ID to ensure uniqueness per node
		vfsCachePassphrase := fmt.Sprintf("%s-%s", volSecrets.Passphrase, *nodeID)
		vfsCachePath, err := rclone.SetupVFSCache(*vfsCacheSize, vfsCachePassphrase)
		if err != nil {
			klog.Fatalf("Failed to set up encrypted VFS cache: %v", err)
		}

		// Start background VFS cache cleanup to remove empty directories and manage disk usage
		rclone.StartVFSCacheCleanup(*vfsCacheCleanupInterval, *vfsCacheDiskUsageThreshold)

		defer func() {
			// Stop cleanup before teardown
			rclone.StopVFSCacheCleanup()
			if err := rclone.TeardownVFSCache(); err != nil {
				klog.Errorf("Failed to teardown VFS cache: %v", err)
			}
		}()

		// The encrypted VFS cache is now mounted directly at /root/.cache/rclone
		// so rclone will automatically use it without any configuration
		klog.Infof("Encrypted VFS cache mounted at rclone's default location: %s", vfsCachePath)
	} else {
		klog.Info("Skipping VFS cache setup (controller mode)")
	}

    // Initialize rclone for S3 sync functionality
    if err := rclone.Initialize(); err != nil {
        klog.Fatalf("Failed to initialize rclone: %v", err)
    }
    defer rclone.Finalize()

	// Initialize and start metrics server
	metrics.Initialize(*nodeID)
	metrics.StartServer(*metricsAddr, driver.GetVersion())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-signalChan
		klog.Info("Received shutdown signal, shutting down...")
		cancel()
	}()

	d := driver.NewDriver(*endpoint, *nodeID)
	klog.Info("Starting LUKS CSI driver")

	if err := d.Run(ctx); err != nil {
		klog.Fatalf("Failed to run CSI driver: %v", err)
	}
}
