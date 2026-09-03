//go:build windows

package wxkey

import (
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/r266-tech/wechat-cli/internal/config"
)

// 微信 4.x V2 图片文件头。
// 结构是：6B magic + 4B aes_size + 4B xor_size + 1B pad，
// 因此第一块 AES 密文从 offset=15 开始。
var windowsImageV2Magic = []byte{0x07, 0x08, 0x56, 0x32, 0x08, 0x07}

type windowsImageProbe struct {
	CipherBlock []byte
	Template    string
	XORKey      *int
}

// runImageKey 是 Windows 的内置图片密钥恢复路径。
//
// DB key 和图片 key 是两套独立材料：setup_windows.go 已经能从 Weixin.exe
// 只读扫描 DB key；这里再用一张真实 V2 .dat 的 AES 首块作为“验算题”，
// 扫描 Weixin.exe 内存中的 32 字节字母数字候选串，取前 16 字节按 AES-128
// ECB 解密。只有解出的首块出现 JPEG/PNG/GIF/RIFF/wxgf 魔数才接受候选。
//
// XOR key 不需要扫描进程：微信缩略图尾部明文通常以 JPEG FF D9 结束，
// 因此可从 *_t.dat 最后两字节反推出单字节 XOR key。整个流程只读微信
// 进程和本地文件，不注入、不重启、不修改微信。
func runImageKey(root string) (*ImageKeyResult, string, error) {
	if strings.TrimSpace(root) == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, "", err
		}
		root = cfg.DBRoot
	}
	if strings.TrimSpace(root) == "" {
		return nil, "", fmt.Errorf("WeChat DB root is empty; run cache refresh/key setup first")
	}

	probe, err := windowsFindImageProbe(root)
	if err != nil {
		return nil, "", err
	}
	procs, err := windowsTargetProcesses()
	if err != nil {
		return nil, "", err
	}
	if len(procs) == 0 {
		return nil, "", fmt.Errorf("no running Weixin.exe/WeChat.exe process found; log in to Windows WeChat first")
	}
	// DB key 扫描已经记录了真正命中过密钥的 KeyPID。图片 AES key 通常也在
	// 同一个主进程里，优先扫描它可以避免先把多个 renderer/helper 子进程扫完。
	// 这既显著降低耗时，也减少对工作集较大的微信实例做无意义 ReadProcessMemory。
	if cfg, loadErr := config.Load(); loadErr == nil && cfg.KeyPID > 0 {
		keyPID := uint32(cfg.KeyPID)
		sort.SliceStable(procs, func(i, j int) bool {
			if procs[i].pid == keyPID {
				return true
			}
			if procs[j].pid == keyPID {
				return false
			}
			return false
		})
	}

	deadline := time.Now().Add(windowsImageKeyScanTimeout())
	var regions int
	var bytesScanned uint64
	var candidates int
	for _, p := range procs {
		key, r, b, c, scanErr := windowsScanProcessForImageKey(p.pid, probe.CipherBlock, deadline)
		regions += r
		bytesScanned += b
		candidates += c
		if key != "" {
			return &ImageKeyResult{
				Key:            key,
				XORKey:         probe.XORKey,
				TemplateFile:   probe.Template,
				TemplateSource: "local-v2-dat",
				Regions:        regions,
				BytesScanned:   bytesScanned,
				Candidates:     candidates,
			}, fmt.Sprintf("Windows image-key scan OK: pid=%d regions=%d candidates=%d\n", p.pid, regions, candidates), nil
		}
		if scanErr != nil && windowsKeyScanDeadlineExceeded(deadline) {
			break
		}
	}
	return nil, "", fmt.Errorf("no usable WeChat V4 image AES key found in running Weixin.exe; open/view an image in WeChat and retry")
}

