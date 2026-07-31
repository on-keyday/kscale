// Command gen_access regenerates the core access artifacts from the resource
// declarations + policies, making access/defs/resource.yaml the single source of
// truth (kscale charter — resource-centric codegen). It mirrors ksdk's
// cmd/gen_access but only the modes kscale uses (no config/* — kscale has no
// config package; gate adapters + client dispatch live in cmd/gen).
//
//	gen_access <mode> <resource.yaml> <policy-dir>
//
// Modes (output is written to stdout; generate.sh redirects to the right file):
//
//	resource         -> access/predefined/root.go        (Resources/consts/PolicyMap)
//	proto            -> protobuf/proto/access/access.proto
//	proto_services   -> protobuf/proto/admin_rpc_services.proto
//	proto_registry   -> protobuf/proto/access/registry.go
package main

import (
	"io/fs"
	"log"
	"os"

	"github.com/goccy/go-yaml"
	"github.com/on-keyday/kscale/access/generator"
)

func main() {
	if len(os.Args) < 4 {
		log.Fatalf("usage: %s <mode> <resource.yaml> <policy-dir>", os.Args[0])
	}
	mode := os.Args[1]
	resourceFile := os.Args[2]
	policyDir := os.Args[3]

	data, err := os.ReadFile(resourceFile)
	if err != nil {
		log.Fatalf("read %s: %v", resourceFile, err)
	}
	var rf generator.ResourceFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		log.Fatalf("unmarshal %s: %v", resourceFile, err)
	}
	def := generator.Definition{ResourceFile: rf}

	err = fs.WalkDir(os.DirFS(policyDir), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fileData, err := os.ReadFile(policyDir + "/" + path)
		if err != nil {
			return err
		}
		var policy generator.Policy
		if err := yaml.Unmarshal(fileData, &policy); err != nil {
			return err
		}
		def.Policies = append(def.Policies, policy)
		return nil
	})
	if err != nil {
		log.Fatalf("walk policy dir %s: %v", policyDir, err)
	}

	// Synthesize CRUD actions from any resource schemas before generating.
	def.ExpandResourceSchemas()

	input := &generator.Input{Definition: &def}
	switch mode {
	case "resource":
		generator.GenerateDefinition(&generator.Writer{W: os.Stdout}, input)
	case "proto":
		generator.GenerateProtoDefinition(os.Stdout, input)
	case "proto_services":
		generator.GenerateProtoServices(os.Stdout, input)
	case "proto_registry":
		generator.GenerateProtoRegistry(os.Stdout, input)
	default:
		log.Fatalf("unknown mode: %s", mode)
	}
}
