package hpack

import (
	"errors"
	"io"
)

type KeyValue struct {
	Key   string
	Value string
}

var predefinedHeaders = [62]KeyValue{
	{Key: "INVALIDINDEX", Value: "INVALIDINDEX"},
	{Key: ":authority", Value: ""},
	{Key: ":method", Value: "GET"},
	{Key: ":method", Value: "POST"},
	{Key: ":path", Value: "/"},
	{Key: ":path", Value: "/index.html"},
	{Key: ":scheme", Value: "http"},
	{Key: ":scheme", Value: "https"},
	{Key: ":status", Value: "200"},
	{Key: ":status", Value: "204"},
	{Key: ":status", Value: "206"},
	{Key: ":status", Value: "304"},
	{Key: ":status", Value: "400"},
	{Key: ":status", Value: "404"},
	{Key: ":status", Value: "500"},
	{Key: "accept-charset", Value: ""},
	{Key: "accept-encoding", Value: "gzip, deflate"},
	{Key: "accept-language", Value: ""},
	{Key: "accept-ranges", Value: ""},
	{Key: "accept", Value: ""},
	{Key: "access-control-allow-origin", Value: ""},
	{Key: "age", Value: ""},
	{Key: "allow", Value: ""},
	{Key: "authorization", Value: ""},
	{Key: "cache-control", Value: ""},
	{Key: "content-disposition", Value: ""},
	{Key: "content-encoding", Value: ""},
	{Key: "content-language", Value: ""},
	{Key: "content-length", Value: ""},
	{Key: "content-location", Value: ""},
	{Key: "content-range", Value: ""},
	{Key: "content-type", Value: ""},
	{Key: "cookie", Value: ""},
	{Key: "date", Value: ""},
	{Key: "etag", Value: ""},
	{Key: "expect", Value: ""},
	{Key: "expires", Value: ""},
	{Key: "from", Value: ""},
	{Key: "host", Value: ""},
	{Key: "if-match", Value: ""},
	{Key: "if-modified-since", Value: ""},
	{Key: "if-none-match", Value: ""},
	{Key: "if-range", Value: ""},
	{Key: "if-unmodified-since", Value: ""},
	{Key: "last-modified", Value: ""},
	{Key: "link", Value: ""},
	{Key: "location", Value: ""},
	{Key: "max-forwards", Value: ""},
	{Key: "proxy-authenticate", Value: ""},
	{Key: "proxy-authorization", Value: ""},
	{Key: "range", Value: ""},
	{Key: "referer", Value: ""},
	{Key: "refresh", Value: ""},
	{Key: "retry-after", Value: ""},
	{Key: "server", Value: ""},
	{Key: "set-cookie", Value: ""},
	{Key: "strict-transport-security", Value: ""},
	{Key: "transfer-encoding", Value: ""},
	{Key: "user-agent", Value: ""},
	{Key: "vary", Value: ""},
	{Key: "via", Value: ""},
	{Key: "www-authenticate", Value: ""},
}

type KeyValEntry struct {
	keyvalue KeyValue
	index    uint64
}

type Table struct {
	entries  []KeyValue
	max_size uint64
	size     uint64
}

func NewTable(max_size uint64) *Table {
	return &Table{
		entries:  make([]KeyValue, 0),
		max_size: max_size,
		size:     0,
	}
}

func (t *Table) removeUntilLimit(additionalCapacity uint64) {
	for t.size+additionalCapacity > t.max_size {
		if len(t.entries) == 0 {
			if t.size != 0 {
				panic("size should be 0 when entries is empty")
			}
			break
		}
		removed := t.entries[0]
		t.entries = t.entries[1:]
		t.size -= SizeOfField(uint64(len(removed.Key)), uint64(len(removed.Value)))
	}
}

func (t *Table) update_max_size(max_size uint64) {
	t.max_size = max_size
	t.removeUntilLimit(0)
}

func (t *Table) insert(key string, value string) error {
	newFieldSize := SizeOfField(uint64(len(key)), uint64(len(value)))
	t.removeUntilLimit(newFieldSize)
	// https://www.rfc-editor.org/rfc/rfc7541.html#section-4.1
	// 4.1.  Calculating Table Size
	// It is not an error to
	// attempt to add an entry that is larger than the maximum size; an
	// attempt to add an entry larger than the maximum size causes the table
	// to be emptied of all existing entries and results in an empty table.
	if newFieldSize > t.max_size {
		return nil
	}
	t.entries = append(t.entries, KeyValue{Key: string(key), Value: string(value)})
	t.size += newFieldSize
	return nil
}

func (table *Table) Lookup(index uint64) (*KeyValEntry, error) {
	if int(index) < len(predefinedHeaders) {
		if index == 0 {
			return nil, errors.New("index 0 is not valid")
		}
		return &KeyValEntry{keyvalue: predefinedHeaders[index], index: index}, nil
	}
	if table != nil {
		i := int(index) - len(predefinedHeaders)
		if i < 0 || i >= len(table.entries) {
			return nil, errors.New("index out of range")
		}
		return &KeyValEntry{keyvalue: table.entries[i], index: index}, nil
	}
	return nil, errors.New("index out of range")
}

func DecodeFields(table *Table, r []byte, emit func(fieldType FieldType, key string, value string)) error {
	remain := r
	for len(remain) > 0 {
		var err error
		remain, err = DecodeField(table, remain, emit)
		if err != nil {
			return err
		}
	}
	return nil
}

