package proto

import (
	"fmt"
	"strconv"
	"strings"
)

// toArgable は ToArg() を持つ message 型 (cli_format 対象) を受け入れる。
type toArgable interface {
	ToArg() string
}

// ArrayToPolicyArg は cli_format 対象型の slice を policy 比較用 []string に
// 変換する。rpc/stub の同名関数の pb 版。
func ArrayToPolicyArg[T toArgable](arr []T) []string {
	var result []string
	for _, item := range arr {
		result = append(result, item.ToArg())
	}
	return result
}

// ToPolicyArg は cli_format 対象 message 一つを policy 文字列に変換する。
func ToPolicyArg[T toArgable](obj T) string {
	return obj.ToArg()
}

// PortInfo CLI helpers — derived from the (ksdk.cli_format) = true field
// option on protobuf/proto/router_service.proto.

func (r *PortInfo) AppendString(sb *strings.Builder) {
	sb.WriteString(fmt.Sprintf("%s:%d", r.Protocol, r.Port))
}

func (r *PortInfo) String() string {
	var sb strings.Builder
	r.AppendString(&sb)
	return sb.String()
}

func (r *PortInfo) ToCLIOutput() string {
	return r.String()
}

func (r *PortInfo) ToArg() string {
	return r.String()
}

// FromArg parses "<protocol>:<port>" (e.g. "tcp:80") into the receiver.
func (r *PortInfo) FromArg(arg string) error {
	splited := strings.Split(arg, ":")
	if len(splited) != 2 {
		return fmt.Errorf("invalid port format: %s (expected protocol:port)", arg)
	}
	port, err := strconv.ParseUint(splited[1], 10, 16)
	if err != nil {
		return fmt.Errorf("invalid port format: %s: %w", arg, err)
	}
	r.Protocol = splited[0]
	r.Port = uint16(port)
	return nil
}

// WasmInfo CLI helpers.

func (w *WasmInfo) AppendString(sb *strings.Builder) {
	sb.WriteString(fmt.Sprintf("WasmInfo {id=%d, binary_path=%q, binary_size=%d, method=%q, path=%q}",
		w.Id, w.BinaryPath, w.BinarySize, w.Method, w.Path))
}

// WasmInfoArrayFromPeer CLI helpers.

