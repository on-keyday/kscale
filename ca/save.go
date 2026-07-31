package ca

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/on-keyday/kscale/ca/storage"
	"github.com/on-keyday/objtrsf/objproto"
)

// returns (bootInfo, triedBootstrap, error)
func LoadOrBootstrapAndSave(ctx context.Context, sess objproto.Endpoint, dst objproto.ConnectionID, path string, password string, bootToken []byte, commonName string, app string) (*BootstrapInfo, bool, error) {
	tryBootstrap := func(origErr error) (*BootstrapInfo, bool, error) {
		if len(bootToken) == 0 {
			return nil, false, fmt.Errorf("%w: bootstrap token is required to bootstrap new key", origErr)
		}
		// ファイルが存在しない場合はブートストラップを実行
		bootInfo, err := BootstrapProtocol(ctx, dst, sess, "ed25519", commonName, bootToken, app)
		if err != nil {
			return nil, true, errors.Join(origErr, fmt.Errorf("bootstrap failed: %w", err))
		}
		dumped, err := bootInfo.DumpResult()
		if err != nil {
			return nil, true, errors.Join(origErr, err)
		}
		// ブートストラップ情報をファイルに保存
		err = storage.SavePasswordEncryptedDataToFile(path, dumped, password)
		if err != nil {
			return nil, true, errors.Join(origErr, err)
		}
		return bootInfo, true, nil
	}
	data, err := storage.LoadPasswordEncryptedDataFromFile(path, password)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, false, err
		}
		return tryBootstrap(err)
	}
	// ファイルが存在する場合はブートストラップ情報をロード
	bootInfo, err := LoadCertInfo(data)
	if err != nil {
		return tryBootstrap(err)
	}
	// check validity
	parsed, err := x509.ParseCertificate(bootInfo.SelfCert)
	if err != nil {
		return tryBootstrap(err)
	}
	now := time.Now()
	if parsed.NotBefore.After(now) || parsed.NotAfter.Before(now) {
		return tryBootstrap(fmt.Errorf("loaded certificate is not valid at current time"))
	}
	return bootInfo, false, nil
}
