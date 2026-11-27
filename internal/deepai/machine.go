package deepai

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"wild-work/internal/auth"
)

// 机器指纹与实例号。
//
// 上游按**机器**按天发放免费额度（`/theos` 的 `granted` 口径）：同一台机器上多个账号
// 共用一枚指纹会互相抵消（实测现象见 spec §3.2 的取证与参考实现结论）。因此本仓
// **每个账号各持一枚原始 UUID**，存在 `Auth.MachineID` 里（该字段本就用于「机器标识」，
// 老账号读出为空串 ⇒ 不发该头，向后兼容）。
//
// 与桌面端的差别（刻意的）：桌面端取本机的固件/系统标识（Linux 只认 systemd machine-id），
// 本工具是服务端网关，多个账号可能来自不同机器、也可能本来就是同一台机器上的多个号——
// 用宿主机器标识会让全部账号撞成一枚指纹，恰恰是上游要惩罚的形态。故取「每账号随机」。

// machineUUIDBytes UUIDv4 的随机字节数（16 字节 = 32 hex + 4 连字符）。
const machineUUIDBytes = 16

// NewUUID 生成标准 UUIDv4 形态的原始机器标识（带连字符，小写）。
//
// 形态与桌面端 `machineId.ts` 一致：上游对形态没有校验（实测 32 位裸 hex 与带连字符都收），
// 但「像桌面端」比「自创形态」更安全，故沿用 UUIDv4。
func NewUUID() string {
	var b [machineUUIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在正常系统上不可达；返回空串让调用方走「不发该头」而非发坏值。
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Fingerprint 把原始机器标识算成出站头值：`sha256(小写(去空白(原始)))`。
//
// 空原始值返回空串（调用方据此**不发**该头，而不是发一个空值——空值等于自报「无指纹」）。
// 小写归一很关键：桌面端 `machineId.ts` 也是先 lower 再 sha256，大小写不同会得出两枚
// 完全不同的指纹，同一台机器被当成两台。
func Fingerprint(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// EnsureMachine 补齐账号的原始机器标识（新增账号在落盘前生成；老账号在启动加载时补）。
//
// 返回是否发生了写入（调用方据此决定要不要记日志）。写入失败**不阻断**上游调用：
// 内存里已有值、本次可用；下次启动会再补一次。日志只打 uid（零凭据红线）。
func EnsureMachine(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	a.Lock()
	if a.MachineID != "" {
		a.Unlock()
		return false
	}
	id := NewUUID()
	a.MachineID = id
	filePath := a.FilePath
	uid := a.UID
	a.Unlock()
	if id == "" {
		log.Printf("deepai: 机器指纹生成失败（crypto/rand），本次请求不发 %s 头 uid=%s", HeaderMachine, uid)
		return false
	}
	if filePath != "" {
		if err := a.SaveAtomic(); err != nil {
			log.Printf("deepai: 机器指纹落盘失败（本次仍可用）uid=%s err=%v", uid, err)
		}
	}
	return true
}

// instanceID 返回该账号的出站实例号（`X-DeepAI-Code-Instance`，32 位小写 hex）。
//
// 桌面端是「每进程一枚随机 16 字节」，本工具是服务端网关，一个进程承载多个账号：
// 若沿用进程级静态值，多账号会共用同一个实例号（等于自报「这些号来自同一个客户端进程」）。
// 故按**账号**派生：sha256(uid) 取前 32 hex —— 稳定（重启不变、无需额外持久化字段）
// 且每号不同。上游对该头只做存在性判断，不校验取值来源。
func instanceID(uid string) string {
	if uid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("deepai.instance|" + uid))
	return hex.EncodeToString(sum[:16])
}

// accountMachine 返回账号级机器指纹（无原始值 ⇒ 空串 ⇒ 调用方不发该头）。
func accountMachine(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	a.RLock()
	raw := a.MachineID
	a.RUnlock()
	return Fingerprint(raw)
}

// sha256Hex 返回 s 的 sha256 小写 hex（凭据派生专用，不落任何明文）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
