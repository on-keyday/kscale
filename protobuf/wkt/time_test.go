package wkt

import (
	"bytes"
	"io"
	"testing"
)

// ベンチマーク用のダミーio.Writer
// Writeメソッドがインライン化されにくいようにし、インターフェース越しの挙動をシミュレートする
type blackHole struct{}

func (blackHole) Write(p []byte) (int, error) {
	return len(p), nil
}

func (blackHole) WriteByte(p byte) error {
	return nil
}

func BenchmarkTimestamp_Encode_Interface(b *testing.B) {
	p := &Timestamp{Seconds: 123456789, Nanos: 987654321}
	buf := bytes.NewBuffer(make([]byte, 0, 128))
	var w io.Writer = buf

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := p.Encode(w); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTimestamp_Encode_Buffer(b *testing.B) {
	p := &Timestamp{Seconds: 123456789, Nanos: 987654321}
	buf := bytes.NewBuffer(make([]byte, 0, 128))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := p.EncodeBuffer(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTimestamp_Append(b *testing.B) {
	p := &Timestamp{Seconds: 123456789, Nanos: 987654321}
	buf := make([]byte, 0, 128)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		if _, err := p.Append(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTimestamp_Read(b *testing.B) {
	p := &Timestamp{Seconds: 123456789, Nanos: 987654321}
	var buf bytes.Buffer
	p.Encode(&buf)
	r := bytes.NewReader(buf.Bytes())
	var data io.Reader = r
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Reset(buf.Bytes())
		var target Timestamp
		if err := target.Read(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTimestamp_ReadBuffer(b *testing.B) {
	p := &Timestamp{Seconds: 123456789, Nanos: 987654321}
	var buf bytes.Buffer
	p.Encode(&buf)
	data := bytes.NewReader(buf.Bytes())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data.Reset(buf.Bytes())
		var target Timestamp
		if err := target.ReadBuffer(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTimestamp_Decode(b *testing.B) {
	p := &Timestamp{Seconds: 123456789, Nanos: 987654321}
	var buf bytes.Buffer
	p.Encode(&buf)
	data := buf.Bytes()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var target Timestamp
		if err := target.Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}
