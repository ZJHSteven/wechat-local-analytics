//go:build windows

package wxkey

import (
	"crypto/aes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsFindImageKeyInBytesASCII(t *testing.T) {
	key := "a1B2c3D4e5F6g7H8"
	probe := encryptedImageProbe(t, key)
	// 微信进程里常见的是 32 字节字母数字候选，真正 AES key 取前 16 字节。
	data := []byte("!" + key + "abcdefghijklmnop" + "!")
	got, tried := windowsFindImageKeyInBytes(data, probe, map[string]bool{})
	if got != key || tried < 1 {
		t.Fatalf("got=%q tried=%d, want key=%q", got, tried, key)
	}
}

func TestWindowsFindImageKeyInBytesUTF16(t *testing.T) {
	key := "a1B2c3D4e5F6g7H8"
	probe := encryptedImageProbe(t, key)
	raw := []byte(key + "abcdefghijklmnop")
	data := make([]byte, 0, len(raw)*2)
	for _, c := range raw {
		data = append(data, c, 0)
	}
	got, _ := windowsFindImageKeyInBytes(data, probe, map[string]bool{})
	if got != key {
		t.Fatalf("got=%q, want key=%q", got, key)
	}
}

func TestWindowsFindImageProbeInfersXOR(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "msg", "attach", "chat", "2026-09", "Img")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := "a1B2c3D4e5F6g7H8"
	xorKey := byte(0x62)
	block := encryptedImageProbe(t, key)
	header := make([]byte, 31)
	copy(header[:6], windowsImageV2Magic)
	binary.LittleEndian.PutUint32(header[6:10], 100)
	binary.LittleEndian.PutUint32(header[10:14], 2)
	copy(header[15:31], block)
	body := append(header, 0xff^xorKey, 0xd9^xorKey)
	path := filepath.Join(dir, "sample_t.dat")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := windowsFindImageProbe(root)
	if err != nil {
		t.Fatal(err)
	}
	if probe.XORKey == nil || *probe.XORKey != int(xorKey) {
		t.Fatalf("xor=%v, want %d", probe.XORKey, xorKey)
	}
	if !windowsImageKeyValid(key, probe.CipherBlock) {
		t.Fatal("probe must validate test AES key")
	}
}

func encryptedImageProbe(t *testing.T, key string) []byte {
	t.Helper()
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, aes.BlockSize)
	copy(plain, []byte{0xff, 0xd8, 0xff, 0xe0, 'J', 'F', 'I', 'F'})
	out := make([]byte, aes.BlockSize)
	block.Encrypt(out, plain)
	return out
}