func (p *WasmInfoArrayFromPeer) AppendString(sb *strings.Builder) {
	sb.WriteString("WasmInfoArrayFromPeer {peer=")
	sb.WriteString(p.Peer.String())
	sb.WriteString(", info=[")
	for i, v := range p.Info {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

// WasmListResponse CLI helpers (agent.ResponseDTO 互換)。

func (r *WasmListResponse) AppendString(sb *strings.Builder) {
	sb.WriteString("WasmListResponse {states=[")
	for i, v := range r.States {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

func (r *WasmListResponse) String() string {
	var sb strings.Builder
	r.AppendString(&sb)
	return sb.String()
}

func (r *WasmListResponse) ToCLIOutput() string {
	return r.String()
}

// FileInfo CLI helpers.

func (f *FileInfo) AppendString(sb *strings.Builder) {
	sb.WriteString(fmt.Sprintf("FileInfo {name=%q, size=%d, modified_at=%s, mode=%s}",
		f.Name, f.Size, f.ModifiedAt.Format("2006-01-02T15:04:05Z07:00"), f.Mode))
}

// FileInfoFromPeer CLI helpers.

func (p *FileInfoFromPeer) AppendString(sb *strings.Builder) {
	sb.WriteString("FileInfoFromPeer {peer=")
	sb.WriteString(p.Peer.String())
	sb.WriteString(", files=[")
	for i, v := range p.Files {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

// FilesList CLI helpers (agent.ResponseDTO 互換)。

func (r *FilesList) AppendString(sb *strings.Builder) {
	sb.WriteString("FilesList {files=[")
	for i, v := range r.Files {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

func (r *FilesList) String() string {
	var sb strings.Builder
	r.AppendString(&sb)
	return sb.String()
}

func (r *FilesList) ToCLIOutput() string {
	return r.String()
}

// SentPacket CLI helpers.

func (p *SentPacket) AppendString(sb *strings.Builder) {
	sb.WriteString(fmt.Sprintf("SentPacket {packet_type=%d, stream_id=%d, sent_at=%s, size=%d, is_mtu_probe=%t}",
		p.PacketType, p.StreamId, p.SentAt.Format("2006-01-02T15:04:05Z07:00"), p.Size, p.IsMtuProbe))
}

// TransferInternalState CLI helpers.

func (s *TransferInternalState) AppendString(sb *strings.Builder) {
	sb.WriteString(fmt.Sprintf("TransferInternalState {active_send_streams=%d, active_receive_streams=%d, current_mtu=%d, send_queue_length=%d, receive_queue_length=%d, send_action_count=%d, update_window_count=%d, cancel_stream_count=%d, bytes_in_flight=%d, congestion_window=%d, smoothed_rtt=%s, rtt_variance=%s",
		s.ActiveSendStreams, s.ActiveReceiveStreams, s.CurrentMtu,
		s.SendQueueLength, s.ReceiveQueueLength, s.SendActionCount,
		s.UpdateWindowCount, s.CancelStreamCount, s.BytesInFlight,
		s.CongestionWindow, s.SmoothedRtt, s.RttVariance))
	sb.WriteString(", sent_packets=[")
	for i, v := range s.SentPackets {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

// TransferInternalStateFromPeer CLI helpers.

func (p *TransferInternalStateFromPeer) AppendString(sb *strings.Builder) {
	sb.WriteString("TransferInternalStateFromPeer {peer=")
	sb.WriteString(p.Peer.String())
	sb.WriteString(", state=")
	if p.State != nil {
		p.State.AppendString(sb)
	} else {
		sb.WriteString("nil")
	}
	sb.WriteString("}")
}

// TransferInternalStates CLI helpers (agent.ResponseDTO 互換)。

func (r *TransferInternalStates) AppendString(sb *strings.Builder) {
	sb.WriteString("TransferInternalStates {states=[")
	for i, v := range r.States {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

func (r *TransferInternalStates) String() string {
	var sb strings.Builder
	r.AppendString(&sb)
	return sb.String()
}

func (r *TransferInternalStates) ToCLIOutput() string {
	return r.String()
}

// AddrPortInfo CLI helpers.

func (a *AddrPortInfo) AppendString(sb *strings.Builder) {
	sb.WriteString("AddrPortInfo {addr=")
	sb.WriteString(a.Addr.String())
	sb.WriteString(", ports=[")
	for i, v := range a.Ports {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

// AddrPortInfoArrayFromPeer CLI helpers.

func (p *AddrPortInfoArrayFromPeer) AppendString(sb *strings.Builder) {
	sb.WriteString("AddrPortInfoArrayFromPeer {peer=")
	sb.WriteString(p.Peer.String())
	sb.WriteString(", ports=[")
	for i, v := range p.Ports {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

// ListAddrPortResponse CLI helpers (agent.ResponseDTO 互換)。

func (r *ListAddrPortResponse) AppendString(sb *strings.Builder) {
	sb.WriteString("ListAddrPortResponse {states=[")
	for i, v := range r.States {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

func (r *ListAddrPortResponse) String() string {
	var sb strings.Builder
	r.AppendString(&sb)
	return sb.String()
}

func (r *ListAddrPortResponse) ToCLIOutput() string {
	return r.String()
}

// ACLDiff CLI helpers.

func (d *ACLDiff) AppendString(sb *strings.Builder) {
	sb.WriteString(fmt.Sprintf("ACLDiff {add=%v, remove=%v}", d.Add, d.Remove))
}

// ACLDiffFromPeer CLI helpers.

func (p *ACLDiffFromPeer) AppendString(sb *strings.Builder) {
	sb.WriteString("ACLDiffFromPeer {peer=")
	sb.WriteString(p.Peer.String())
	sb.WriteString(", diff=")
	if p.Diff != nil {
		p.Diff.AppendString(sb)
	} else {
		sb.WriteString("nil")
	}
	sb.WriteString("}")
}

// ACLDiffResponse CLI helpers (agent.ResponseDTO 互換)。

func (r *ACLDiffResponse) AppendString(sb *strings.Builder) {
	sb.WriteString("ACLDiffResponse {states=[")
	for i, v := range r.States {
		if i > 0 {
			sb.WriteString(", ")
		}
		v.AppendString(sb)
	}
	sb.WriteString("]}")
}

func (r *ACLDiffResponse) String() string {
	var sb strings.Builder
	r.AppendString(&sb)
	return sb.String()
}

func (r *ACLDiffResponse) ToCLIOutput() string {
	return r.String()
}

// --- agent.ResponseDTO compatibility (A4 phase 21) ---

func (r *WasmListResponse) ProtoTypeName() string             { return "WasmListResponse" }
func (r *WasmListResponse) EncodeAdminResponseBody() ([]byte, error) {
	return r.Append(nil)
}

func (r *FilesList) ProtoTypeName() string                    { return "FilesList" }
func (r *FilesList) EncodeAdminResponseBody() ([]byte, error) { return r.Append(nil) }

func (r *TransferInternalStates) ProtoTypeName() string       { return "TransferInternalStates" }
func (r *TransferInternalStates) EncodeAdminResponseBody() ([]byte, error) {
	return r.Append(nil)
}

func (r *ListAddrPortResponse) ProtoTypeName() string         { return "ListAddrPortResponse" }
func (r *ListAddrPortResponse) EncodeAdminResponseBody() ([]byte, error) {
	return r.Append(nil)
}

func (r *ACLDiffResponse) ProtoTypeName() string              { return "ACLDiffResponse" }
func (r *ACLDiffResponse) EncodeAdminResponseBody() ([]byte, error) {
	return r.Append(nil)
}

