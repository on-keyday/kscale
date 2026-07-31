package stat

import (
	"testing"

	"github.com/on-keyday/kscale/consts"
)

func TestAppLifecycle(t *testing.T) {
	l := NewAppLifecycle()
	if l.Status() != consts.AppStatusInitialized {
		t.Fatalf("initial = %v, want Initialized", l.Status())
	}
	// Stat() carries the status as Appstat and nothing else (so it never becomes a dest).
	st := l.Stat()
	if st.CdnAppRealtime == nil || st.CdnAppRealtime.Appstat == nil {
		t.Fatal("Stat() must carry CdnAppRealtime.Appstat")
	}
	if *st.CdnAppRealtime.Appstat != uint32(consts.AppStatusInitialized) {
		t.Fatalf("Stat appstat = %d", *st.CdnAppRealtime.Appstat)
	}
	if st.CdnAppRealtime.BoundIfaces != nil || st.CdnAppRealtime.LbId != nil {
		t.Fatal("Stat() must not carry LbId/interface (would become a dest)")
	}
	for _, tc := range []struct {
		set  func()
		want consts.AppStatus
	}{
		{l.SetRunning, consts.AppStatusRunning},
		{l.SetStopped, consts.AppStatusStopped},
		{l.SetError, consts.AppStatusError},
	} {
		tc.set()
		if l.Status() != tc.want {
			t.Fatalf("after set: %v, want %v", l.Status(), tc.want)
		}
		if *l.Stat().CdnAppRealtime.Appstat != uint32(tc.want) {
			t.Fatalf("stat mismatch after %v", tc.want)
		}
	}
}
