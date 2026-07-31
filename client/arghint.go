package client

// ArgTypeHint renders an arg's type for human-facing usage / TUI forms. A []byte arg's
// value is a LOCAL FILE PATH — the generated dispatch reads that file's bytes via
// os.ReadFile (e.g. cplane-file upload --content <path>) — so show "local file path"
// rather than the misleading raw "[]byte".
func ArgTypeHint(t string) string {
	switch t {
	case "[]byte":
		return "local file path"
	case "[]string":
		return `comma-separated list (or JSON array '["a","b"]')`
	}
	return t
}
