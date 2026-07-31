package h2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/on-keyday/kscale/protobuf/wire/h2/hpack"
	"github.com/on-keyday/kscale/protobuf/wire/zerocopy"
)

type HTTP2Conn struct {
	globalLock  sync.Mutex // TODO: optimize locking granularity
	streams     map[uint32]*HTTP2Stream
	encHpack    *hpacker
	decHpack    *hpack.Table
	writeBuffer []byte
	writeCh     chan struct{}
	readBuffer  zerocopy.ZeroCopyBuffer

	headerBuffer     []byte
	localSettings    PredefinedSettings
	updatingSettings PredefinedSettings
	peerSettings     PredefinedSettings
	header           FrameHeaderState

	localWindow window // receiving limit
	peerWindow  window // sending limit

	continuationStreamID uint32
	nextStreamID         uint32 // this side initiated
	maxReceivedStreamID  uint32
	isClient             bool
	state                ParseState
	waitingSettingsAck   bool
	activeStreams        int // active streams that opened by this side

	acceptQueue []*HTTP2Stream
	acceptCh    chan struct{}

	inspectBody func(hdr *FrameHeader, body *FrameBody)
}

func (c *HTTP2Conn) SetFrameInspector(inspect func(hdr *FrameHeader, body *FrameBody)) {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	c.inspectBody = inspect
}

type HTTP2Stream struct {
	conn     *HTTP2Conn
	streamID uint32
	code     uint32
	state    H2StreamState

	localWindow    window // receiving limit
	peerWindow     window // sending limit
	windowUpdateCh chan struct{}

	sentHeader     http.Header
	receivedHeader http.Header
	streamData     []byte
	readCh         chan struct{}
}

func (c *HTTP2Stream) notifyWindowUpdate() {
	select {
	case c.windowUpdateCh <- struct{}{}:
	default:
	}
}

func (c *HTTP2Stream) notifyRead() {
	select {
	case c.readCh <- struct{}{}:
	default:
	}
}

func (c *HTTP2Stream) PeekPeerData() []byte {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	return bytes.Clone(c.streamData)
}

func (c *HTTP2Stream) TakePeerDataNonBlocking() []byte {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	data := c.streamData
	c.streamData = nil
	return data
}

func (c *HTTP2Stream) TakePeerData() ([]byte, error) {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	for len(c.streamData) == 0 {
		if c.state == H2StreamState_Close || c.state == H2StreamState_HalfClosedRemote {
			return nil, io.EOF
		}
		c.conn.globalLock.Unlock()
		<-c.readCh
		c.conn.globalLock.Lock()
	}
	data := c.streamData
	c.streamData = nil
	return data, nil
}

func (c *HTTP2Stream) setState(state H2StreamState) {
	c.state = state
	if state == H2StreamState_Close {
		c.notifyRead()
		c.notifyWindowUpdate()
		// Fully closed — prune from the frame-routing map so a long-lived
		// connection doesn't accumulate dead streams. Holders of the stream
		// pointer (TakePeerData / PeerHeader) are unaffected. Kept while a
		// CONTINUATION sequence for this stream is still in flight; the
		// continuation completion path prunes it instead.
		if c.conn.continuationStreamID != c.streamID {
			delete(c.conn.streams, c.streamID)
		}
	}
}

func (c *HTTP2Stream) Read(p []byte) (n int, err error) {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	for n < len(p) {
		if len(c.streamData) == 0 {
			if c.state == H2StreamState_Close || c.state == H2StreamState_HalfClosedRemote {
				return n, io.EOF
			}
			if n != 0 {
				return n, nil
			}
			c.conn.globalLock.Unlock()
			<-c.readCh
			c.conn.globalLock.Lock()
		} else {
			m := copy(p[n:], c.streamData)
			c.streamData = c.streamData[m:]
			n += m
		}
	}
	return n, nil
}

func (c *HTTP2Stream) PeerHeader() http.Header {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	return c.receivedHeader
}

