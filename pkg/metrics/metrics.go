package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog"
)

var (
	// Volume metrics
	VolumesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_volumes_total",
			Help: "Total number of volumes managed by the driver",
		},
		[]string{"backend", "node"},
	)

	VolumeOpen = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_volume_open",
			Help: "Whether a volume is currently open/mounted (1=open, 0=closed)",
		},
		[]string{"volume_id", "backend", "node"},
	)

	VolumeCapacityBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_volume_capacity_bytes",
			Help: "Capacity of the volume in bytes",
		},
		[]string{"volume_id", "backend", "node"},
	)

	VolumeUsedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_volume_used_bytes",
			Help: "Used space in the volume in bytes (sampled)",
		},
		[]string{"volume_id", "backend", "node"},
	)

	VolumeEncrypted = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_volume_encrypted",
			Help: "Whether a volume is encrypted (1=yes, 0=no)",
		},
		[]string{"volume_id", "backend", "node"},
	)

	// Operation metrics
	OperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lukscryptwalker_operations_total",
			Help: "Total number of CSI operations",
		},
		[]string{"operation", "status", "node"},
	)

	OperationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "lukscryptwalker_operation_duration_seconds",
			Help:    "Duration of CSI operations in seconds",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
		},
		[]string{"operation", "node"},
	)

	// S3 cache metrics (for S3 backend)
	S3CacheSizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_s3_cache_size_bytes",
			Help: "Current size of S3 VFS cache in bytes",
		},
		[]string{"node"},
	)

	S3CacheMaxBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_s3_cache_max_bytes",
			Help: "Maximum size of S3 VFS cache in bytes",
		},
		[]string{"node"},
	)

	// Health metric
	DriverHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_healthy",
			Help: "Whether the driver is healthy (1=healthy, 0=unhealthy)",
		},
		[]string{"node"},
	)

	// Driver info
	DriverInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "lukscryptwalker_driver_info",
			Help: "Driver version information",
		},
		[]string{"version", "node"},
	)
)

var (
	registerOnce sync.Once
	nodeID       string
)

// Initialize registers all metrics and sets up the node ID
func Initialize(node string) {
	nodeID = node
	registerOnce.Do(func() {
		prometheus.MustRegister(
			VolumesTotal,
			VolumeOpen,
			VolumeCapacityBytes,
			VolumeUsedBytes,
			VolumeEncrypted,
			OperationsTotal,
			OperationDurationSeconds,
			S3CacheSizeBytes,
			S3CacheMaxBytes,
			DriverHealthy,
			DriverInfo,
		)
	})
}

// GetNodeID returns the configured node ID
func GetNodeID() string {
	return nodeID
}

// StartServer starts the HTTP server for metrics and health endpoints
func StartServer(addr string, version string) {
	// Set driver info
	DriverInfo.WithLabelValues(version, nodeID).Set(1)
	DriverHealthy.WithLabelValues(nodeID).Set(1)

	mux := http.NewServeMux()

	// Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Readiness endpoint
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	klog.Infof("Starting metrics server on %s", addr)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Metrics server failed: %v", err)
		}
	}()
}

// RecordVolumeStaged records that a volume has been staged
func RecordVolumeStaged(volumeID, backend string, capacityBytes int64) {
	VolumeOpen.WithLabelValues(volumeID, backend, nodeID).Set(1)
	VolumeCapacityBytes.WithLabelValues(volumeID, backend, nodeID).Set(float64(capacityBytes))
	VolumeEncrypted.WithLabelValues(volumeID, backend, nodeID).Set(1) // Always encrypted
}

// RecordVolumeUnstaged records that a volume has been unstaged
func RecordVolumeUnstaged(volumeID, backend string) {
	VolumeOpen.WithLabelValues(volumeID, backend, nodeID).Set(0)
}

// RecordVolumeUsage records sampled filesystem usage for a volume
func RecordVolumeUsage(volumeID, backend string, usedBytes int64) {
	VolumeUsedBytes.WithLabelValues(volumeID, backend, nodeID).Set(float64(usedBytes))
}

// RecordOperation records a CSI operation
func RecordOperation(operation, status string, durationSeconds float64) {
	OperationsTotal.WithLabelValues(operation, status, nodeID).Inc()
	OperationDurationSeconds.WithLabelValues(operation, nodeID).Observe(durationSeconds)
}

// SetVolumesTotal sets the total number of volumes for a backend
