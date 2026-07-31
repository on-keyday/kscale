// Command gen is the kscale code generator driver. It emits, from the resource
// declarations, the server-side ABAC gate adapters ({Resource}Gated, into
// service/) and the client-side typed-RPC dispatchers (Dispatch{Resource}, into
// client/) — the M4 steps that mechanize the hand-written gate and the
// hand-written cmd/cli call switch (kscale charter M4).
//
//	gen <resource.yaml> [resource_name ...]
//
// Scope is declarative: a resource is generated when it is `kind: resource` and
// rpc_service (see generator.generatesGate) — tagging a resource enrolls it, no
// name allowlist required. The optional resource_name args further restrict to
// the named ones (handy for regenerating a single resource).
package main

import (
	"go/format"
	"log"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
	"github.com/on-keyday/kscale/access/generator"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <resource.yaml> [resource_name ...]", os.Args[0])
	}
	resourceFile := os.Args[1]
	var filter map[string]bool
	if len(os.Args) > 2 {
		filter = map[string]bool{}
		for _, name := range os.Args[2:] {
			filter[name] = true
		}
	}

	data, err := os.ReadFile(resourceFile)
	if err != nil {
		log.Fatalf("read %s: %v", resourceFile, err)
	}
	var rf generator.ResourceFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		log.Fatalf("unmarshal %s: %v", resourceFile, err)
	}
	if filter != nil {
		kept := rf.Resources[:0]
		for _, r := range rf.Resources {
			if filter[r.Name] {
				kept = append(kept, r)
			}
		}
		rf.Resources = kept
	}

	def := &generator.Definition{ResourceFile: rf}
	// Synthesize CRUD actions from any resource schemas before generating.
	def.ExpandResourceSchemas()
	input := &generator.Input{Definition: def}

	type genFile struct{ path, content string }
	var files []genFile
	for _, f := range generator.GenerateGatedAdapters(input) {
		files = append(files, genFile{f.Path, f.Content})
	}
	for _, f := range generator.GenerateClientDispatch(input) {
		files = append(files, genFile{f.Path, f.Content})
	}
	// The router + usage enumerate every resource, so only (re)write them on a
	// full run — a name-filtered run would otherwise clobber them with partials.
	if filter == nil {
		if rf := generator.GenerateClientRouter(input); rf.Content != "" {
			files = append(files, genFile{rf.Path, rf.Content})
		}
		uf := generator.GenerateClientUsage(input)
		files = append(files, genFile{uf.Path, uf.Content})
		// Peering is top-level (not resource-scoped), so only on a full run.
		for _, f := range generator.GeneratePeering(input) {
			files = append(files, genFile{f.Path, f.Content})
		}
		// register_gen.go is one file covering all resources -> full run only.
		for _, f := range generator.GenerateServiceRegister(input) {
			files = append(files, genFile{f.Path, f.Content})
		}
		// stream_gen.go (StreamResource router) — one file, full run only.
		if sf := generator.GenerateClientStream(input); sf.Content != "" {
			files = append(files, genFile{sf.Path, sf.Content})
		}
	}
	for _, f := range generator.GenerateReconcile(input) {
		files = append(files, genFile{f.Path, f.Content})
	}
	// Generated declarative backing stores (opted in via schema.generated_store) —
	// resource-scoped, so safe on a name-filtered run too.
	for _, f := range generator.GenerateStore(input) {
		files = append(files, genFile{f.Path, f.Content})
	}
	if len(files) == 0 {
		log.Fatalf("no rpc_service resources matched")
	}
	for _, f := range files {
		formatted, err := format.Source([]byte(f.content))
		if err != nil {
			log.Fatalf("gofmt %s: %v", f.path, err)
		}
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			log.Fatalf("mkdir for %s: %v", f.path, err)
		}
		if err := os.WriteFile(f.path, formatted, 0o644); err != nil {
			log.Fatalf("write %s: %v", f.path, err)
		}
		log.Printf("generated %s", f.path)
	}
}