func DecodeField(table *Table, r []byte, emit func(fieldType FieldType, key string, value string)) ([]byte, error) {
	if len(r) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	b := r[0]
	remain := r[1:]
	fieldType := GetFieldType(b)
	prefixLen := uint32(FieldPrefixLen(fieldType))
	switch fieldType {
	case FieldType_Index:
		index, remain, _, err := DecodeIntegerWithFirstByte(remain, prefixLen, b)
		if err != nil {
			return nil, err
		}
		entry, err := table.Lookup(index)
		if err != nil {
			return nil, err
		}
		emit(fieldType, entry.keyvalue.Key, entry.keyvalue.Value)
		return remain, nil
	case FieldType_IndexLiteralInsert:
		if table == nil {
			return nil, errors.New("table is nil")
		}
		index, remain, _, err := DecodeIntegerWithFirstByte(remain, prefixLen, b)
		if err != nil {
			return nil, err
		}
		var key []byte
		var value []byte
		if index == 0 {
			key, remain, err = DecodeString(remain)
			if err != nil {
				return nil, err
			}
			value, remain, err = DecodeString(remain)
			if err != nil {
				return nil, err
			}
		} else {
			entry, err := table.Lookup(index)
			if err != nil {
				return nil, err
			}
			key = []byte(entry.keyvalue.Key)
			value, remain, err = DecodeString(remain)
			if err != nil {
				return nil, err
			}
		}
		strKey := string(key)
		strValue := string(value)
		table.insert(strKey, strValue)
		emit(fieldType, strKey, strValue)
		return remain, nil
	case FieldType_IndexLiteralNoInsert, FieldType_IndexLiteralNeverIndexed:
		index, remain, _, err := DecodeIntegerWithFirstByte(remain, prefixLen, b)
		if err != nil {
			return nil, err
		}
		var key []byte = nil
		var value []byte = nil
		if index == 0 {
			key, remain, err = DecodeString(remain)
			if err != nil {
				return nil, err
			}
			value, remain, err = DecodeString(remain)
			if err != nil {
				return nil, err
			}
		} else {
			entry, err := table.Lookup(index)
			if err != nil {
				return nil, err
			}
			key = []byte(entry.keyvalue.Key)
			value, remain, err = DecodeString(remain)
			if err != nil {
				return nil, err
			}
		}
		emit(fieldType, string(key), string(value))
		return remain, nil
	case FieldType_DynTableUpdate:
		// RFC 7541 §6.3: resize the dynamic table. Peers (incl. Go's net/http
		// client) emit this at the head of a header block after the peer's
		// SETTINGS_HEADER_TABLE_SIZE is applied. Not associated with a field,
		// so nothing is emitted.
		newSize, remain, _, err := DecodeIntegerWithFirstByte(remain, prefixLen, b)
		if err != nil {
			return nil, err
		}
		if table != nil {
			table.update_max_size(newSize)
		}
		return remain, nil
	default:
		return nil, errors.New("unknown field type")
	}
}

func (table *Table) ReverseLookup(key string, value string, only_hdr_equal bool) *KeyValEntry {
	for i, entry := range predefinedHeaders[1:] {
		if !only_hdr_equal {
			if key == entry.Key && value == entry.Value {
				return &KeyValEntry{keyvalue: entry, index: uint64(i) + 1}
			}
		} else {
			if key == entry.Key {
				return &KeyValEntry{keyvalue: entry, index: uint64(i) + 1}
			}
		}
	}
	if table != nil {
		for i, x := range table.entries {
			dynTableIndex := uint64(i) + uint64(len(predefinedHeaders))
			if !only_hdr_equal {
				if key == x.Key && value == x.Value {
					return &KeyValEntry{keyvalue: x, index: dynTableIndex}
				}
			} else {
				if key == x.Key {
					return &KeyValEntry{keyvalue: x, index: dynTableIndex}
				}
			}
		}
	}
	return nil
}

func AppendField(buf []byte, entry KeyValue, mode AppendStringMode, table *Table, mayAdd func(_ KeyValue, indexed bool) FieldType) ([]byte, error) {
	// exact match
	matched := table.ReverseLookup(entry.Key, entry.Value, false)
	if matched != nil {
		return AppendInteger(buf, 7, MatchFieldType(FieldType_Index), matched.index)
	}
	// key match
	matched_key := table.ReverseLookup(entry.Key, entry.Value, true)
	index := uint64(0)
	fieldType := FieldType_IndexLiteralNoInsert
	if table != nil && mayAdd != nil {
		typ := mayAdd(entry, matched_key != nil)
		if typ != FieldType_IndexLiteralInsert && typ != FieldType_IndexLiteralNeverIndexed && typ != FieldType_IndexLiteralNoInsert {
			return nil, errors.New("invalid field type returned by mayAdd")
		}
		fieldType = typ
	}
	if matched_key != nil {
		index = matched_key.index
	}
	var err error
	buf, err = AppendInteger(buf, uint32(FieldPrefixLen(fieldType)), MatchFieldType(fieldType), index)
	if err != nil {
		return nil, err
	}
	if matched_key == nil {
		buf, err = AppendString(buf, 0, []byte(entry.Key), mode)
		if err != nil {
			return nil, err
		}
	}
	buf, err = AppendString(buf, 0, []byte(entry.Value), mode)
	if err != nil {
		return nil, err
	}
	if fieldType == FieldType_IndexLiteralInsert {
		return buf, table.insert(entry.Key, entry.Value)
	}
	return buf, nil
}
