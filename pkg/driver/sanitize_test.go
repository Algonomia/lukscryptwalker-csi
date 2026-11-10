package driver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/proto"
)

func TestStripSecrets(t *testing.T) {
	const secret = "s3cr3t-value"
	reqs := []proto.Message{
		&csi.CreateVolumeRequest{Name: "pvc-1", Secrets: map[string]string{"passphrase": secret}},
		&csi.DeleteVolumeRequest{VolumeId: "pvc-1", Secrets: map[string]string{"aws_secret_access_key": secret}},
		&csi.ControllerExpandVolumeRequest{VolumeId: "pvc-1", Secrets: map[string]string{"passphrase": secret}},
		&csi.NodeExpandVolumeRequest{VolumeId: "pvc-1", Secrets: map[string]string{"passphrase": secret}},
	}
	for _, req := range reqs {
		t.Run(fmt.Sprintf("%T", req), func(t *testing.T) {
			out := fmt.Sprintf("%+v", stripSecrets(req))
			if strings.Contains(out, secret) {
				t.Fatalf("secret leaked in log output: %s", out)
			}
			if !strings.Contains(out, "pvc-1") {
				t.Fatalf("non-secret fields dropped: %s", out)
			}
			if !strings.Contains(fmt.Sprintf("%+v", req), secret) {
				t.Fatal("original request was mutated")
			}
		})
	}
}
