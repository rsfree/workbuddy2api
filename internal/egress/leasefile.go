// 租约文件读写：登录时写、启动时读。
//
// 为什么单独一个文件而不是塞进 config.json：
//   - config.json 是**人工维护**的配置（面板可热改），租约是**程序写入**的运行时状态，
//     两者混在一起会导致「保存配置」把租约覆盖掉。
//   - 换机器/回滚时，删掉这个文件就等于「全部回哈希」，是干净的运维开关。
package egress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// leaseFile 磁盘格式。带 version 便于将来迁移。
type leaseFile struct {
	Version int               `json:"version"`
	Leases  map[string]string `json:"leases"`
}

const leaseFileVersion = 1

// LoadLeaseFile 读取租约文件。文件不存在返回空表（不是错误）。
func LoadLeaseFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var f leaseFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse lease file: %w", err)
	}
	if f.Leases == nil {
		return map[string]string{}, nil
	}
	return f.Leases, nil
}

// SaveLeaseFile 原子写回租约文件（tmp + rename）。
func SaveLeaseFile(path string, m map[string]string) error {
	if path == "" {
		return nil
	}
	if m == nil {
		m = map[string]string{}
	}
	raw, err := json.MarshalIndent(leaseFile{Version: leaseFileVersion, Leases: m}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
