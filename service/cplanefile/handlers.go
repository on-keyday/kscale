// Package cplanefile holds the hand-written business behind CplaneFileService —
// the control plane's local file store. Files are staged here (upload), listed,
// and deleted; DpFile.send later pushes a staged file to a dataplane node. The
// store is a flat directory; file names are basename-sanitized to prevent path
// traversal.
package cplanefile

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
)

type Store struct {
	dir      string
	mu       sync.Mutex
	onUpload []func(name string)
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// OnUpload registers a callback fired (with the basename) after each successful Upload —
// i.e. whenever a staged file's content changes. The node_file reconcile subscribes so it
// can re-ship a cplane file to its nodes when the source bytes are updated. Reload of any
// consumer (e.g. l4lb) is intentionally NOT chained here; re-applying that resource is the
// operator's call.
func (s *Store) OnUpload(fn func(name string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onUpload = append(s.onUpload, fn)
}

func (s *Store) notifyUpload(name string) {
	s.mu.Lock()
	fns := append([]func(string){}, s.onUpload...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(name)
	}
}

// Path returns the on-disk path of a stored file (basename-sanitized).
func (s *Store) Path(name string) string { return filepath.Join(s.dir, filepath.Base(name)) }

// Read returns a staged file's content (used by DpFile.send).
func (s *Store) Read(name string) ([]byte, error) { return os.ReadFile(s.Path(name)) }

type Handlers struct {
	pb.UnimplementedCplaneFileServiceServer
	Store *Store
}

var _ pb.CplaneFileServiceServer = (*Handlers)(nil)

func (h *Handlers) Upload(ctx context.Context, req *pbaccess.ResourceCplaneFileActionUploadArgsDTO) (*pbaccess.ResourceCplaneFileActionUploadResponseDTO, error) {
	name := filepath.Base(req.FileName)
	if err := os.WriteFile(h.Store.Path(name), req.Content, 0o644); err != nil {
		return nil, err
	}
	h.Store.notifyUpload(name)
	return &pbaccess.ResourceCplaneFileActionUploadResponseDTO{FileName: name}, nil
}

func (h *Handlers) List(ctx context.Context, _ *pbaccess.ResourceCplaneFileActionListArgsDTO) (*pbaccess.ResourceCplaneFileActionListResponseDTO, error) {
	entries, err := os.ReadDir(h.Store.dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return &pbaccess.ResourceCplaneFileActionListResponseDTO{Files: files}, nil
}

func (h *Handlers) Delete(ctx context.Context, req *pbaccess.ResourceCplaneFileActionDeleteArgsDTO) (*pbaccess.ResourceCplaneFileActionDeleteResponseDTO, error) {
	name := filepath.Base(req.FileName)
	if err := os.Remove(h.Store.Path(name)); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return &pbaccess.ResourceCplaneFileActionDeleteResponseDTO{FileName: name}, nil
}
