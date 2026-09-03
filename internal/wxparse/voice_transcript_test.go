package wxparse

import "testing"

func TestNativeVoiceTranscriptCompleted(t *testing.T) {
	packed := protoFieldBytes(5, append(protoFieldVarint(1, 2), protoFieldBytes(2, []byte("老师说明天下午三点开会"))...))
	if got := NativeVoiceTranscript(packed); got != "老师说明天下午三点开会" {
		t.Fatalf("got %q", got)
	}
}

func TestNativeVoiceTranscriptRejectsIncomplete(t *testing.T) {
	packed := protoFieldBytes(5, append(protoFieldVarint(1, 1), protoFieldBytes(2, []byte("半成品"))...))
	if got := NativeVoiceTranscript(packed); got != "" {
		t.Fatalf("incomplete transcript must be ignored, got %q", got)
	}
}

func TestNativeVoiceTranscriptRejectsUnrelatedString(t *testing.T) {
	packed := protoFieldBytes(4, []byte("wxid_not_a_transcript"))
	if got := NativeVoiceTranscript(packed); got != "" {
		t.Fatalf("unrelated packed string must be ignored, got %q", got)
	}
}

func protoFieldVarint(number int, value uint64) []byte {
	out := appendProtoVarint(nil, uint64(number<<3))
	return appendProtoVarint(out, value)
}

func protoFieldBytes(number int, value []byte) []byte {
	out := appendProtoVarint(nil, uint64(number<<3|2))
	out = appendProtoVarint(out, uint64(len(value)))
	return append(out, value...)
}

func appendProtoVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}
