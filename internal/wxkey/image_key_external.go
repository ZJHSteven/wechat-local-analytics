//go:build !windows

package wxkey

// runImageKey 在非 Windows 平台继续复用独立 wxkey helper。
// 这样平台差异被限制在 wxkey 包内部，wechat-cli 上层完全不需要知道
// 图片密钥来自外部 helper 还是 Windows 进程内扫描。
func runImageKey(root string) (*ImageKeyResult, string, error) {
	return runImageKeyExternal(root)
}
