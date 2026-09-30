// Package vaultgate 提供 VaultGate 的文档修订与审批保留组件。
// 元数据保存在 SQLite，正文保存在本地目录。
package vaultgate

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Decision 是审批终态。
type Decision string

const (
	Approved          Decision = "approved"
	Rejected          Decision = "rejected"
	RevisionRequested Decision = "revision_requested"
)

// Gate 是组件入口。通过 Open 创建。所有方法可并发调用。
type Gate struct {
	dir  string
	st   *store
	pins *pinTracker
	mu   sync.Mutex
}

// Open 在 dir 下打开（必要时创建）一个 Gate。
// dir 下保存 metadata.sqlite 与 bodies/ 正文目录。
func Open(dir string) (*Gate, error) {
	if err := os.MkdirAll(filepath.Join(dir, "bodies"), 0o755); err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(dir, "metadata.sqlite"))
	if err != nil {
		return nil, err
	}
	return &Gate{dir: dir, st: st, pins: newPinTracker()}, nil
}

// Close 关闭底层资源。
func (g *Gate) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st.close()
}

func validDocID(docID string) bool {
	return docID != "" && docID != "." && docID != ".." &&
		!strings.ContainsAny(docID, "/\\\x00")
}

func (g *Gate) bodyPath(docID string, version int64) string {
	return filepath.Join(g.dir, "bodies", docID,
		fmt.Sprintf("%d.body", version))
}

// Publish 从 src 流式接收正文，成功后发布一个不可变修订，返回版本号。
// 版本号按该文档成功发布的顺序从 1 连续递增。
func (g *Gate) Publish(docID string, src io.Reader) (int64, error) {
	if !validDocID(docID) {
		return 0, fmt.Errorf("vaultgate: invalid docID %q", docID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	docDir := filepath.Join(g.dir, "bodies", docID)
	if err := os.MkdirAll(docDir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(docDir, ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, src); err != nil {
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

	cur, _, err := g.st.maxVersion(docID)
	if err != nil {
		return 0, err
	}
	version := cur + 1
	if err := os.Rename(tmpName, g.bodyPath(docID, version)); err != nil {
		return 0, err
	}
	if err := g.st.insertRevision(docID, version, time.Now().UnixNano()); err != nil {
		os.Remove(g.bodyPath(docID, version))
		return 0, err
	}
	if err := g.collectLocked(docID); err != nil {
		return 0, err
	}
	return version, nil
}

// RequestApproval 创建 pending 审批，返回审批 ID 及绑定的当前已发布版本。
func (g *Gate) RequestApproval(docID string) (string, int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	version, ok, err := g.st.maxVersion(docID)
	if err != nil {
		return "", 0, err
	}
	if !ok {
		return "", 0, ErrNoPublished
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", 0, err
	}
	id := hex.EncodeToString(buf[:])
	if err := g.st.insertApproval(id, docID, version); err != nil {
		return "", 0, err
	}
	return id, version, nil
}

// OpenReview 仅对 pending 审批返回绑定版本号和 io.ReadCloser。
// 句柄在 Close 之前持续有效，即使审批随后结束或文档继续发布新版。
func (g *Gate) OpenReview(approvalID string) (int64, io.ReadCloser, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	a, err := g.st.getApproval(approvalID)
	if err != nil {
		return 0, nil, err
	}
	if a.status != "pending" {
		return 0, nil, ErrApprovalClosed
	}
	f, err := os.Open(g.bodyPath(a.docID, a.version))
	if err != nil {
		return 0, nil, fmt.Errorf("vaultgate: open body: %w", err)
	}
	id := g.pins.add(a.docID, a.version)
	return a.version, &reviewHandle{file: f, gate: g, id: id, docID: a.docID}, nil
}

// Respond 将 pending 审批一次性转为终态，返回绑定版本及终态。
// 决定只属于审批绑定的版本，不漂移到当前版。
func (g *Gate) Respond(approvalID string, decision Decision) (int64, Decision, error) {
	switch decision {
	case Approved, Rejected, RevisionRequested:
	default:
		return 0, "", ErrInvalidDecision
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	a, err := g.st.getApproval(approvalID)
	if err != nil {
		return 0, "", err
	}
	ok, err := g.st.decideApproval(approvalID, string(decision))
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", ErrApprovalClosed
	}
	if err := g.collectLocked(a.docID); err != nil {
		return 0, "", err
	}
	return a.version, decision, nil
}

// collectLocked 回收 docID 已失去全部保留理由的版本正文。调用方须持有 g.mu。
func (g *Gate) collectLocked(docID string) error {
	versions, err := g.st.listVersions(docID)
	if err != nil {
		return err
	}
	pending, err := g.st.pendingVersions(docID)
	if err != nil {
		return err
	}
	retain := retainSet(versions, pending, g.pins.versionsFor(docID))
	for _, v := range versions {
		if retain[v] {
			continue
		}
		if err := os.Remove(g.bodyPath(docID, v)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := g.st.deleteRevision(docID, v); err != nil {
			return err
		}
	}
	return nil
}

// reviewHandle 是 OpenReview 返回的阅读句柄。
// Close 释放该句柄对版本的保留理由，并触发回收。
type reviewHandle struct {
	file  *os.File
	gate  *Gate
	id    int64
	docID string
	once  sync.Once
}

func (h *reviewHandle) Read(p []byte) (int, error) {
	return h.file.Read(p)
}

func (h *reviewHandle) Close() error {
	var err error
	h.once.Do(func() {
		err = h.file.Close()
		h.gate.mu.Lock()
		defer h.gate.mu.Unlock()
		h.gate.pins.remove(h.id)
		if gcErr := h.gate.collectLocked(h.docID); err == nil {
			err = gcErr
		}
	})
	return err
}
