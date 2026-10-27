package core

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// 配置档（config.*.yaml）的枚举与切换。
//
// 与 /mode 的 preset 是两件事：preset（config/presets/<mode>.yaml）只切 tool/policy
// 插件子集，可以实时生效；而这里切的是**整份 config.yaml**（provider、插件表、窗口、
// 会话目录……），运行期替换它等于换掉宿主自己的装配——不可能安全热生效。
// 所以本文件只做「枚举 + 备份 + 原子替换」，是否重启由调用方（TUI）决定并明确告知。

// ConfigProfileSummary 一个可切换的配置档（config.*.yaml，排除 example 与生效文件本身）。
type ConfigProfileSummary struct {
	Name     string    // 文件名，如 config.DeepSeek.yaml（不含目录）
	Path     string    // 完整路径
	Size     int64     // 字节数
	Modified time.Time // 修改时间
	Active   bool      // 内容与当前生效的 config.yaml 完全相同
}

// configProfilesDir 返回生效配置文件所在目录（配置档与它同级）。
func (m *Manager) configProfilesDir() string {
	return PDir(m.persistConfigPath())
}

// isConfigProfileName 判断文件名是否为可切换的配置档：
//   - 形如 config.<name>.yaml / config.<name>.yml（config.yaml 自身不匹配，没有中段）；
//   - 名称里含 "example"（任意大小写组合）的一律忽略——那是模板，不是档位。
func isConfigProfileName(name string) bool {
	lower := strings.ToLower(name)
	if !strings.HasPrefix(lower, "config.") {
		return false
	}
	if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
		return false
	}
	if strings.Contains(lower, "example") {
		return false
	}
	// 必须有中段：config..yaml / config.yaml 都不是档位（前者中段为空，后者不含中段）
	dot := strings.LastIndex(name, ".")
	if dot <= len("config.") {
		return false
	}
	return strings.TrimSpace(name[len("config."):dot]) != ""
}

// ListConfigProfiles 列出当前生效配置同目录下的可切换配置档，按文件名排序。
// 供 TUI 的 /config 菜单动态生成——不做任何硬编码，新增一个 config.<x>.yaml
// 就会出现在列表里。
func (m *Manager) ListConfigProfiles() ([]ConfigProfileSummary, error) {
	dir := m.configProfilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("config profiles: read %s: %w", dir, err)
	}
	activePath := m.persistConfigPath()
	activeData, _ := os.ReadFile(activePath) // 读不到就不标 Active

	out := make([]ConfigProfileSummary, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isConfigProfileName(name) {
			continue
		}
		path := PJoin(dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := ConfigProfileSummary{Name: name, Path: path, Size: info.Size(), Modified: info.ModTime()}
		if data, err := os.ReadFile(path); err == nil && len(activeData) > 0 {
			p.Active = bytes.Equal(data, activeData)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out, nil
}

// SwitchConfigProfile 把指定配置档的内容写回生效的 config.yaml：
//  1. 目标必须出现在 ListConfigProfiles 的结果里（只认文件名，天然拒绝路径穿越）；
//  2. 内容必须是可解析的 YAML 映射——否则拒绝，别把宿主配置换成垃圾；
//  3. 先把当前 config.yaml 备份到 config-backups/（沿用启动自愈那套旋转备份）；
//  4. 临时文件 + rename 原子替换。
//
// 返回备份文件路径。**不会**热加载：调用方须告知用户重启生效（新配置里的 provider/
// 插件表与当前进程已装配的内容不一致，热替换只会得到一个半新半旧的宿主）。
func (m *Manager) SwitchConfigProfile(name string) (string, error) {
	base := strings.TrimSpace(name)
	if base != name || base == "" || strings.ContainsAny(base, `/\`) {
		return "", fmt.Errorf("config profile: invalid name %q", name)
	}
	profiles, err := m.ListConfigProfiles()
	if err != nil {
		return "", err
	}
	var target *ConfigProfileSummary
	for i := range profiles {
		if profiles[i].Name == base {
			target = &profiles[i]
			break
		}
	}
	if target == nil {
		return "", fmt.Errorf("config profile %q not found", base)
	}

	data, err := os.ReadFile(target.Path)
	if err != nil {
		return "", fmt.Errorf("config profile: read %s: %w", target.Path, err)
	}
	// 健康检查：必须是 YAML 映射且非空。只在解析失败时拒绝，
	// 不校验字段语义（那是启动时的事，且不同版本字段会演化）。
	var probe map[string]any
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("config profile %s is not valid YAML: %w", base, err)
	}
	if len(probe) == 0 {
		return "", fmt.Errorf("config profile %s is empty (not a YAML mapping)", base)
	}

	activePath := m.persistConfigPath()
	backup, err := BackupGoodFile(activePath, m.logger)
	if err != nil {
		// 备份失败就不要动当前配置：宁可切换失败，也不能让用户失去回退路径。
		return "", fmt.Errorf("config profile: backup current config first: %w", err)
	}
	tmp := activePath + ".switch.tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return "", fmt.Errorf("config profile: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, activePath); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("config profile: replace %s: %w", activePath, err)
	}
	m.logger.Info("config profile switched", "profile", base, "backup", backup)
	return backup, nil
}
