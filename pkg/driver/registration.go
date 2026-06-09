package driver

import (
	"context"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

// RegistrationHealthPort is where the driver serves its registration-health
// endpoint; the node-driver-registrar's liveness probe targets it so a lost
// registration restarts only the registrar, not the driver or its mounts.
const RegistrationHealthPort = ":9810"

// registrationStartupGrace lets initial registration complete before the
// endpoint can report unhealthy.
const registrationStartupGrace = 90 * time.Second

var registrationHealthStart time.Time

// RunRegistrationHealthServer serves GET /healthz: 200 while the driver is in
// this node's CSINode, 503 otherwise. Blocks; run in a goroutine.
func (ns *NodeServer) RunRegistrationHealthServer(addr string) {
	registrationHealthStart = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if ns.isDriverRegistered() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("registered"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("driver not registered in CSINode"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	klog.Infof("Registration health server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		klog.Errorf("Registration health server stopped: %v", err)
	}
}

// isDriverRegistered reports whether this node's CSINode lists the driver,
// failing open during startup grace and on API errors so the registrar is
// restarted only on a confirmed absence.
func (ns *NodeServer) isDriverRegistered() bool {
	if time.Since(registrationHealthStart) < registrationStartupGrace {
		return true
	}
	if ns.clientset == nil || ns.driver.nodeID == "" {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	csiNode, err := ns.clientset.StorageV1().CSINodes().Get(ctx, ns.driver.nodeID, metav1.GetOptions{})
	if err != nil {
		klog.V(4).Infof("registration check: could not get CSINode %s: %v (failing open)", ns.driver.nodeID, err)
		return true
	}
	for _, d := range csiNode.Spec.Drivers {
		if d.Name == DriverName {
			return true
		}
	}
	klog.Warningf("registration check: driver %s is NOT present in CSINode %s — "+
		"registrar liveness will fail to force re-registration", DriverName, ns.driver.nodeID)
	return false
}
