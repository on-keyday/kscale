package hpack

import (
	"errors"
	"io"
)

func AppendInteger(buf []byte, prefix_len uint32, prefix byte, value uint64) ([]byte, error) {
	if prefix_len >= 8 {
		return nil, errors.New("prefix_len must be less than 8")
	}
	var mask uint8 = ^(uint8(0)) >> (8 - prefix_len)
	if mask&prefix != 0 {
		return nil, errors.New("prefix must not overlap with mask")
	}
	if value < uint64(mask) {
		var v uint8 = uint8(value) & mask
		buf = append(buf, v|prefix)
	} else {
		buf = append(buf, prefix|mask)
		var val uint64 = value
		val -= uint64(mask)
		for val > 0x7f {
			var t uint8 = uint8(val & 0x7F)
			buf = append(buf, t|0x80)
			val >>= 7
		}
		var t uint8 = uint8(val)
		buf = append(buf, t)
	}
	return buf, nil
}

func DecodeIntegerWithFirstByte(r []byte, prefix_len uint32, first_byte uint8) (_ uint64, remain []byte, prefix uint8, _ error) {
	if prefix_len >= 8 {
		return 0, nil, 0, errors.New("prefix_len must be less than 8")
	}
	var b = first_byte
	mask := ^uint8(0) >> (8 - prefix_len)
	prefix = b & ^mask
	var value uint64 = uint64(b & mask)
	if value < uint64(mask) {
		return value, r, prefix, nil
	}
	var shift uint32 = 0
	for {
		if shift >= 64 {
			return 0, nil, 0, errors.New("integer overflow")
		}
		if len(r) == 0 {
			return 0, nil, 0, io.ErrUnexpectedEOF
		}
		b, r = r[0], r[1:]
		value += uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return value, r, prefix, nil
}

func DecodeInteger(r []byte, prefix_len uint32) (uint64, []byte, uint8, error) {
	if len(r) == 0 {
		return 0, nil, 0, errors.New("input is empty")
	}
	return DecodeIntegerWithFirstByte(r[1:], prefix_len, r[0])
}

type AppendStringMode uint8

const (
	BestEffort AppendStringMode = iota
	ForceHuffman
	ForceLiteral
)

func AppendString(buf []byte, prefix uint8, str []uint8, mode AppendStringMode) ([]byte, error) {
	var err error
	huffLen := HuffmanLength(str)
	literalIsBetter := huffLen > len(str)
	switch mode {
	case ForceHuffman:
		literalIsBetter = false
	case ForceLiteral:
		literalIsBetter = true
	}
	if literalIsBetter {
		buf, err = AppendInteger(buf, 7, 0x7f&prefix, uint64(len(str)))
		if err != nil {
			return nil, err
		}
		buf = append(buf, str...)
	} else {
		buf, err = AppendInteger(buf, 7, 0x80|prefix, uint64(huffLen))
		if err != nil {
			return nil, err
		}
		var bitWriter = &BitWriter{}
		for _, c := range str {
			Codes[c].Write(bitWriter)
		}
		bitWriter.Fill()
		buf = append(buf, bitWriter.buf...)
	}
	return buf, nil
}

var ErrDone = errors.New("done")

func decodeSingleChar(r *BitReader) (_ HuffmanTree, _ error) {
	allone := 1
	var node = GetRoot()
	for {
		if node.HasValue() {
			return node, nil
		}
		bit, err := r.ReadBit()
		if err != nil {
			if err == io.EOF && (node == GetRoot() || allone != 0 && allone-1 <= 7) {
				return 0, ErrDone
			}
			if err == io.EOF {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if allone != 0 && bit {
			allone++
		} else {
			allone = 0
		}
		next, ok := node.Next(bit)
		if !ok {
			return 0, errors.New("invalid huffman code")
		}
		node = next
	}
}

func DecodeHuffmanString(r []byte) ([]byte, error) {
	bitReader := &BitReader{buf: r}
	result := make([]byte, 0)
	for {
		node, err := decodeSingleChar(bitReader)
		if err != nil {
			if err == ErrDone {
				break
			}
			return nil, err
		}
		if node.GetValue() == 256 {
			return nil, errors.New("Out of range value in huffman code")
		}
		result = append(result, byte(node.GetValue()))
	}
	return result, nil
}

func DecodeStringWithLenPrefix(r []byte, expectLen uint64, prefix uint8) (str []byte, remain []byte, err error) {
	if len(r) < int(expectLen) {
		return nil, nil, errors.New("string length exceeds input length")
	}
	if prefix&0x80 != 0 {
		decoded, err := DecodeHuffmanString(r[:expectLen])
		if err != nil {
			return nil, nil, err
		}
		return decoded, r[expectLen:], nil
	} else {
		return r[:expectLen], r[expectLen:], nil
	}
}

func DecodeString(r []byte) ([]byte, []byte, error) {
	len, r, prefix, err := DecodeInteger(r, 7)
	if err != nil {
		return nil, nil, err
	}
	str, remain, err := DecodeStringWithLenPrefix(r, len, prefix)
	if err != nil {
		return nil, nil, err
	}
	return str, remain, nil
}

func DecodeStringWithFirstByte(r []byte, first_byte byte) ([]byte, []byte, error) {
	var prefix byte = 0
	len, r, prefix, err := DecodeIntegerWithFirstByte(r, 7, first_byte)
	if err != nil {
		return nil, nil, err
	}
	return DecodeStringWithLenPrefix(r, len, prefix)
}
