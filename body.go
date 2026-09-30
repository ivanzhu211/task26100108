package vaultgate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// 正文文件存储层。

// readerState 跟踪一个未关闭的阅读句柄。
type readerState struct {
	docID   string
	version int64
}

// docDir 返回文档正文目录，目录名为 docID 的哈希，避免非法字符与冲突。
func docDir(bodyDir, docID string) string {
	sum := sha256.Sum256([]byte(docID))
	return filepath.Join(bodyDir, hex.EncodeToString(sum[:16]))
}

// bodyPath 返回某版本正文文件路径。
func bodyPath(bodyDir, docID string, version int64) string {
	return filepath.Join(docDir(bodyDir, docID), fmt.Sprintf("%d.body", version))
}

// writeBody 将 src 流式写入目标版本正文文件。
// 先写临时文件并 fsync，再原子 rename，返回字节数。
func writeBody(bodyDir, docID string, version int64, src io.Reader) (int64, error) {
	dir := docDir(bodyDir, docID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	n, err := io.Copy(tmp, src)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, bodyPath(bodyDir, docID, version)); err != nil {
		return 0, err
	}
	return n, nil
}

// openBody 打开某版本正文用于读取。
func openBody(bodyDir, docID string, version int64) (*os.File, error) {
	return os.Open(bodyPath(bodyDir, docID, version))
}

// removeBody 删除某版本正文文件；文件不存在视为成功。
func removeBody(bodyDir, docID string, version int64) error {
	err := os.Remove(bodyPath(bodyDir, docID, version))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
