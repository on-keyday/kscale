package dev

import (
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/on-keyday/kscale/protobuf/proto"
	"github.com/scrapli/scrapligo/driver/network"
	"github.com/scrapli/scrapligo/driver/options"
	"github.com/scrapli/scrapligo/platform"
	"github.com/scrapli/scrapligo/transport"
	"golang.org/x/crypto/ssh"
)

// Client はルーターへの接続設定と排他ロックを管理します
type Client struct {
	Host     string
	Username string
	Password string
	VIP      []netip.Addr
	mu       sync.Mutex // 同時実行を防ぐためのロック
}

// NewClient は新しいRouter Clientを作成します
func NewClient(host, user, pass string) *Client {
	return &Client{
		Host:     host,
		Username: user,
		Password: pass,
	}
}

func (c *Client) SetHost(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Host = host
}

func (c *Client) SetUser(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Username = user
}
func (c *Client) SetPassword(pass string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Password = pass
}

func (c *Client) SetVIP(vip []netip.Addr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.VIP = vip
}

// SyncACL は指定されたACLの中身を指定されたポートリストと同期させます
// 失敗時の自動ロールバック機能(reload in 5)を含みます
func (c *Client) SyncACL(aclName string, desiredPorts []*pb.PortInfo, dryRun bool, logger *slog.Logger) (*pb.ACLDiff, error) {
	// 1. ロックを取得（他のgoroutineが操作中の場合は待つ）
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.VIP) == 0 {
		return nil, fmt.Errorf("router client: no VIP configured")
	}
	if len(c.VIP) > 1 {
		return nil, fmt.Errorf("router client: multiple VIPs configured, not supported")
	}

	var desiredAddrPorts []*pb.AddrPortInfo
	desiredAddrPorts = append(desiredAddrPorts, &pb.AddrPortInfo{
		Addr:  c.VIP[0],
		Ports: desiredPorts,
	})

	// 2. 接続確立
	d, err := c.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer d.Close()

	// 3. 現状確認
	currentInfos, err := c.getCurrentAclPorts(d, aclName, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to get current acl: %w", err)
	}

	// 4. 差分計算
	addCmds, removeCmds := c.calculateDiff(currentInfos, desiredAddrPorts)
	if len(addCmds) == 0 && len(removeCmds) == 0 {
		// 変更なし
		return &pb.ACLDiff{
			Add:    []string{},
			Remove: []string{},
		}, nil
	}
	if dryRun {
		// DryRunモード: 変更内容を返すだけ
		return &pb.ACLDiff{
			Add:    addCmds,
			Remove: removeCmds,
		}, nil
	}

	// 5. 安全装置セット (Reload in 5)
	if err := c.setReloadTimer(d, 5); err != nil {
		return nil, fmt.Errorf("failed to set safety reload timer: %w", err)
	}

	// 6. 設定変更実行
	// ACLコンフィグ作成
	var configLines []string
	configLines = append(configLines, fmt.Sprintf("ip access-list extended %s", aclName))
	configLines = append(configLines, removeCmds...) // 削除を先に実行
	configLines = append(configLines, addCmds...)    // 追加を後に実行

	logger.Info("[Router] Applying changes to ACL", "host", c.Host, "acl", aclName, "adds", addCmds, "removes", removeCmds)
	_, err = d.SendConfigs(configLines)
	if err != nil {
		// 設定投入失敗
		// reload cancel は実行せず、呼び出し元にエラーを返す（ルーターは5分後に再起動して戻る）
		return nil, fmt.Errorf("failed to apply config (router will reload in 5 min): %w", err)
	}

	logger.Info("[Router] Config applied, waiting to confirm", "host", c.Host, "acl", aclName)

	// 7. 疎通確認 (簡易チェック)
	// 特権モードが維持できているか確認
	if _, err := d.SendCommand("show privilege"); err != nil {
		return nil, fmt.Errorf("lost connection after config (router will reload in 5 min): %w", err)
	}

	logger.Info("[Router] Config confirmed, cancelling reload", "host", c.Host, "acl", aclName)

	// 8. 安全装置解除 (Reload Cancel) & 保存
	if err := c.cancelReloadAndSave(d); err != nil {
		return nil, fmt.Errorf("failed to cancel reload or save: %w", err)
	}

	return &pb.ACLDiff{
		Add:    addCmds,
		Remove: removeCmds,
	}, nil
}

// 内部メソッド: 接続
func (c *Client) connect() (*network.Driver, error) {
	keyExchanges := []string{
		ssh.InsecureKeyExchangeDH14SHA1,
		ssh.InsecureKeyExchangeDH1SHA1,
		ssh.InsecureKeyExchangeDHGEXSHA1,
	}
	p, err := platform.NewPlatform(
		"cisco_iosxe",
		c.Host,
		options.WithTransportType(transport.StandardTransport),
		options.WithAuthNoStrictKey(),
		options.WithAuthUsername(c.Username),
		options.WithAuthPassword(c.Password),
		// 古い機器向けにタイムアウトを長めに設定
		options.WithTimeoutSocket(30*time.Second),
		options.WithStandardTransportExtraKexs(keyExchanges),
	)
	if err != nil {
		return nil, err
	}

	d, err := p.GetNetworkDriver()
	if err != nil {
		return nil, err
	}

	if err = d.Open(); err != nil {
		return nil, err
	}

	return d, nil
}

func (c *Client) GetCurrentACLPorts(aclName string, logger *slog.Logger) ([]*pb.AddrPortInfo, error) {
	// 1. ロックを取得（他のgoroutineが操作中の場合は待つ）
	c.mu.Lock()
	defer c.mu.Unlock()

	// 2. 接続確立
	d, err := c.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	defer d.Close()

	// 3. 現状確認
	return c.getCurrentAclPorts(d, aclName, logger)
}

