package wire

import (
	"bytes"
	"testing"
)

func TestProtobuf(t *testing.T) {
	v := Varint{
		Value: 400,
	}
	var buf [2]byte
	data, err := v.Encode(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	v2 := Varint{}
	err = v2.DecodeExact(data)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Len != 2 || v2.Value != 400 {
		t.Fatalf("unexpected %d %d", v2.Len, v2.Value)
	}
}

func TestSample(t *testing.T) {
	sampleBinary := []byte{
		0x08,       // フィールド番号1, ワイヤータイプ0 (Varint)
		0x96, 0x01, // Value: 150
		0x12,       // フィールド番号2, ワイヤータイプ2 (Length-delimited)
		0x02,       // 長さ: 2
		0x47, 0x6f, // 値: "Go"
		0x19,                   // フィールド番号3, ワイヤータイプ1 (fixed64)
		0x08, 0x07, 0x06, 0x05, // 値 (Little Endian)
		0x04, 0x03, 0x02, 0x01,
		0x25,                   // フィールド番号4, ワイヤータイプ5 (fixed32)
		0x0c, 0x0b, 0x0a, 0x09, // 値 (Little Endian)
	}

	// Field 1: Varint
	field1 := &Field{}
	remain, err := field1.Decode(sampleBinary)
	if err != nil {
		t.Fatal(err)
	}
	if field1.Tag.Type() != WireType_Varint || field1.ValueVi() == nil || field1.ValueVi().Value != 150 {
		t.Fatal("field1: unexpected value")
	}

	// Field 2: Length-delimited
	field2 := &Field{}
	remain, err = field2.Decode(remain)
	if err != nil {
		t.Fatal(err)
	}
	if field2.Tag.Type() != WireType_LengthDelimited || !bytes.Equal(*field2.ValueData(), []byte("Go")) {
		t.Fatal("field2: unexpected value")
	}

	// Field 3: fixed64 (WireType 1)
	field3 := &Field{}
	remain, err = field3.Decode(remain)
	if err != nil {
		t.Fatal(err)
	}
	if field3.Tag.Type() != WireType_Fixed64 {
		t.Fatal("field3: expect fixed64 (WireType 1)")
	}
	if v64 := field3.Value64(); v64 == nil || v64.Value != 0x0102030405060708 {
		t.Fatal("field3: unexpected value")
	}

	// Field 4: fixed32 (WireType 5)
	field4 := &Field{}
	remain, err = field4.Decode(remain)
	if err != nil {
		t.Fatal(err)
	}
	if field4.Tag.Type() != WireType_Fixed32 {
		t.Fatal("field4: expect fixed32 (WireType 5)")
	}
	if v32 := field4.Value32(); v32 == nil || v32.Value != 0x090a0b0c {
		// ※ field3.Value32() は実装に基づいた適切なアクセサを想定しています
		t.Fatal("field4: unexpected value")
	}

	if len(remain) != 0 {
		t.Fatal("unexpected remaining bytes")
	}

	// Encode check
	convData := make([]byte, len(sampleBinary))
	offset := 0
	for _, f := range []*Field{field1, field2, field3, field4} {
		encoded, err := f.Encode(convData[offset:])
		if err != nil {
			t.Fatal(err)
		}
		offset += len(encoded)
	}

	if !bytes.Equal(sampleBinary, convData) {
		t.Errorf("encoded binary mismatch\nexpect: %x\nactual: %x", sampleBinary, convData)
	}

	// Message level decode check
	msg := &Message{}
	remain, err = msg.DecodeCopy(sampleBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(msg.MustEncodeCopy(nil), sampleBinary) {
		t.Fatal("message encode mismatch")
	}

	if !bytes.Equal(msg.MustAppend(nil), sampleBinary) {
		t.Fatal("message encode mismatch")
	}
}

func TestSample_Error(t *testing.T) {
	// ケース1: Varintが途中で切れている (MSBが1なのに次のバイトがない)
	t.Run("truncated_varint", func(t *testing.T) {
		truncatedVarint := []byte{
			0x08, // Field 1, WireType 0 (Varint)
			0x96, // 150の一部だが、次のバイト(0x01)が欠落
		}
		field := &Field{}
		_, err := field.Decode(truncatedVarint)
		if err == nil {
			t.Error("expected error for truncated varint, but got nil")
		}
	})

	// ケース2: Length-delimitedの長さが足りない (2バイトと宣言して1バイトしかない)
	t.Run("short_length_delimited", func(t *testing.T) {
		shortLD := []byte{
			0x12, // Field 2, WireType 2 (Length-delimited)
			0x02, // 長さ: 2バイト
			0x47, // 1バイト(G)しかない
		}
		field := &Field{}
		_, err := field.Decode(shortLD)
		// Decode時、あるいはValueData()取得時にエラーになる設計か確認
		if err == nil {
			t.Error("expected error for short length-delimited data, but got nil")
		}
	})

	// ケース3: 固定バッファEncode時のサイズ不足
	t.Run("encode_buffer_shortage", func(t *testing.T) {
		field := &Field{}
		// 適切な値をセット（例としてVarint 150）
		field.Decode([]byte{0x08, 0x96, 0x01})

		tinyBuffer := make([]byte, 1) // 3バイト必要なところに1バイトだけ
		_, err := field.Encode(tinyBuffer)
		if err == nil {
			t.Error("expected error for short encode buffer, but got nil")
		}
	})

	// ケース4: 未知のワイヤータイプ (例: WireType 7)
	t.Run("unknown_wire_type", func(t *testing.T) {
		unknownType := []byte{
			(1 << 3) | 7, // Field 1, WireType 7 (未定義)
			0x01,
		}
		field := &Field{}
		_, err := field.Decode(unknownType)
		// パーサのポリシーにより、エラーにするか、あるいはスキップ可能にするか
		if err == nil {
			t.Log("Note: parser allowed unknown wire type. verify if this is intended.")
		}
	})
}
func TestSample_StrictError(t *testing.T) {
	// ケース1: Varintのオーバーフロー (最大10バイトを超えて継続ビットが立っている)
	t.Run("varint_overflow", func(t *testing.T) {
		// 11バイト目に到達する不正なVarint (0xFFを10個並べて最後に0x02)
		overflowVarint := []byte{
			0x08,
			0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x02,
		}
		field := &Field{}
		_, err := field.Decode(overflowVarint)
		if err == nil {
			t.Error("expected error for 11-byte varint, but got nil")
		}
	})

	// ケース2: 冗長なVarintエンコード (0x00で済むところを 0x80 0x00 と表現)
	// ※実装の厳密さによりますが、セキュリティ上弾く設計が望ましいです
	t.Run("redundant_varint", func(t *testing.T) {
		redundantZero := []byte{0x08, 0x80, 0x00}
		field := &Field{}
		_, err := field.Decode(redundantZero)
		// 厳密なデコーダなら「正規化されていない」としてエラーにする
		if err != nil {
			t.Logf("Note: parser rejected redundant varint as expected: %v", err)
		}
	})

	// ケース3: 巨大なLength-delimitedによるメモリ負荷攻撃 (長さ2GBと自称)
	t.Run("huge_length_delimited", func(t *testing.T) {
		// 0x12 (Field 2, Type 2), 長さ 2^31 (約2GB) をVarintで表現
		hugeLength := []byte{0x12, 0x80, 0x80, 0x80, 0x80, 0x08}
		field := &Field{}
		_, err := field.Decode(hugeLength)

		// 実際のバイナリが足りないので、Decode時点でEOFエラーになるべき
		// もしValueData()内で make([]byte, length) しているなら、そこでのパニック回避も重要
		if err == nil {
			t.Error("expected error for huge length declaration, but got nil")
		}
	})

	// ケース4: ゼロバイトのLength-delimited (正常系だが境界値として重要)
	t.Run("zero_length_delimited", func(t *testing.T) {
		zeroLD := []byte{0x12, 0x00}
		field := &Field{}
		remain, err := field.Decode(zeroLD)
		if err != nil {
			t.Fatalf("failed to decode zero length field: %v", err)
		}
		ld := field.ValueData()
		if ld == nil || len(*ld) != 0 {
			t.Error("expect empty byte slice for zero length")
		}
		if len(remain) != 0 {
			t.Error("remain should be empty")
		}
	})
}

func BenchmarkMessageDecode(b *testing.B) {
	// 重い初期化処理などはここ（タイマー停止中に行う）
	sampleBinary := []byte{
		0x08,       // フィールド番号1, ワイヤータイプ0 (Varint)
		0x96, 0x01, // Value: 150
		0x12,       // フィールド番号2, ワイヤータイプ2 (Length-delimited)
		0x02,       // 長さ: 2
		0x47, 0x6f, // 値: "Go"
		0x19,                   // フィールド番号3, ワイヤータイプ1 (fixed64)
		0x08, 0x07, 0x06, 0x05, // 値 (Little Endian)
		0x04, 0x03, 0x02, 0x01,
		0x25,                   // フィールド番号4, ワイヤータイプ5 (fixed32)
		0x0c, 0x0b, 0x0a, 0x09, // 値 (Little Endian)
	}
	msg := Message{Fields: make([]Field, 0, 4)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg.Fields = msg.Fields[:0]
		copy := sampleBinary
		offset := 0
		msg.DecodeSlice(copy, &offset)
	}
}

func Benchmark4FieldDecode(b *testing.B) {
	// 重い初期化処理などはここ（タイマー停止中に行う）
	sampleBinary := []byte{
		0x08,       // フィールド番号1, ワイヤータイプ0 (Varint)
		0x96, 0x01, // Value: 150
		0x12,       // フィールド番号2, ワイヤータイプ2 (Length-delimited)
		0x02,       // 長さ: 2
		0x47, 0x6f, // 値: "Go"
		0x19,                   // フィールド番号3, ワイヤータイプ1 (fixed64)
		0x08, 0x07, 0x06, 0x05, // 値 (Little Endian)
		0x04, 0x03, 0x02, 0x01,
		0x25,                   // フィールド番号4, ワイヤータイプ5 (fixed32)
		0x0c, 0x0b, 0x0a, 0x09, // 値 (Little Endian)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy := sampleBinary
		offset := 0
		var field [4]Field
		for i := range field {
			field[i].DecodeSlice(copy, &offset)
		}
	}
}