func windowsImageKeyScanTimeout() time.Duration {
	raw := firstEnv("WECHAT_CLI_IMAGE_KEY_SCAN_TIMEOUT", "WX_MCP_IMAGE_KEY_SCAN_TIMEOUT")
	if raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Minute
}

// windowsFindImageProbe 选择近期 V2 .dat，并从第一块密文构造密钥验证样本。
// 同时对近期缩略图尾字节做众数统计，尽量得到账号级 XOR key。
func windowsFindImageProbe(root string) (*windowsImageProbe, error) {
	base := filepath.Join(root, "msg", "attach")
	type candidate struct {
		path  string
		mtime time.Time
	}
	var v2 []candidate
	tails := map[[2]byte]int{}
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".dat") {
			return nil
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		head := make([]byte, 31)
		n, _ := f.Read(head)
		if strings.HasSuffix(strings.ToLower(d.Name()), "_t.dat") {
			if _, seekErr := f.Seek(-2, 2); seekErr == nil {
				var tail [2]byte
				if readN, _ := f.Read(tail[:]); readN == 2 {
					tails[tail]++
				}
			}
		}
		_ = f.Close()
		if n >= 31 && string(head[:6]) == string(windowsImageV2Magic) {
			if info, statErr := d.Info(); statErr == nil {
				v2 = append(v2, candidate{path: path, mtime: info.ModTime()})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan WeChat image cache: %w", err)
	}
	if len(v2) == 0 {
		return nil, fmt.Errorf("no WeChat V4 image .dat found under %s", base)
	}
	sort.Slice(v2, func(i, j int) bool { return v2[i].mtime.After(v2[j].mtime) })

	f, err := os.Open(v2[0].path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	header := make([]byte, 31)
	if n, readErr := f.Read(header); readErr != nil || n < len(header) {
		return nil, fmt.Errorf("read image-key probe: %w", readErr)
	}
	// 读出 size 字段做基本 sanity check，避免把碰巧同 magic 的损坏文件当探针。
	aesSize := binary.LittleEndian.Uint32(header[6:10])
	if aesSize == 0 {
		return nil, fmt.Errorf("invalid WeChat V4 image probe: aes_size=0")
	}

	var xor *int
	bestCount := 0
	for tail, count := range tails {
		key := int(tail[0] ^ 0xff)
		if int(tail[1]^0xd9) == key && count > bestCount {
			k := key
			xor = &k
			bestCount = count
		}
	}
	return &windowsImageProbe{
		CipherBlock: append([]byte(nil), header[15:31]...),
		Template:    v2[0].path,
		XORKey:      xor,
	}, nil
}

func windowsScanProcessForImageKey(pid uint32, probe []byte, deadline time.Time) (string, int, uint64, int, error) {
	h, _, err := procOpenProcess.Call(processVMRead|processQueryInformation, 0, uintptr(pid))
	if h == 0 {
		return "", 0, 0, 0, err
	}
	defer procCloseHandle.Call(h)

	const maxUserAddress = uintptr(0x00007fffffffffff)
	regions := 0
	var scanned uint64
	candidates := 0
	seen := map[string]bool{}
	for addr := uintptr(0); addr < maxUserAddress; {
		if windowsKeyScanDeadlineExceeded(deadline) {
			return "", regions, scanned, candidates, errWindowsKeyScanDeadline
		}
		var m windowsMemoryBasicInformation
		r, _, _ := procVirtualQueryEx.Call(h, addr, uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m))
		if r == 0 {
			addr += 0x10000
			continue
		}
		next := m.BaseAddress + m.RegionSize
		if next <= addr {
			break
		}
		if windowsReadableRegion(m) && m.RegionSize < 32<<20 {
			regions++
			key, bytesRead, tried := windowsScanImageRegion(h, m.BaseAddress, m.RegionSize, probe, seen, deadline)
			scanned += bytesRead
			candidates += tried
			if key != "" {
				return key, regions, scanned, candidates, nil
			}
		}
		addr = next
	}
	return "", regions, scanned, candidates, nil
}

func windowsScanImageRegion(process uintptr, base, size uintptr, probe []byte, seen map[string]bool, deadline time.Time) (string, uint64, int) {
	const chunkSize = 4 << 20
	var overlap []byte
	var scanned uint64
	tried := 0
	for off := uintptr(0); off < size; {
		if windowsKeyScanDeadlineExceeded(deadline) {
			return "", scanned, tried
		}
		n := chunkSize
		if remain := size - off; remain < uintptr(n) {
			n = int(remain)
		}
		buf := make([]byte, n)
		var got uintptr
		r, _, _ := procReadProcessMemory.Call(process, base+off, uintptr(unsafe.Pointer(&buf[0])), uintptr(n), uintptr(unsafe.Pointer(&got)))
		if r != 0 && got > 0 {
			scanned += uint64(got)
			data := append(append([]byte{}, overlap...), buf[:got]...)
			if key, count := windowsFindImageKeyInBytes(data, probe, seen); key != "" {
				return key, scanned, tried + count
			} else {
				tried += count
			}
			if len(data) > 96 {
				overlap = append(overlap[:0], data[len(data)-96:]...)
			} else {
				overlap = append(overlap[:0], data...)
			}
		}
		off += uintptr(n)
	}
	return "", scanned, tried
}

// windowsFindImageKeyInBytes 识别两种微信进程里常见的候选形式：
// 32 字节 ASCII 字母数字串，以及同内容的 UTF-16LE。真正的 AES-128 key
// 是候选的前 16 个字符；每个候选都必须通过真实图片密文验证。
func windowsFindImageKeyInBytes(data, probe []byte, seen map[string]bool) (string, int) {
	tried := 0
	// ASCII 路径按“连续字母数字 run”扫描，而不是对每个字节都向后重查
	// 32 次。进程内存通常数百 MB，这个线性写法能把扫描成本降一个数量级。
	for i := 0; i < len(data); {
		if !isImageKeyChar(data[i]) {
			i++
			continue
		}
		start := i
		for i < len(data) && isImageKeyChar(data[i]) {
			i++
		}
		if i-start != 32 {
			continue
		}
		key := string(data[start : start+16])
		if !seen[key] {
			seen[key] = true
			tried++
			if windowsImageKeyValid(key, probe) {
				return key, tried
			}
		}
	}

	// UTF-16LE 仍按可能的起点探测，但失败时尽量跳过明显不可能的区域。
	for i := 0; i+64 <= len(data); {
		if !isImageKeyChar(data[i]) || data[i+1] != 0 {
			i++
			continue
		}
		ok := true
		chars := make([]byte, 32)
		for j := 0; j < 32; j++ {
			c := data[i+j*2]
			if !isImageKeyChar(c) || data[i+j*2+1] != 0 {
				ok = false
				break
			}
			chars[j] = c
		}
		if !ok {
			i += 2
			continue
		}
		key := string(chars[:16])
		if seen[key] {
			i += 64
			continue
		}
		seen[key] = true
		tried++
		if windowsImageKeyValid(key, probe) {
			return key, tried
		}
		i += 64
	}
	return "", tried
}

func isImageKeyChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func windowsImageKeyValid(key string, probe []byte) bool {
	if len(key) != 16 || len(probe) < aes.BlockSize {
		return false
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return false
	}
	plain := make([]byte, aes.BlockSize)
	block.Decrypt(plain, probe[:aes.BlockSize])
	return windowsImagePlainMagic(plain)
}

func windowsImagePlainMagic(p []byte) bool {
	if len(p) < 4 {
		return false
	}
	if p[0] == 0xff && p[1] == 0xd8 && p[2] == 0xff {
		return true
	}
	return string(p[:4]) == "\x89PNG" || string(p[:4]) == "GIF8" || string(p[:4]) == "RIFF" || string(p[:4]) == "wxgf"
}