// 内部メソッド: ACL現状取得
func (c *Client) getCurrentAclPorts(d *network.Driver, aclName string, logger *slog.Logger) ([]*pb.AddrPortInfo, error) {
	cmd := fmt.Sprintf("show access-lists %s", aclName)
	resp, err := d.SendCommand(cmd)
	if err != nil {
		return nil, err
	}
	return c.parseCurrentAclResponse(resp.Result, logger)
}

// 内部メソッド: ACLレスポンス解析
func (c *Client) parseCurrentAclResponse(resp string, logger *slog.Logger) ([]*pb.AddrPortInfo, error) {
	var ports []*pb.AddrPortInfo
	re := regexp.MustCompile(`permit (tcp|udp) any host ([^\s]+) eq\s+(\w+)`)
	// icmp echo permits (VIP ping 用) は eq <port> を持たない別形式。parse できない行は
	// diff から見えず「消せない・毎 sync 再 add」になるので、管理対象の形は必ず拾う。
	reIcmp := regexp.MustCompile(`permit (icmp) any host ([^\s]+) echo(\s|$)`)

	add := func(addr netip.Addr, proto string, portNum uint16) {
		for p := range ports {
			if ports[p].Addr == addr {
				ports[p].Ports = append(ports[p].Ports, &pb.PortInfo{Port: portNum, Protocol: proto})
				return
			}
		}
		ports = append(ports, &pb.AddrPortInfo{
			Addr:  addr,
			Ports: []*pb.PortInfo{{Port: portNum, Protocol: proto}},
		})
	}

	lines := strings.Split(resp, "\n")
	for _, line := range lines {
		//logger.Info("[Router] ACL Line", "line", line) // デバッグ用ログ
		if matches := re.FindStringSubmatch(line); len(matches) > 3 {
			addr, err := netip.ParseAddr(matches[2])
			if err != nil {
				continue
			}
			portNum, ok := c.portNameToNum(matches[3])
			if !ok {
				continue
			}
			add(addr, matches[1], uint16(portNum))
		} else if matches := reIcmp.FindStringSubmatch(line); len(matches) > 2 {
			addr, err := netip.ParseAddr(matches[2])
			if err != nil {
				continue
			}
			add(addr, "icmp", 0)
		}
	}
	return ports, nil
}

// aclEntry renders one permit line for a PortInfo: icmp (Port-0 sentinel) has no
// port and permits echo requests only; tcp/udp use the eq <port> form.
func aclEntry(addr string, p *pb.PortInfo) string {
	if p.Protocol == "icmp" {
		return fmt.Sprintf("permit icmp any host %s echo", addr)
	}
	return fmt.Sprintf("permit %s any host %s eq %d", p.Protocol, addr, p.Port)
}

// 内部メソッド: 差分計算
func (c *Client) calculateDiff(current []*pb.AddrPortInfo, desired []*pb.AddrPortInfo) (adds []string, removes []string) {
	// 削除リスト作成
	for _, cur := range current {
		addrExists := false
		for _, des := range desired {
			if cur.Addr != des.Addr {
				continue
			}
			addrExists = true
			for _, curPort := range cur.Ports {
				exists := false
				for _, desPort := range des.Ports {
					if curPort.Port == desPort.Port && curPort.Protocol == desPort.Protocol {
						exists = true
						break
					}
				}
				if !exists {
					removes = append(removes, "no "+aclEntry(cur.Addr.String(), curPort))
				}
			}
		}
		if !addrExists {
			// アドレス自体が存在しない場合、全ポートを削除
			for _, curPort := range cur.Ports {
				removes = append(removes, "no "+aclEntry(cur.Addr.String(), curPort))
			}
		}
	}

	// 追加リスト作成
	for _, des := range desired {
		exists := false
		for _, cur := range current {
			if des.Addr == cur.Addr {
				exists = true
				for _, desPort := range des.Ports {
					portExists := false
					for _, curPort := range cur.Ports {
						if desPort.Port == curPort.Port && desPort.Protocol == curPort.Protocol {
							portExists = true
							break
						}
					}
					if !portExists {
						adds = append(adds, aclEntry(des.Addr.String(), desPort))
					}
				}
				break
			}
		}
		if !exists {
			// アドレス自体が存在しない場合、全ポートを追加
			for _, desPort := range des.Ports {
				adds = append(adds, aclEntry(des.Addr.String(), desPort))
			}
		}
	}
	return adds, removes
}

// 内部メソッド: Reload予約
func (c *Client) setReloadTimer(d *network.Driver, minutes int) error {
	// write memory
	if _, err := d.SendCommand("write memory"); err != nil {
		return err
	}
	// reload in X
	cmd := fmt.Sprintf("reload in %d\n", minutes) // 改行を送って確認プロンプトをスキップ
	_, err := d.SendCommand(cmd)
	return err
}

// 内部メソッド: Reloadキャンセルと保存
func (c *Client) cancelReloadAndSave(d *network.Driver) error {
	if _, err := d.SendCommand("reload cancel"); err != nil {
		return err
	}
	if _, err := d.SendCommand("write memory"); err != nil {
		return err
	}
	return nil
}

// ヘルパー: ポート名変換
func (c *Client) portNameToNum(s string) (int, bool) {
	if num, err := strconv.Atoi(s); err == nil {
		return num, true
	}
	switch s {
	case "www":
		return 80, true
	case "https", "443":
		return 443, true
	case "ssh":
		return 22, true
	case "telnet":
		return 23, true
	// 必要に応じて追加
	default:
		return 0, false
	}
}
