package container

import (
	"strings"
	"testing"
	"time"

	pb "github.com/on-keyday/kscale/protobuf/proto"
)

func TestPullNote(t *testing.T) {
	if got := pullNote(&pb.WorkloadServiceContainerStatus{PullState: "PULLING", PullImage: "a:1"}); got != "pulling a:1" {
		t.Fatalf("pulling note = %q", got)
	}
	failed := &pb.WorkloadServiceContainerStatus{
		PullState: "FAILED", PullImage: "a:1", PullError: "registry down",
		PullRetryAtUnixMs: time.Now().Add(42 * time.Second).UnixMilli(),
	}
	got := pullNote(failed)
	if !strings.HasPrefix(got, "pull of a:1 failed: registry down, retry in ") || !strings.HasSuffix(got, "s") {
		t.Fatalf("failed note = %q", got)
	}
	failed.PullRetryAtUnixMs = time.Now().Add(-time.Minute).UnixMilli() // already due: never negative
	if got := pullNote(failed); !strings.HasSuffix(got, "retry in 0s") {
		t.Fatalf("overdue retry note = %q", got)
	}
}
