package wire

import (
	"context"

	"github.com/on-keyday/kscale/protobuf/wire/zerocopy"
)

func GrpcFrame(dataLen uint32) [5]byte {
	hdr := &GRPCHeader{
		Compress: 0,
		Len:      dataLen,
	}
	var buf [5]byte
	hdr.MustEncode(buf[:])
	return buf
}

type GRPCFramer struct {
	Send          func([]byte) error
	Receive       func() ([]byte, error)
	DoClose       func() error
	header        GRPCHeader
	headerRead    bool
	receiveBuffer zerocopy.ZeroCopyBuffer
}

var _ RawStream = (*GRPCFramer)(nil)

func (f *GRPCFramer) Close() error {
	if f.DoClose != nil {
		return f.DoClose()
	}
	return nil
}

func (f *GRPCFramer) SendMessage(ctx context.Context, msg []byte) error {
	header := GrpcFrame(uint32(len(msg)))
	return f.Send(append(header[:], msg...))
}

func (f *GRPCFramer) ReadMessage(ctx context.Context) ([]byte, error) {
	var buffer []byte
	var err error
	for {
		if !f.headerRead {
			if len(buffer) == 0 && len(f.receiveBuffer.Buffer) < 5 {
				buffer, err = f.Receive()
				if err != nil {
					return nil, err
				}
			}
			buffer := f.receiveBuffer.TryGetRequestedSizeBuffer(&buffer, 5)
			if buffer != nil {
				if err := f.header.DecodeExact(buffer); err != nil {
					return nil, err
				}
				f.headerRead = true
			}
		} else {
			if len(buffer) == 0 && len(f.receiveBuffer.Buffer) < int(f.header.Len) {
				buffer, err = f.Receive()
				if err != nil {
					return nil, err
				}
			}
			buffer := f.receiveBuffer.TryGetRequestedSizeBuffer(&buffer, int(f.header.Len))
			if buffer != nil {
				f.headerRead = false
				return buffer, nil
			}
		}
	}
}