func (c *HTTP2Stream) WriteHeader(hdr http.Header) (err error) {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	if c.state != H2StreamState_Idle && c.state != H2StreamState_Open && c.state != H2StreamState_HalfClosedRemote {
		return errors.New("stream is not in a state where headers can be written")
	}
	c.sentHeader = hdr
	defer func() {
		if err == nil && c.state == H2StreamState_Idle {
			c.setState(H2StreamState_Open)
		}
	}()
	return c.conn.writeHeader(c.streamID, func(emit func(hpack.KeyValue, hpack.AppendStringMode) error) error {
		// RFC 9113 §8.3: pseudo-header fields (":"-prefixed) MUST precede
		// regular fields in the header block. http.Header is a map, so its
		// range order is randomized — emit pseudo-headers first, then the
		// rest, or a peer will reject the block with PROTOCOL_ERROR.
		emitWhere := func(keep func(string) bool) error {
			for k, v := range hdr {
				if !keep(k) {
					continue
				}
				for _, vv := range v {
					if err := emit(hpack.KeyValue{Key: k, Value: vv}, hpack.BestEffort); err != nil {
						return err
					}
				}
			}
			return nil
		}
		isPseudo := func(k string) bool { return strings.HasPrefix(k, ":") }
		if err := emitWhere(isPseudo); err != nil {
			return err
		}
		return emitWhere(func(k string) bool { return !isPseudo(k) })
	})
}

