package edge

import (
	"testing"
	"time"
)

func TestComputeBudgetResolution(t *testing.T) {
	const builtin = 100 * time.Millisecond
	cases := []struct {
		name   string
		cfg    *PopcacheConfigInfo // nil = no provider
		module time.Duration       // 0 = module declares nothing
		want   time.Duration
	}{
		{"no cfg, module unset -> builtin", nil, 0, builtin},
		{"no cfg, module above builtin -> clamped to builtin", nil, 500 * time.Millisecond, builtin},
		{"no cfg, module below builtin -> module", nil, 30 * time.Millisecond, 30 * time.Millisecond},
		{"cfg default raises the unset case", &PopcacheConfigInfo{WasmComputeDefault: 200 * time.Millisecond}, 0, 200 * time.Millisecond},
		{"cfg default alone is also the ceiling", &PopcacheConfigInfo{WasmComputeDefault: 200 * time.Millisecond}, 900 * time.Millisecond, 200 * time.Millisecond},
		{"max grants headroom above the default", &PopcacheConfigInfo{WasmComputeDefault: 200 * time.Millisecond, WasmComputeMax: time.Second}, 500 * time.Millisecond, 500 * time.Millisecond},
		{"max clamps a greedy module", &PopcacheConfigInfo{WasmComputeDefault: 200 * time.Millisecond, WasmComputeMax: time.Second}, 5 * time.Second, time.Second},
		{"max without default clamps the builtin too", &PopcacheConfigInfo{WasmComputeMax: 50 * time.Millisecond}, 0, 50 * time.Millisecond},
		{"max alone lets a module opt up", &PopcacheConfigInfo{WasmComputeMax: time.Second}, 700 * time.Millisecond, 700 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &edgeComputing{computingTimeout: builtin, routing: NewRoutingRegistry()}
			if tc.cfg != nil {
				r.cfg = fakePopcacheConfig{info: *tc.cfg}
			}
			const id = 7
			spec := ModuleSpec{ID: id, Method: "GET", Path: "/x", FilePath: "x.wasm", ComputeBudget: tc.module}
			if err := r.routing.Register(spec, 1, fakeCompiledModule{}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if got := r.computeBudgetFor(id); got != tc.want {
				t.Fatalf("computeBudgetFor = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("unknown module id gets the default", func(t *testing.T) {
		r := &edgeComputing{computingTimeout: builtin, routing: NewRoutingRegistry()}
		if got := r.computeBudgetFor(999); got != builtin {
			t.Fatalf("computeBudgetFor(unknown) = %v, want %v", got, builtin)
		}
	})
}
