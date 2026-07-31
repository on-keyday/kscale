package ca

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/on-keyday/kscale/ca/storage"
)

func DefaultMigrator(logger *slog.Logger, oldPath, currentPath, migrateDir string) func(old storage.CertificateStorage) (storage.CertificateStorage, error) {
	return func(old storage.CertificateStorage) (storage.CertificateStorage, error) {
		err := os.MkdirAll(migrateDir, 0700)
		if err != nil {
			return nil, err
		}
		tmpStorage, err := storage.MigrateDirStorage(old, migrateDir, nil, time.Now)
		if err != nil {
			return nil, err
		}
		err = os.Rename(currentPath, oldPath)
		if err != nil {
			return nil, err
		}
		err = storage.RenameDir(tmpStorage, currentPath)
		if err != nil {
			renameErr := os.Rename(oldPath, currentPath)
			if renameErr != nil {
				logger.Error("failed to rollback CA storage after migration failure", "error", renameErr)
				return nil, fmt.Errorf("migration failed: %v, rollback failed: %v", err, renameErr)
			}
			return nil, err
		}
		// migration successful, remove old storage
		old.Close()
		err = os.RemoveAll(oldPath)
		if err != nil {
			logger.Error("failed to remove old CA storage after migration", "error", err)
		}
		return tmpStorage, nil
	}
}