func (c *HTTP2Stream) WriteHeaderOrdered(hdr []hpack.KeyValue, mode hpack.AppendStringMode) (err error) {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	if c.state != H2StreamState_Idle && c.state != H2StreamState_Open && c.state != H2StreamState_HalfClosedRemote {
		return errors.New("stream is not in a state where headers can be written")
	}
	defer func() {
		if err == nil && c.state == H2StreamState_Idle {
			c.setState(H2StreamState_Open)
		}
	}()
	return c.conn.writeHeader(c.streamID, func(emit func(hpack.KeyValue, hpack.AppendStringMode) error) error {
		for _, kv := range hdr {
			if err := emit(kv, mode); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *HTTP2Stream) WriteNonBlocking(p []byte) (n int, err error) {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	if c.state != H2StreamState_Open && c.state != H2StreamState_HalfClosedRemote {
		return 0, errors.New("stream is not open: " + c.state.String())
	}
	n, _, err = c.conn.writeData(c.streamID, &c.peerWindow, p, false)
	if err != nil {
		return n, err
	}
	return n, nil
}

func (c *HTTP2Stream) Write(p []byte) (n int, err error) {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	for n < len(p) {
		if c.state != H2StreamState_Open && c.state != H2StreamState_HalfClosedRemote {
			return n, fmt.Errorf("stream is not open: %s", c.state.String())
		}
		m, blocking, err := c.conn.writeData(c.streamID, &c.peerWindow, p[n:], false)
		if err != nil {
			return n, err
		}
		n += m
		if blocking != blockingReasonMaxFrameSize {
			c.conn.globalLock.Unlock()
			<-c.windowUpdateCh
			c.conn.globalLock.Lock()
		}
	}
	return n, nil
}

func (c *HTTP2Stream) Close() error {
	c.conn.globalLock.Lock()
	defer c.conn.globalLock.Unlock()
	if c.state != H2StreamState_Open && c.state != H2StreamState_HalfClosedRemote {
		return nil // already closed
	}
	_, _, err := c.conn.writeData(c.streamID, &c.peerWindow, nil, true)
	if err != nil {
		return err
	}
	switch c.state {
	case H2StreamState_Open:
		c.setState(H2StreamState_HalfClosedLocal)
	case H2StreamState_HalfClosedRemote:
		c.setState(H2StreamState_Close)
	}
	return nil
}

var defaultSettings = PredefinedSettings{
	TableSize:            4096,
	EnablePush:           true,
	MaxConcurrentStreams: 0, // unlimited
	InitialWindowSize:    65535,
	MaxFrameSize:         16384,
	MaxHeaderListSize:    0, // unlimited
}

type window struct {
	size int64
}

func (w *window) Available() int64 {
	return w.size
}

func (w *window) Consume(n int64) error {
	if w.size < n {
		return errors.New("flow control window exceeded")
	}
	w.size -= n
	return nil
}

func (w *window) Update(n int64) {
	w.size += n
}

type hpacker struct {
	encState    *hpack.Table
	hpackBuffer []byte
}

type headerEmitter = func(emit func(hpack.KeyValue, hpack.AppendStringMode) error) error

func (h *hpacker) PackHeader(hdr headerEmitter) ([]byte, error) {
	h.hpackBuffer = h.hpackBuffer[:0]
	err := hdr(func(kv hpack.KeyValue, mode hpack.AppendStringMode) error {
		buffer, err := hpack.AppendField(h.hpackBuffer, kv, mode, h.encState, nil)
		if err != nil {
			return err
		}
		h.hpackBuffer = buffer
		return nil
	})
	if err != nil {
		return nil, err
	}
	return h.hpackBuffer, nil
}

func newHPacker(maxTableSize int) *hpacker {
	return &hpacker{
		encState:    hpack.NewTable(uint64(maxTableSize)),
		hpackBuffer: nil,
	}
}

// Abort transitions the connection to GoneAway and force-closes every stream,
// waking any goroutine blocked in Encode, TakePeerData, Write, or AcceptStream.
// Use it when the underlying transport dies (socket read/write error) so that
// in-flight callers fail with io.EOF / state errors instead of hanging on
// internal channels that would never be signalled again.
func (c *HTTP2Conn) Abort() {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	c.state = ParseState_GoneAway
	for _, stream := range c.streams {
		stream.setState(H2StreamState_Close)
	}
	c.notifyWritable()
	select {
	case c.acceptCh <- struct{}{}:
	default:
	}
}

func (c *HTTP2Conn) CreateStream() (*HTTP2Stream, bool) {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	if c.state == ParseState_GoneAway {
		return nil, false
	}
	if c.peerSettings.MaxConcurrentStreams != 0 && c.activeStreams >= int(c.peerSettings.MaxConcurrentStreams) {
		return nil, false
	}
	c.activeStreams++
	id := c.nextStreamID
	c.nextStreamID += 2
	return c.newHTTP2Stream(id, H2StreamState_Idle), true
}

func (c *HTTP2Conn) AcceptStream(blocking bool) (*HTTP2Stream, error) {
	c.globalLock.Lock()
	for {
		if c.state == ParseState_GoneAway {
			c.globalLock.Unlock()
			return nil, errors.New("connection is gone away")
		}
		if len(c.acceptQueue) > 0 {
			stream := c.acceptQueue[0]
			c.acceptQueue = c.acceptQueue[1:]
			c.globalLock.Unlock()
			return stream, nil
		}
		c.globalLock.Unlock()
		if !blocking {
			return nil, nil
		}
		<-c.acceptCh
	}
}

func (c *HTTP2Conn) FindStream(id uint32) (*HTTP2Stream, bool) {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	stream, ok := c.streams[id]
	return stream, ok
}

var ErrFrameTooLarge = errors.New("frame size exceeds maximum allowed")

func (c *HTTP2Conn) maxFrameSize() int {
	return int(c.peerSettings.MaxFrameSize)
}

func (c *HTTP2Conn) prepaereHeader(streamID uint32, frameType H2Type, payloadLen int, flags Flags) (*FrameHeaderState, []byte, error) {
	if payloadLen > c.maxFrameSize() {
		return nil, nil, ErrFrameTooLarge
	}
	state := &FrameHeaderState{}
	state.Header.SetStreamId(streamID)
	state.Header.SetType(frameType)
	state.Header.SetLen(uint32(payloadLen))
	state.Header.Flags = flags
	headerBuf := make([]byte, 0, 9)
	data, err := state.Header.Append(headerBuf)
	return state, data, err
}

type blockingReason int

const (
	blockingReasonMaxFrameSize blockingReason = iota
	blockingReasonConnectionWindow
	blockingReasonStreamWindow
)

func (c *HTTP2Conn) writeData(id uint32, streamPeerWindow *window, p []byte, endStream bool) (int, blockingReason, error) {
	minimum := min(len(p), c.maxFrameSize(), int(streamPeerWindow.Available()), int(c.peerWindow.Available()))
	if minimum <= 0 && !endStream {
		reason := blockingReasonMaxFrameSize
		if minimum == int(streamPeerWindow.Available()) {
			reason = blockingReasonStreamWindow
		}
		if minimum == int(c.peerWindow.Available()) {
			reason = blockingReasonConnectionWindow
		}
		return 0, reason, nil
	}
	if minimum < 0 {
		minimum = 0 // endStream with an over-consumed window: emit the bare END_STREAM frame
	}
	flags := Flags{}
	flags.SetEndStreamOrAck(endStream)
	_, headerBuf, err := c.prepaereHeader(id, H2Type_DATA, minimum, flags)
	if err != nil {
		return 0, blockingReasonMaxFrameSize, err
	}
	c.writeBuffer = append(c.writeBuffer, headerBuf...)
	// Only the window/frame-size-limited prefix — the frame header above
	// declares exactly `minimum` bytes; appending more would desynchronize
	// the peer's frame parser.
	c.writeBuffer = append(c.writeBuffer, p[:minimum]...)
	err = streamPeerWindow.Consume(int64(minimum))
	if err != nil {
		return 0, blockingReasonStreamWindow, err
	}
	err = c.peerWindow.Consume(int64(minimum))
	if err != nil {
		return 0, blockingReasonConnectionWindow, err
	}
	c.notifyWritable()
	return minimum, blockingReasonMaxFrameSize, nil
}

func (c *HTTP2Conn) appendContinuation(id uint32, p []byte) error {
	for {
		flags := Flags{}
		frameLen := min(len(p), c.maxFrameSize())
		if frameLen == len(p) {
			flags.SetEndHeaders(true)
		}
		_, headerBuf, err := c.prepaereHeader(id, H2Type_CONTINUATION, frameLen, flags)
		if err != nil {
			return err
		}
		c.writeBuffer = append(c.writeBuffer, headerBuf...)
		p = p[frameLen:]
		if flags.EndHeaders() {
			break
		}
	}
	return nil
}

func (c *HTTP2Conn) notifyWritable() {
	select {
	case c.writeCh <- struct{}{}:
	default:
	}
}

func (c *HTTP2Conn) writeHeader(id uint32, hdr headerEmitter) (err error) {
	defer func() {
		if err == nil {
			c.notifyWritable()
		}
	}()
	packed, err := c.encHpack.PackHeader(hdr)
	if err != nil {
		return err
	}
	flags := Flags{}
	bufSize := min(len(packed), c.maxFrameSize())
	if bufSize == len(packed) {
		flags.SetEndHeaders(true)
	}
	_, headerBuf, err := c.prepaereHeader(id, H2Type_HEADERS, bufSize, flags)
	if err != nil {
		return err
	}
	c.writeBuffer = append(c.writeBuffer, headerBuf...)
	c.writeBuffer = append(c.writeBuffer, packed[:bufSize]...)

	if !flags.EndHeaders() {
		return c.appendContinuation(id, packed[bufSize:])
	}

	return nil
}

func (c *HTTP2Conn) writeRstStream(id uint32, code uint32) error {
	state, headerBuf, err := c.prepaereHeader(id, H2Type_RST_STREAM, 4, Flags{})
	if err != nil {
		return err
	}
	c.writeBuffer = append(c.writeBuffer, headerBuf...)
	c.writeBuffer = (&RstStreamFrame{ErrorCode: code}).MustAppend_impl(c.writeBuffer, state)
	c.notifyWritable()
	return nil
}

func (c *HTTP2Conn) updateSettings(setting []Setting) error {
	if c.waitingSettingsAck {
		return errors.New("cannot update settings while waiting for ACK")
	}
	c.updatingSettings = c.localSettings
	for _, s := range setting {
		switch s.Id {
		case SettingsID_HEADER_TABLE_SIZE:
			c.updatingSettings.TableSize = s.Value
		case SettingsID_ENABLE_PUSH:
			c.updatingSettings.EnablePush = s.Value != 0
		case SettingsID_MAX_CONCURRENT_STREAMS:
			c.updatingSettings.MaxConcurrentStreams = s.Value
		case SettingsID_INITIAL_WINDOW_SIZE:
			c.updatingSettings.InitialWindowSize = s.Value
		case SettingsID_MAX_FRAME_SIZE:
			if !AllowedMaxFrameSize(s.Value) {
				return errors.New("invalid max frame size")
			}
			c.updatingSettings.MaxFrameSize = s.Value
		case SettingsID_MAX_HEADER_LIST_SIZE:
			c.updatingSettings.MaxHeaderListSize = s.Value
		}
	}
	size := 6 * len(setting)
	hdr := &FrameHeaderState{
		Header: FrameHeader{},
	}
	hdr.Header.SetType(H2Type_SETTINGS)
	hdr.Header.SetLen(uint32(size))
	hdr.Header.Flags.SetEndStreamOrAck(false)
	buf := make([]byte, 0, 9+size)
	var err error
	buf, err = hdr.Header.Append(buf)
	if err != nil {
		return err
	}
	buf, err = (&SettingsFrame{Settings: setting}).Append_impl(buf, hdr)
	if err != nil {
		return err
	}
	c.writeBuffer = append(c.writeBuffer, buf...)
	c.waitingSettingsAck = true
	c.notifyWritable()
	return nil
}

func (c *HTTP2Conn) writeGoAway(lastStreamID uint32, code uint32, debugData []byte) error {
	state, headerBuf, err := c.prepaereHeader(CONNECTION_LEVEL_ID, H2Type_GOAWAY, 8+len(debugData), Flags{})
	if err != nil {
		return err
	}
	c.writeBuffer = append(c.writeBuffer, headerBuf...)
	frame := &GoAwayFrame{ErrorCode: code, DebugData: debugData}
	frame.SetLastStreamId(lastStreamID)
	c.writeBuffer = frame.MustAppend_impl(c.writeBuffer, state)
	c.notifyWritable()
	return nil
}

func (c *HTTP2Conn) handleConnectionLevelFrame(body *FrameBody) error {
	if ping := body.Ping(&c.header); ping != nil {
		if c.header.Header.Flags.EndStreamOrAck() { // ACK for ping, ignore
			return nil
		}
		c.header.Header.Flags.SetEndStreamOrAck(true)
		c.writeBuffer = c.header.Header.MustAppend(c.writeBuffer)
		c.writeBuffer = ping.MustAppend_impl(c.writeBuffer, &c.header)
		return nil
	}
	if settings := body.Settings(&c.header); settings != nil {
		if c.header.Header.Flags.EndStreamOrAck() {
			if !c.waitingSettingsAck {
				return errors.New("unexpected settings ACK")
			}
			if c.localSettings.InitialWindowSize != c.updatingSettings.InitialWindowSize {
				diff := int64(c.updatingSettings.InitialWindowSize) - int64(c.localSettings.InitialWindowSize)
				c.localWindow.Update(diff)
				// for each stream, update the local window as well
				for _, stream := range c.streams {
					stream.localWindow.Update(diff)
				}
			}
			c.localSettings = c.updatingSettings
			c.waitingSettingsAck = false
			return nil
		}
		for _, setting := range settings.Settings {
			switch setting.Id {
			case SettingsID_HEADER_TABLE_SIZE:
				c.peerSettings.TableSize = setting.Value
			case SettingsID_ENABLE_PUSH:
				c.peerSettings.EnablePush = setting.Value != 0
			case SettingsID_MAX_CONCURRENT_STREAMS:
				c.peerSettings.MaxConcurrentStreams = setting.Value
			case SettingsID_INITIAL_WINDOW_SIZE:
				c.peerSettings.InitialWindowSize = setting.Value
			case SettingsID_MAX_FRAME_SIZE:
				if !AllowedMaxFrameSize(setting.Value) {
					return errors.New("invalid max frame size")
				}
				c.peerSettings.MaxFrameSize = setting.Value
			case SettingsID_MAX_HEADER_LIST_SIZE:
				c.peerSettings.MaxHeaderListSize = setting.Value
			}
		}
		c.header.Header.Flags.SetEndStreamOrAck(true)
		c.header.Header.SetLen(0)
		c.writeBuffer = c.header.Header.MustAppend(c.writeBuffer)
		return nil
	}
	if goaway := body.Goaway(&c.header); goaway != nil {
		c.state = ParseState_GoneAway
		for _, stream := range c.streams {
			stream.setState(H2StreamState_Close)
		}
		return nil
	}
	if windowUpdate := body.WindowUpdate(&c.header); windowUpdate != nil {
		c.peerWindow.Update(int64(windowUpdate.WindowSizeIncrement()))
		for _, stream := range c.streams {
			stream.notifyWindowUpdate()
		}
		return nil
	}
	// check if stream-level frame
	if c.header.Header.Type() == H2Type_HEADERS ||
		c.header.Header.Type() == H2Type_CONTINUATION ||
		c.header.Header.Type() == H2Type_PRIORITY ||
		c.header.Header.Type() == H2Type_RST_STREAM {
		return errors.New("unexpected stream-level frame on connection")
	}
	return nil // skip unknown frame types
}

func (c *HTTP2Conn) handleHpackDecode(stream *HTTP2Stream, hdrBlock []byte) error {
	if stream.receivedHeader == nil {
		stream.receivedHeader = make(http.Header)
	}
	return hpack.DecodeFields(c.decHpack, hdrBlock, func(fieldType hpack.FieldType, key, value string) {
		stream.receivedHeader.Add(key, value)
	})
}

func (c *HTTP2Conn) writeWindowUpdate(id uint32, increment uint32) error {
	state, headerBuf, err := c.prepaereHeader(id, H2Type_WINDOW_UPDATE, 4, Flags{})
	if err != nil {
		return err
	}
	windowUpdate := &WindowUpdateFrame{}
	windowUpdate.SetWindowSizeIncrement(increment)
	c.writeBuffer = append(c.writeBuffer, headerBuf...)
	c.writeBuffer = windowUpdate.MustAppend_impl(c.writeBuffer, state)
	return nil
}

func (c *HTTP2Conn) maySendWindowUpdate(stream *HTTP2Stream) {
	threshold := c.localSettings.InitialWindowSize / 2
	if stream.localWindow.Available() < int64(threshold) {
		increment := int64(c.localSettings.InitialWindowSize) - int64(stream.localWindow.Available())
		if increment <= 0 {
			return
		}
		c.writeWindowUpdate(stream.streamID, uint32(increment))
		stream.localWindow.Update(increment)
	}
	if c.localWindow.Available() < int64(threshold) {
		increment := int64(c.localSettings.InitialWindowSize) - int64(c.localWindow.Available())
		if increment <= 0 {
			return
		}
		c.writeWindowUpdate(CONNECTION_LEVEL_ID, uint32(increment))
		c.localWindow.Update(increment)
	}
}

func (c *HTTP2Conn) newHTTP2Stream(id uint32, state H2StreamState) *HTTP2Stream {
	stream := &HTTP2Stream{
		conn:     c,
		streamID: id,
		state:    state,
		localWindow: window{
			size: int64(c.localSettings.InitialWindowSize),
		},
		peerWindow: window{
			size: int64(c.peerSettings.InitialWindowSize),
		},
		windowUpdateCh: make(chan struct{}, 1),
		readCh:         make(chan struct{}, 1),
	}
	c.streams[stream.streamID] = stream
	if !c.isThisSideInitiated(id) {
		c.acceptQueue = append(c.acceptQueue, stream)
		select {
		case c.acceptCh <- struct{}{}:
		default:
		}
	}
	return stream
}

func (c *HTTP2Conn) isThisSideInitiated(id uint32) bool {
	return IsClientInitiated(id) == c.isClient
}

func (c *HTTP2Conn) handleStreamLevelFrame(body *FrameBody) error {
	stream, ok := c.streams[c.header.Header.StreamId()]
	if !ok {
		if c.isThisSideInitiated(c.header.Header.StreamId()) {
			if c.header.Header.StreamId() < c.nextStreamID {
				// The stream existed and has been fully closed (pruned from
				// the map). RFC 9113 §5.1 permits late WINDOW_UPDATE /
				// RST_STREAM / PRIORITY on closed streams — ignore whatever
				// arrives rather than killing the connection.
				return nil
			}
			return errors.New("invalid stream ID")
		}
		stream = c.newHTTP2Stream(c.header.Header.StreamId(), H2StreamState_Idle)
		c.maxReceivedStreamID = max(c.maxReceivedStreamID, stream.streamID)
		/*
			https://www.rfc-editor.org/rfc/rfc9113.html#section-5.1.1-3 says
			A HEADERS frame will transition the client-initiated stream identified by the stream identifier
			 in the frame header from "idle" to "open".
			 A PUSH_PROMISE frame will transition the server-initiated stream identified by
			 the Promised Stream ID field in the frame payload from "idle" to "reserved (local)" or
			 "reserved (remote)". When a stream transitions out of the "idle" state,
			 all streams in the "idle" state that might have been opened by the peer
			 with a lower-valued stream identifier immediately transition to "closed".
			 That is, an endpoint may skip a stream identifier, with the effect being that
			 the skipped stream is immediately closed.
		*/
		if stream.streamID < c.maxReceivedStreamID {
			stream.setState(H2StreamState_Close)
		}
	}
	if c.continuationStreamID != 0 {
		if c.header.Header.StreamId() != c.continuationStreamID {
			return errors.New("continuation frame received for different stream")
		}
		if c.header.Header.Type() != H2Type_CONTINUATION {
			return errors.New("expected continuation frame")
		}
	}
	if cont := body.Continuation(&c.header); cont != nil {
		if c.continuationStreamID == 0 {
			return errors.New("continuation received without active continuation stream")
		}
		c.headerBuffer = append(c.headerBuffer, cont.HeaderBlock...)
		if c.header.Header.Flags.EndHeaders() {
			c.continuationStreamID = 0
			err := c.handleHpackDecode(stream, c.headerBuffer)
			if stream.state == H2StreamState_Close {
				// setState deferred the prune while this CONTINUATION
				// sequence was in flight; finish it now.
				delete(c.streams, stream.streamID)
			}
			return err
		}
		return nil
	}
	if priority := body.Priority(&c.header); priority != nil {
		return nil // this implementation does not handle priority frames, so we just ignore them
	}
	if windowUpdate := body.WindowUpdate(&c.header); windowUpdate != nil {
		stream.peerWindow.Update(int64(windowUpdate.WindowSizeIncrement()))
		stream.notifyWindowUpdate()
		return nil
	}
	if rstStream := body.RstStream(&c.header); rstStream != nil {
		if stream.state != H2StreamState_Close {
			stream.setState(H2StreamState_Close)
			stream.code = rstStream.ErrorCode
			if c.isThisSideInitiated(stream.streamID) {
				c.activeStreams--
			}
		}
		return nil
	}
	if stream.state == H2StreamState_Close {
		// https://www.rfc-editor.org/rfc/rfc9113.html#section-5.1-7.14.3 says
		// An endpoint MUST NOT send frames other than PRIORITY on a closed stream.
		// An endpoint MAY treat receipt of any other type of frame on a closed stream as a
		// connection error (Section 5.4.1) of type STREAM_CLOSED, except as noted below.
		// An endpoint that sends a frame with the END_STREAM flag set or
		// a RST_STREAM frame might receive a WINDOW_UPDATE or RST_STREAM frame from
		// its peer in the time before the peer receives and processes the frame that
		// closes the stream.
		return errors.New("frame received on closed stream")
	}
	if stream.state == H2StreamState_Idle {
		if c.header.Header.Type() != H2Type_HEADERS {
			return errors.New("expected headers frame to open idle stream")
		}
	}
	mayHandleClose := func() {
		if c.header.Header.Flags.EndStreamOrAck() {
			switch stream.state {
			case H2StreamState_Open:
				stream.setState(H2StreamState_HalfClosedRemote)
			case H2StreamState_HalfClosedLocal:
				stream.setState(H2StreamState_Close)
				if c.isThisSideInitiated(stream.streamID) {
					c.activeStreams--
				}
			}
		}
	}
	if hdr := body.Headers(&c.header); hdr != nil {
		if stream.state == H2StreamState_Idle {
			stream.setState(H2StreamState_Open)
		}
		mayHandleClose()
		if c.header.Header.Flags.EndHeaders() {
			return c.handleHpackDecode(stream, hdr.HeaderBlock)
		}
		c.continuationStreamID = stream.streamID
		c.headerBuffer = append(c.headerBuffer[:0], hdr.HeaderBlock...)
		return nil
	}
	if data := body.Data(&c.header); data != nil {
		mayHandleClose()
		if err := c.localWindow.Consume(int64(c.header.Header.Len())); err != nil {
			return err
		}
		if err := stream.localWindow.Consume(int64(c.header.Header.Len())); err != nil {
			return err
		}
		stream.streamData = append(stream.streamData, data.Data...)
		stream.notifyRead()
		c.maySendWindowUpdate(stream)
		return nil
	}
	if pp := body.PushPromise(&c.header); pp != nil {
		if !c.localSettings.EnablePush {
			return errors.New("push promise received but push is disabled")
		}
		if !c.isClient {
			return errors.New("push promise received on server connection")
		}
		if pp.PromisedStreamId() <= c.maxReceivedStreamID {
			return errors.New("invalid promised stream ID")
		}
		c.maxReceivedStreamID = pp.PromisedStreamId()
		promisedStream := c.newHTTP2Stream(pp.PromisedStreamId(), H2StreamState_ReservedRemote)
		c.streams[promisedStream.streamID] = promisedStream
		if c.header.Header.Flags.EndHeaders() {
			return c.handleHpackDecode(promisedStream, pp.HeaderBlock)
		}
		c.continuationStreamID = promisedStream.streamID
		c.headerBuffer = append(c.headerBuffer[:0], pp.HeaderBlock...)
		return nil
	}
	// check if connection-level
	if c.header.Header.Type() == H2Type_PING ||
		c.header.Header.Type() == H2Type_SETTINGS ||
		c.header.Header.Type() == H2Type_GOAWAY {
		return errors.New("unexpected connection-level frame on stream")
	}
	return nil // skip unknown frame types
}

func (c *HTTP2Conn) handleBody(body *FrameBody) error {
	if c.inspectBody != nil {
		c.inspectBody(&c.header.Header, body)
	}
	switch c.header.Header.StreamId() {
	case CONNECTION_LEVEL_ID: // connection-level frame
		return c.handleConnectionLevelFrame(body)
	default:
		return c.handleStreamLevelFrame(body)
	}
}

func (c *HTTP2Conn) Start() error {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	// initialize connection-level window
	c.localWindow.Update(int64(c.localSettings.InitialWindowSize))
	c.peerWindow.Update(int64(c.peerSettings.InitialWindowSize))
	if c.isClient {
		c.writeBuffer = append(c.writeBuffer, CONNECTION_PREFACE...)
		c.nextStreamID = CLIENT_INITIAL_ID
	} else {
		c.nextStreamID = SERVER_INITIAL_ID
		c.state = ParseState_WaitPreface
	}
	enablePush := 0
	if c.localSettings.EnablePush {
		enablePush = 1
	}
	// send settings frame
	err := c.updateSettings([]Setting{
		{Id: SettingsID_HEADER_TABLE_SIZE, Value: c.localSettings.TableSize},
		{Id: SettingsID_ENABLE_PUSH, Value: uint32(enablePush)},
		{Id: SettingsID_INITIAL_WINDOW_SIZE, Value: c.localSettings.InitialWindowSize},
		{Id: SettingsID_MAX_FRAME_SIZE, Value: c.localSettings.MaxFrameSize},
	})
	if err == nil {
		c.notifyWritable()
	}
	return err
}

func (c *HTTP2Conn) EncodeNonBlock() ([]byte, error) {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	if c.state == ParseState_GoneAway {
		return nil, errors.New("connection is gone away")
	}
	data := c.writeBuffer
	c.writeBuffer = nil
	return data, nil
}

func (c *HTTP2Conn) Encode() ([]byte, error) {
	c.globalLock.Lock()
	for {
		if c.state == ParseState_GoneAway {
			c.globalLock.Unlock()
			return nil, errors.New("connection is gone away")
		}
		if len(c.writeBuffer) > 0 {
			data := c.writeBuffer
			c.writeBuffer = nil
			c.globalLock.Unlock()
			return data, nil
		}
		c.globalLock.Unlock()
		<-c.writeCh
		c.globalLock.Lock()
	}
}

func (c *HTTP2Conn) Decode(p []byte) (err error) {
	c.globalLock.Lock()
	defer c.globalLock.Unlock()
	defer func() {
		if err != nil {
			c.state = ParseState_GoneAway
		} else if len(c.writeBuffer) > 0 {
			c.notifyWritable()
		}
	}()
	for {
		switch c.state {
		case ParseState_WaitPreface:
			buffer := c.readBuffer.TryGetRequestedSizeBuffer(&p, len(CONNECTION_PREFACE))
			if buffer == nil {
				return nil
			}
			if !bytes.Equal(buffer, []byte(CONNECTION_PREFACE)) {
				return errors.New("invalid connection preface")
			}
			c.state = ParseState_WaitHeader
			fallthrough
		case ParseState_WaitHeader:
			buffer := c.readBuffer.TryGetRequestedSizeBuffer(&p, 9)
			if buffer == nil {
				return nil
			}
			err := c.header.Header.DecodeExact(buffer)
			if err != nil {
				return err
			}
			c.state = ParseState_HeaderRead
			fallthrough
		case ParseState_HeaderRead:
			buffer := c.readBuffer.TryGetRequestedSizeBuffer(&p, int(c.header.Header.Len()))
			if buffer == nil {
				return nil
			}
			body := &FrameBody{}
			err := body.DecodeExact_impl(buffer, &c.header)
			if err != nil {
				return err
			}
			err = c.handleBody(body)
			if err != nil {
				return err
			}
			if c.state == ParseState_HeaderRead {
				c.state = ParseState_WaitHeader
			}
		case ParseState_GoneAway:
			if len(c.readBuffer.Buffer) != 0 || len(p) != 0 {
				return errors.New("data received after GOAWAY")
			}
			return nil
		}
	}
}

func NewHTTP2Conn(isClient bool) *HTTP2Conn {
	return &HTTP2Conn{
		streams:       make(map[uint32]*HTTP2Stream),
		state:         ParseState_WaitHeader,
		encHpack:      newHPacker(int(defaultSettings.TableSize)),
		decHpack:      hpack.NewTable(uint64(defaultSettings.TableSize)),
		isClient:      isClient,
		localSettings: defaultSettings,
		peerSettings:  defaultSettings,
		acceptCh:      make(chan struct{}, 1),
		writeCh:       make(chan struct{}, 1),
	}
}
