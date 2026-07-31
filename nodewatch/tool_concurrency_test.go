package nodewatch

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// TestRegistryConcurrentRepoint exercises the reconnect pattern: one goroutine
// re-registers tools (the connection-session loop after a control-plane restart)
// while others run specs/exec (in-flight observe and chat turns). Run with -race.
func TestRegistryConcurrentRepoint(t *testing.T) {
	r := NewRegistry()
	r.Add(&recordTool{name: "node_list"})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.AddAll([]Tool{&recordTool{name: "node_list"}, &recordTool{name: "stats_get"}})
		}
		close(stop)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.specs()
			r.exec(context.Background(), toolCall{Function: toolCallFunc{Name: "node_list", Arguments: json.RawMessage(`{}`)}})
		}
	}()
	wg.Wait()
}
