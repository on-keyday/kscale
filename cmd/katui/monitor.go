package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/kscale/client"
	"github.com/on-keyday/kscale/consts"
	pb "github.com/on-keyday/kscale/protobuf/proto"
	pbaccess "github.com/on-keyday/kscale/protobuf/proto/access"
	"github.com/on-keyday/kscale/protobuf/proto/stat"
)

// Monitor mode is a live fleet dashboard (the one good thing ksdk's console TUI had that
// katui lacked): one row per dataplane node, merged from two existing server-streams —
//   - dataplane_node watch (one-shot snapshot, re-fetched on a tick): membership, status,
//     remote address, reconcile_errors — so STOPPED nodes (which emit no stats) still show.
//   - stats watch (continuous): RTT / CPU / Mem / Load and a per-dp-type app summary.
// Keyed by common_name; the node snapshot is authoritative for membership (prunes gone
// nodes), stats just decorate.

// fleetRow is one node's merged live state.
type fleetRow struct {
	cn     string
	node   string // short label (nodeLabel of cn)
	dpType string
	connID string // transport connection_id — routes the remote-shell EXEC stream to this node
	status string
	remote string
	rtt    string
	cpu    string
	mem    string
	load   string
	app    string
	errs   []string
}

type monStatsMsg struct {
	batch *stat.StatBatch
	err   error
	done  bool
}

type monNodesMsg struct {
	nodes []*pbaccess.ResourceDataplaneNodeActionWatchResponseDTO
	err   error
}

type monTickMsg struct{}

// monAlertsMsg carries the latest active-alert snapshot (re-fetched on the tick).
type monAlertsMsg struct {
	alerts []*pbaccess.ResourceAlertActionGetResponseDTO
	err    error
}

// monLogMsg is one line of the ambient log feed (or its termination). source tags the
// stream it came from ("cp" for the control plane; "" for the node fan-out, whose records
// already carry a "node" attr).
type monLogMsg struct {
	line   string
	source string
	done   bool
	err    error
}

// monActionMsg is the result of a start/stop issued from the dashboard.
type monActionMsg struct {
	out string
	err error
}

const monLogLimit = 200 // tail this many CP log lines
const monLogPage = 10   // logical lines per PgUp/PgDn step in the log feed

// clampMonLogScroll keeps the log scrollback offset within [0, len(monLog)-1].
func (m *model) clampMonLogScroll() {
	if max := len(m.monLog) - 1; m.monLogScroll > max {
		m.monLogScroll = max
	}
	if m.monLogScroll < 0 {
		m.monLogScroll = 0
	}
}

// startMonitor resets the dashboard state and kicks off the stats stream goroutine, the
// first node snapshot, and the refresh tick. Returned as the cmd when entering the mode.
func (m *model) startMonitor() tea.Cmd {
	sctx, cancel := context.WithCancel(m.ctx)
	m.monCancel = cancel
	m.monRows = map[string]*fleetRow{}
	m.monOrder = nil
	m.monCur = 0
	m.monLog = nil
	m.monAlerts = nil
	m.monStatus = ""
	m.monErr = ""
	ch := make(chan monStatsMsg, 16)
	m.monStatCh = ch
	// Deep buffer so a backlog burst coalesces into few waitMonLog batches
	// (producers keep filling while a render is in flight).
	logCh := make(chan monLogMsg, 512)
	m.monLogCh = logCh
	src := m.src
	go func() {
		c := pb.NewStatsServiceClient(src)
		stream, err := c.Watch(sctx, &pbaccess.ResourceStatsActionWatchArgsDTO{CommonName: "*"})
		if err != nil {
			trySend(ch, monStatsMsg{err: err, done: true}, sctx)
			return
		}
		for {
			item, err := stream.Recv(sctx)
			if errors.Is(err, io.EOF) {
				trySend(ch, monStatsMsg{done: true}, sctx)
				return
			}
			if err != nil {
				trySend(ch, monStatsMsg{err: err, done: true}, sctx)
				return
			}
			if !trySend(ch, monStatsMsg{batch: item}, sctx) {
				return
			}
		}
	}()
	// Ambient log feed: TWO streams merged — the control plane's own log (no common_name,
	// tagged "cp") and the fan-out of every dataplane node ("*", each record already tagged
	// with a "node" attr). logs stream resolves cp and nodes separately, so both are run.
	logStream := func(commonName, source string) {
		err := client.StreamResource(sctx, src, "logs", "stream", map[string]string{"common_name": commonName}, func(line string) error {
			select {
			case logCh <- monLogMsg{line: line, source: source}:
				return nil
			case <-sctx.Done():
				return sctx.Err()
			}
		})
		select {
		case logCh <- monLogMsg{done: true, err: err}:
		case <-sctx.Done():
		}
	}
	go logStream("", "cp")
	go logStream("*", "")
	return tea.Batch(waitMonStats(ch), waitMonLog(logCh), m.fetchMonNodes(), m.fetchMonAlerts(), monTickCmd())
}

