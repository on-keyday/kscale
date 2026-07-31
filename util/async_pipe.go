package util

import (
	"bytes"
	"io"
	"sync"
)

type AsyncPipe struct {
	buf        bytes.Buffer
	mu         sync.Mutex
	closed     bool
	readSignal chan struct{}
}

func NewAsyncPipe() *AsyncPipe {
	return &AsyncPipe{
		readSignal: make(chan struct{}, 1),
	}
}

func (p *AsyncPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.signalRead()
	return nil
}

func (p *AsyncPipe) signalRead() {
	select {
	case p.readSignal <- struct{}{}:
	default:
	}
}

func (p *AsyncPipe) Write(data []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.EOF
	}
	x, err := p.buf.Write(data)
	p.signalRead()
	return x, err
}

func (p *AsyncPipe) ReadNonblocking(b []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, err = p.buf.Read(b)
	if n > 0 || err != nil {
		if err != io.EOF {
			return n, err
		}
	}
	if p.closed {
		return 0, io.EOF
	}
	return 0, nil
}

func (p *AsyncPipe) WaitReadable() <-chan struct{} {
	return p.readSignal
}

func (p *AsyncPipe) Read(b []byte) (n int, err error) {
	for {
		n, err = p.ReadNonblocking(b)
		if n > 0 || err != nil {
			return n, err
		}
		<-p.WaitReadable()
	}
}
