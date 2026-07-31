package dataplanenode

import (
	"context"
	"log/slog"
	"testing"

	"github.com/on-keyday/kscale/dpbroker"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

// TestSnapshotFoldsInExpectedNodes: declared nodes with no live connection appear
// as NotConnected rows (sorted), so absence is visible instead of silent — the
// expected_node advisory inventory's whole point.
func TestSnapshotFoldsInExpectedNodes(t *testing.T) {
	h := &Handlers{
		Broker: dpbroker.New(), // no connections
		Logger: slog.Default(),
		ExpectedNodes: func() map[string]string {
			return map[string]string{
				"s2.popcache.dp.system.kscale.local": "popcache",
				"s1.popcache.dp.system.kscale.local": "popcache",
			}
		},
	}
	resp, err := h.List(context.Background(), &pbaccess.ResourceDataplaneNodeActionListArgsDTO{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %+v, want the 2 declared-but-unconnected nodes", resp.Items)
	}
	for _, it := range resp.Items {
		if it.AppStatus != AppStatusNotConnected || it.ConnectionId != "" {
			t.Fatalf("declared-unconnected row = %+v", it)
		}
	}
	if resp.Items[0].CommonName != "s1.popcache.dp.system.kscale.local" {
		t.Fatalf("missing rows not sorted: %+v", resp.Items)
	}
	// Get resolves a declared-but-unconnected node too.
	got, err := h.Get(context.Background(), &pbaccess.ResourceDataplaneNodeActionGetArgsDTO{
		CommonName: "s2.popcache.dp.system.kscale.local",
	})
	if err != nil || got.AppStatus != AppStatusNotConnected {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}
