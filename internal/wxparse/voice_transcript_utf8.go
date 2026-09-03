package wxparse

import "unicode/utf8"

func decodeRuneStd(raw []byte) (rune, int) {
	return utf8.DecodeRune(raw)
}