// monLogBatchMsg carries one burst of log lines as a single bubbletea message.
// The logs subscribe replays up to 200 backlog lines per source (cp + every
// node) the moment the monitor opens; delivered line-by-line that was one
// full-screen render per line — hundreds of sequential renders — which froze
// the TUI. Batching collapses a burst into one Update/render.
type monLogBatchMsg struct {
	msgs []monLogMsg
}

// waitMonLog blocks for the next log line, then drains everything already
// buffered on the channel into the same message (bounded, in case producers
// outpace the drain indefinitely).
func waitMonLog(ch chan monLogMsg) tea.Cmd {
	return func() tea.Msg {
		batch := monLogBatchMsg{msgs: []monLogMsg{<-ch}}
		for len(batch.msgs) < 1024 {
			select {
			case next := <-ch:
				batch.msgs = append(batch.msgs, next)
			default:
				return batch
			}
		}
		return batch
	}
}

// fetchMonAlerts pulls the current active-alert set from the control plane's evaluator.
func (m model) fetchMonAlerts() tea.Cmd {
	src, base := m.src, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 5*time.Second)
		defer cancel()
		c := pb.NewAlertServiceClient(src)
		resp, err := c.List(ctx, &pbaccess.ResourceAlertActionListArgsDTO{})
		if err != nil {
			return monAlertsMsg{err: err}
		}
		return monAlertsMsg{alerts: resp.Items}
	}
}

// startStopSelected issues a node start/stop for the currently selected fleet row.
func (m model) startStopSelected(start bool) tea.Cmd {
	if m.monCur < 0 || m.monCur >= len(m.monOrder) {
		return nil
	}
	cn := m.monRows[m.monOrder[m.monCur]].cn
	ctx, src := m.ctx, m.src
	action := "stop"
	if start {
		action = "start"
	}
	return func() tea.Msg {
		out, err := client.DispatchResource(ctx, src, "node", action, map[string]string{"common_name": cn})
		return monActionMsg{out: out, err: err}
	}
}

// onMonLog appends one log line (compacted to a single styled line) to the tail ring.
func (m *model) onMonLog(msg monLogMsg) {
	m.monLog = append(m.monLog, formatLogLine(msg.line, msg.source))
	// When the user has scrolled back, keep the viewport anchored on the same lines
	// as new ones stream in (offset counts from the newest end, so each append needs a
	// matching bump — otherwise the feed would slide forward under them). Trimming the
	// front below does not move a from-end offset.
	if m.monLogScroll > 0 {
		m.monLogScroll++
	}
	if len(m.monLog) > monLogLimit {
		m.monLog = m.monLog[len(m.monLog)-monLogLimit:]
	}
	m.clampMonLogScroll()
}

