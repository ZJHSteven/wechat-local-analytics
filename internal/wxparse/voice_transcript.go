package wxparse

import "strings"

// NativeVoiceTranscript 从 WeChat 4.x 消息行的 packed_info_data 中读取
// 微信客户端自己已经完成并缓存的“语音转文字”结果。
//
// 这里故意只接受已经确认过的精确 protobuf-like 路径：
//
//	top-level field 5 (length-delimited)
//	    -> nested field 1 = 2   // status=completed
//	    -> nested field 2       // UTF-8 transcript
//
// 不做“在 blob 里搜可打印字符串”之类的猜测，否则很容易把其它元数据、
// wxid 或 URL 误识别成语音文字。解析失败、状态未完成、UTF-8 非法时都返回
// 空字符串，由上层决定是否显示 unavailable 或启用显式 ASR fallback。
func NativeVoiceTranscript(packed []byte) string {
	if len(packed) == 0 || len(packed) > 1024*1024 {
		return ""
	}
	top, ok := parseProtoLikeFields(packed)
	if !ok {
		return ""
	}
	for _, field := range top {
		if field.number != 5 || field.wire != 2 || len(field.bytes) == 0 {
			continue
		}
		nested, ok := parseProtoLikeFields(field.bytes)
		if !ok {
			continue
		}
		var status uint64
		var haveStatus bool
		var text []byte
		for _, inner := range nested {
			switch {
			case inner.number == 1 && inner.wire == 0:
				status = inner.varint
				haveStatus = true
			case inner.number == 2 && inner.wire == 2:
				text = inner.bytes
			}
		}
		if !haveStatus || status != 2 || len(text) == 0 || !validUTF8(text) {
			continue
		}
		if transcript := strings.TrimSpace(string(text)); transcript != "" {
			return transcript
		}
	}
	return ""
}

type protoLikeField struct {
	number int
	wire   int
	varint uint64
	bytes  []byte
}

func parseProtoLikeFields(raw []byte) ([]protoLikeField, bool) {
	fields := make([]protoLikeField, 0, 8)
	for pos := 0; pos < len(raw); {
		tag, next, ok := readProtoLikeVarint(raw, pos)
		if !ok || next <= pos {
			return nil, false
		}
		pos = next
		fieldNo := int(tag >> 3)
		wire := int(tag & 0x07)
		if fieldNo <= 0 {
			return nil, false
		}
		field := protoLikeField{number: fieldNo, wire: wire}
		switch wire {
		case 0:
			value, n, ok := readProtoLikeVarint(raw, pos)
			if !ok || n <= pos {
				return nil, false
			}
			field.varint = value
			pos = n
		case 1:
			if pos+8 > len(raw) {
				return nil, false
			}
			field.bytes = raw[pos : pos+8]
			pos += 8
		case 2:
			size, n, ok := readProtoLikeVarint(raw, pos)
			if !ok || n <= pos || size > uint64(len(raw)) {
				return nil, false
			}
			pos = n
			end := pos + int(size)
			if end < pos || end > len(raw) {
				return nil, false
			}
			field.bytes = raw[pos:end]
			pos = end
		case 5:
			if pos+4 > len(raw) {
				return nil, false
			}
			field.bytes = raw[pos : pos+4]
			pos += 4
		default:
			return nil, false
		}
		fields = append(fields, field)
	}
	return fields, true
}

func readProtoLikeVarint(raw []byte, offset int) (uint64, int, bool) {
	var value uint64
	var shift uint
	pos := offset
	for i := 0; i < 10; i++ {
		if pos >= len(raw) {
			return 0, offset, false
		}
		b := raw[pos]
		pos++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, pos, true
		}
		shift += 7
	}
	return 0, offset, false
}

func validUTF8(raw []byte) bool {
	for len(raw) > 0 {
		r, size := decodeRune(raw)
		if r == runeError && size == 1 {
			return false
		}
		raw = raw[size:]
	}
	return true
}

// 这两个小包装避免在上面的业务逻辑里反复写 utf8.DecodeRune，保持解析路径
// 连续易读；具体 UTF-8 判定仍然完全使用 Go 标准库。
const runeError = '\uFFFD'

func decodeRune(raw []byte) (rune, int) {
	return decodeRuneStd(raw)
}
