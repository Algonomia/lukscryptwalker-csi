package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lukscryptwalker-csi/pkg/driver"
	"k8s.io/klog/v2"
)

var (
	endpoint = flag.String("endpoint", "unix:///tmp/csi.sock", "CSI endpoint")
	nodeID   = flag.String("nodeid", "", "node id")
	version  = flag.Bool("version", false, "Print the version and exit.")
)

func main() {
	flag.Parse()

	if *version {
		fmt.Printf("lukscryptwalker-csi version: %s\n", driver.GetVersion())
		os.Exit(0)
	}

	if *nodeID == "" {
		klog.Fatal("NodeID cannot be empty")
	}

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