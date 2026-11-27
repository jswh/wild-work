package login_deepai

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 官方桌面端凭据文件导入（spec §8.3）。
//
// 桌面端把登录结果写成**明文 JSON**：`{apiKey, username, isPro}`（权限 0600），
// 路径取自上游 `apps/desktop/DesktopStatePaths.ts` 的口径：
//
//	baseDir  = T3CODE_HOME || ~/.deepai-code
//	stateDir = baseDir/userdata（开发模式为 baseDir/dev）
//	secrets  = stateDir/secrets
//
// 与其它导入器（raccoon / loomy / officeace）同款纪律：**读不到就明确报「未找到桌面凭据」**，
// 不猜路径、不去翻别的目录——猜出来的凭据可能属于另一个账号，写进池子就是对用户的欺骗。

// credentialFile 桌面端凭据文件名。
const credentialFile = "deepai-account.bin"

// DesktopCredentials 是桌面端凭据文件的内容（字段名与桌面端 JSON 一致）。
type DesktopCredentials struct {
	APIKey   string `json:"apiKey"`
	Username string `json:"username"`
	IsPro    bool   `json:"isPro"`
	// Path 实际读到它的文件路径（诊断/面板回显用，不含凭据）。
	Path string `json:"-"`
}

// DesktopDirs 返回桌面端可能存放 secrets 的目录（去重后）。
//
// explicit 非空时只认它（面板可让用户直接给任意层级路径：baseDir / stateDir / secrets / 文件本身）。
func DesktopDirs(explicit string) []string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			return []string{filepath.Dir(explicit)}
		}
		return dedupe([]string{
			filepath.Join(explicit, "userdata", "secrets"),
			filepath.Join(explicit, "secrets"),
			filepath.Join(explicit, "dev", "secrets"),
		})
	}
	var bases []string
	for _, env := range []string{"T3CODE_HOME", "DEEPAI_CODE_HOME"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			bases = append(bases, v)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		bases = append(bases, filepath.Join(home, ".deepai-code"))
	}
	var out []string
	for _, base := range bases {
		out = append(out,
			filepath.Join(base, "userdata", "secrets"),
			filepath.Join(base, "secrets"),
			filepath.Join(base, "dev", "secrets"),
		)
	}
	return dedupe(out)
}

// FindDesktopCredentials 探测本机桌面端凭据（找不到就明确报错，不猜）。
func FindDesktopCredentials(explicit string) (DesktopCredentials, error) {
	dirs := DesktopDirs(explicit)
	var lastErr error
	for _, dir := range dirs {
		path := filepath.Join(dir, credentialFile)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		cred, err := readCredentials(path)
		if err != nil {
			lastErr = err
			continue
		}
		return cred, nil
	}
	if lastErr != nil {
		return DesktopCredentials{}, fmt.Errorf("读取桌面端凭据失败：%w", lastErr)
	}
	return DesktopCredentials{}, fmt.Errorf(
		"未找到 DeepAI Code 桌面端凭据（已尝试：%s）。请先登录桌面端，或改用「粘贴 API Key」", strings.Join(dirs, ", "))
}

// readCredentials 读并校验一个凭据文件。
func readCredentials(path string) (DesktopCredentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return DesktopCredentials{}, err
	}
	var cred DesktopCredentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		return DesktopCredentials{}, fmt.Errorf("%s 不是合法的凭据 JSON：%w", filepath.Base(path), err)
	}
	if strings.TrimSpace(cred.APIKey) == "" {
		return DesktopCredentials{}, errors.New(filepath.Base(path) + " 里没有 apiKey")
	}
	cred.Path = path
	return cred, nil
}

// dedupe 去重（大小写不敏感，Windows/macOS 的文件系统口径）。
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if strings.TrimSpace(v) == "" {
			continue
		}
		k := strings.ToLower(filepath.Clean(v))
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}