type logRec struct {
	TimeUnixNano int64  `json:"time_unix_nano"`
	Level        string `json:"level"`
	Message      string `json:"message"`
	Attrs        []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"attrs"`
}

// formatLogLine renders a streamed LogRecord JSON as one compact styled line:
// "[source] HH:MM:SS LEVEL message  k=v k=v". The leading tag is `source` ("cp") or, for a
// node fan-out record, the value of its "node" attr. Falls back to the raw line if it isn't
// a LogRecord.
func formatLogLine(raw, source string) string {
	var r logRec
	if err := json.Unmarshal([]byte(raw), &r); err != nil || (r.Message == "" && r.Level == "") {
		return strings.Join(strings.Fields(raw), " ")
	}
	tag := source
	var attrs strings.Builder
	for _, a := range r.Attrs {
		if a.Key == "node" && tag == "" { // promote the fan-out's node tag to the leading label
			tag = a.Value
			continue
		}
		attrs.WriteString(" " + a.Key + "=" + a.Value)
	}
	ts := ""
	if r.TimeUnixNano > 0 {
		ts = time.Unix(0, r.TimeUnixNano).Format("15:04:05") + " "
	}
	lvl := r.Level
	switch r.Level {
	case "ERROR":
		lvl = stErr.Render(lvl)
	case "WARN":
		lvl = stWarn.Render(lvl)
	default:
		lvl = stMuted.Render(lvl)
	}
	label := ""
	if tag != "" {
		label = stTitle.Render(padTrunc(tag, 12)) + " " // fits "<node>.<dp_type>" e.g. s2.popcache
	}
	return label + stMuted.Render(ts) + lvl + " " + r.Message + stMuted.Render(attrs.String())
}

func trySend[T any](ch chan T, msg T, ctx context.Context) bool {
	select {
	case ch <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *model) stopMonitor() {
	if m.monCancel != nil {
		m.monCancel()
		m.monCancel = nil
	}
	m.monStatCh = nil
}

func waitMonStats(ch chan monStatsMsg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func monTickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return monTickMsg{} })
}

// fetchMonNodes runs the (one-shot) dataplane_node watch snapshot to completion and
// returns the full node set.
func (m model) fetchMonNodes() tea.Cmd {
	src, base := m.src, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 5*time.Second)
		defer cancel()
		c := pb.NewDataplaneNodeServiceClient(src)
		stream, err := c.Watch(ctx, &pbaccess.ResourceDataplaneNodeActionWatchArgsDTO{})
		if err != nil {
			return monNodesMsg{err: err}
		}
		var nodes []*pbaccess.ResourceDataplaneNodeActionWatchResponseDTO
		for {
			item, err := stream.Recv(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return monNodesMsg{err: err}
			}
			nodes = append(nodes, item)
		}
		return monNodesMsg{nodes: nodes}
	}
}

// onMonNodes folds a node snapshot in: membership is authoritative, so rows for nodes not
// in the snapshot are pruned (disconnected).
func (m *model) onMonNodes(msg monNodesMsg) {
	if msg.err != nil {
		m.monErr = msg.err.Error()
		return
	}
	m.monErr = ""
	seen := make(map[string]bool, len(msg.nodes))
	for _, n := range msg.nodes {
		seen[n.CommonName] = true
		r := m.monRows[n.CommonName]
		if r == nil {
			r = &fleetRow{cn: n.CommonName}
			m.monRows[n.CommonName] = r
		}
		r.node = nodeLabel(n.CommonName)
		r.dpType = n.DpType
		r.connID = n.ConnectionId
		r.status = n.AppStatus
		r.remote = n.RemoteAddress
		r.errs = n.ReconcileErrors
	}
	for cn := range m.monRows {
		if !seen[cn] {
			delete(m.monRows, cn)
		}
	}
	m.rebuildMonOrder()
}

// onMonStats overlays one stats batch onto the existing rows (decoration only).
func (m *model) onMonStats(msg monStatsMsg) {
	if msg.batch == nil {
		return
	}
	for _, s := range msg.batch.Stats {
		if s.CommonName == "" {
			continue
		}
		r := m.monRows[s.CommonName]
		if r == nil {
			r = &fleetRow{cn: s.CommonName, node: nodeLabel(s.CommonName)}
			m.monRows[s.CommonName] = r
			m.rebuildMonOrder()
		}
		if s.DpType != "" {
			r.dpType = s.DpType
		}
		if s.Connection != nil {
			r.rtt = fmtRTT(s.Connection.Rtt)
		}
		if s.HostRealtime != nil {
			r.cpu = fmtCPU(s.HostRealtime.CpuUsages)
			r.mem = fmtBytes(s.HostRealtime.MemoryUsage)
			r.load = fmtLoad(s.HostRealtime.LoadAvg)
		}
		if a := appSummary(s); a != "" {
			r.app = a
		}
		if r.status == "" && s.CdnAppRealtime != nil && s.CdnAppRealtime.Appstat != nil {
			r.status = consts.AppStatus(*s.CdnAppRealtime.Appstat).String()
		}
	}
}

func (m *model) rebuildMonOrder() {
	m.monOrder = m.monOrder[:0]
	for cn := range m.monRows {
		m.monOrder = append(m.monOrder, cn)
	}
	// stable display: by dp_type then node label.
	sort.Slice(m.monOrder, func(i, j int) bool {
		a, b := m.monRows[m.monOrder[i]], m.monRows[m.monOrder[j]]
		if a.dpType != b.dpType {
			return a.dpType < b.dpType
		}
		return a.node < b.node
	})
	if m.monCur >= len(m.monOrder) {
		m.monCur = len(m.monOrder) - 1
	}
	if m.monCur < 0 {
		m.monCur = 0
	}
}

// monitorView renders the full-width live dashboard: a selectable fleet table on top, a
// start/stop status line, and an ambient CP-log feed filling the rest.
func (m model) monitorView() string {
	up := 0
	for _, cn := range m.monOrder {
		if m.monRows[cn].status == consts.AppStatusRunning.String() {
			up++
		}
	}
	var b strings.Builder
	used := 0
	wl := func(s string) { b.WriteString(s + "\n"); used++ }

	crit := 0
	for _, a := range m.monAlerts {
		if a.Severity == "critical" {
			crit++
		}
	}
	head := "  " + stTitle.Render("MONITOR — fleet (live)") + stMuted.Render(fmt.Sprintf("    %d nodes · %d up", len(m.monOrder), up))
	if n := len(m.monAlerts); n > 0 {
		badge := stWarn.Render(fmt.Sprintf("⚠ %d alert(s)", n))
		if crit > 0 {
			badge = stErr.Render(fmt.Sprintf("✖ %d alert(s), %d critical", n, crit))
		}
		head += "    " + badge
	}
	wl("")
	wl(head)
	wl("")
	if m.monErr != "" {
		wl("  " + stErr.Render("node snapshot error: "+m.monErr))
	}
	// Active alerts (most important first) — the deterministic evaluator's output.
	for _, a := range m.monAlerts {
		mark, st := "⚠", stWarn
		if a.Severity == "critical" {
			mark, st = "✖", stErr
		}
		age := ""
		if a.Age != "" {
			age = stMuted.Render("  (" + a.Age + ")")
		}
		wl("  " + st.Render(mark+" "+padTrunc(a.Node, 12)) + " " + a.Message + age)
	}
	if len(m.monAlerts) > 0 {
		wl("")
	}

	// columns: ▶ Node Type Status RTT CPU Mem Load App
	wl(stMuted.Render("  " + padTrunc("Node", 12) + padTrunc("Type", 10) + padTrunc("Status", 12) +
		padTrunc("RTT", 9) + padTrunc("CPU", 6) + padTrunc("Mem", 8) + padTrunc("Load", 7) + "App"))

	var errLines []string
	if len(m.monOrder) == 0 {
		wl("  " + stMuted.Render("(no dataplane nodes connected)"))
	}
	for i, cn := range m.monOrder {
		r := m.monRows[cn]
		cur := "  "
		if i == m.monCur {
			cur = stTitle.Render("▶ ")
		}
		wl(cur + padTrunc(r.node, 12) + padTrunc(r.dpType, 10) +
			monStatusCell(r.status, 12) +
			padTrunc(dash(r.rtt), 9) + padTrunc(dash(r.cpu), 6) +
			padTrunc(dash(r.mem), 8) + padTrunc(dash(r.load), 7) + dash(r.app))
		for _, e := range r.errs {
			errLines = append(errLines, "  "+stErr.Render("! "+r.node+"  "+e))
		}
	}
	for _, e := range errLines {
		wl(e)
	}
	if m.shellPrompt {
		node := ""
		if m.monCur >= 0 && m.monCur < len(m.monOrder) {
			node = m.monRows[m.monOrder[m.monCur]].node
		}
		wl("  " + stTitle.Render("shell on "+node+" ▶") + m.shellInput.View() +
			stMuted.Render("  (enter run · esc cancel)"))
	} else if m.monStatus != "" {
		wl("  " + m.monStatus)
	}

	// Ambient CP log feed fills the rest of the body.
	wl("")
	sc := m.monLogScroll
	if max := len(m.monLog) - 1; sc > max {
		sc = max
	}
	if sc < 0 {
		sc = 0
	}
	if sc > 0 {
		wl("  " + stWarn.Render(fmt.Sprintf("── log (scrolled ↑%d) — PgUp/PgDn scroll · G live ──", sc)))
	} else {
		wl("  " + stMuted.Render("── log (live) — cp + all nodes · PgUp scroll ──"))
	}
	used++ // account for the footer gap we leave
	budget := m.bodyH() - used
	if budget < 1 {
		budget = 1
	}
	// Hide the newest `sc` logical lines (scrollback), then soft-wrap the rest to the
	// pane width (ANSI-aware) so a long log line — a verbose message or a long k=v tail
	// — folds onto continuation rows instead of being clipped at the right edge. Budget
	// is counted in *physical* rows after wrapping so the feed never overruns the body.
	visible := m.monLog
	if sc > 0 && sc < len(visible) {
		visible = visible[:len(visible)-sc]
	}
	wrapW := m.w - 2 // the "  " indent
	rows := make([]string, 0, len(visible))
	for _, l := range visible {
		if wrapW > 0 {
			rows = append(rows, strings.Split(ansiWrap(l, wrapW), "\n")...)
		} else {
			rows = append(rows, l)
		}
	}
	if len(rows) > budget {
		rows = rows[len(rows)-budget:]
	}
	for _, r := range rows {
		b.WriteString("  " + r + "\n")
	}
	return b.String()
}

func monStatusCell(status string, w int) string {
	cell := padTrunc(status, w)
	switch status {
	case consts.AppStatusRunning.String():
		return stOK.Render(cell)
	case consts.AppStatusError.String():
		return stErr.Render(cell)
	case consts.AppStatusStopped.String():
		return stMuted.Render(cell)
	case consts.AppStatusInitialized.String():
		return stWarn.Render(cell)
	}
	return cell
}

// ---- formatting helpers ----

func padTrunc(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		if w <= 1 {
			return string(r[:w])
		}
		return string(r[:w-1]) + "…"
	}
	return s + strings.Repeat(" ", w-len(r))
}

// nodeLabel is the short node name — the CN up to the first dot ("s1.popcache.…" -> "s1").
func nodeLabel(cn string) string {
	if i := strings.IndexByte(cn, '.'); i >= 0 {
		return cn[:i]
	}
	return cn
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fmtRTT(rtt int64) string {
	if rtt <= 0 {
		return ""
	}
	return time.Duration(rtt).Round(time.Microsecond).String()
}

func fmtCPU(cpus []float64) string {
	if len(cpus) == 0 {
		return ""
	}
	var sum float64
	for _, c := range cpus {
		sum += c
	}
	return fmt.Sprintf("%.0f%%", sum/float64(len(cpus)))
}

func fmtLoad(load []float64) string {
	if len(load) == 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", load[0])
}

func fmtBytes(b uint64) string {
	if b == 0 {
		return ""
	}
	const u = 1024
	if b < u {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(b)/float64(div), "KMGTPE"[exp])
}

func short(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func appSummary(s *stat.Stats) string {
	switch s.DpType {
	case "popcache":
		if s.Popcache == nil {
			return ""
		}
		p := s.Popcache
		tot := p.CacheHitsTotal + p.CacheMissesTotal
		hit := 0.0
		if tot > 0 {
			hit = float64(p.CacheHitsTotal) / float64(tot) * 100
		}
		return fmt.Sprintf("req%s hit%.0f%%", short(p.RequestsTotal), hit)
	case "dns":
		if s.Dns == nil {
			return ""
		}
		return fmt.Sprintf("q%s e%s", short(s.Dns.RequestsTotal), short(s.Dns.ErrorsTotal))
	case "l4lb":
		if s.CdnAppRealtime != nil && s.CdnAppRealtime.LbId != nil {
			return fmt.Sprintf("lb%d", *s.CdnAppRealtime.LbId)
		}
	}
	return ""
}
