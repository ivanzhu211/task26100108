// Package vaultgate 提供文档修订与审批保留组件。
// 元数据保存在 SQLite，正文保存在本地目录。
package vaultgate

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Decision 是审批终态。
type Decision string

const (
	DecisionApproved          Decision = "approved"
	DecisionRejected          Decision = "rejected"
	DecisionRevisionRequested Decision = "revision_requested"
)

var (
	// ErrNoPublishedRevision 表示文档尚无已发布版本。
	ErrNoPublishedRevision = errors.New("vaultgate: no published revision")
	// ErrApprovalNotFound 表示审批不存在。
	ErrApprovalNotFound = errors.New("vaultgate: approval not found")
	// ErrApprovalNotPending 表示审批已结束，不再是 pending。
	ErrApprovalNotPending = errors.New("vaultgate: approval not pending")
	// ErrInvalidDecision 表示终态取值非法。
	ErrInvalidDecision = errors.New("vaultgate: invalid decision")
)

// VaultGate 是组件入口。
type VaultGate struct {
	db      *sql.DB
	bodyDir string

	mu      sync.Mutex
	readers map[string]*readerState
}

// Open 打开（必要时创建）一个 VaultGate 实例。
// dbPath 为 SQLite 元数据文件路径，bodyDir 为正文存储目录。
func Open(dbPath, bodyDir string) (*VaultGate, error) {
	if err := os.MkdirAll(bodyDir, 0o755); err != nil {
		return nil, err
	}
	db, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	return &VaultGate{db: db, bodyDir: bodyDir, readers: map[string]*readerState{}}, nil
}

// Close 关闭实例。未关闭的阅读句柄在进程内随之失效，其文件句柄由调用方负责。
func (v *VaultGate) Close() error {
	return v.db.Close()
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Publish 从 src 流式接收正文，成功后发布一个不可变修订，返回版本号。
// 版本号按该文档成功发布的顺序从 1 连续递增。
func (v *VaultGate) Publish(docID string, src io.Reader) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	tx, err := v.db.Begin()
	if err != nil {
		return 0, err
	}
	current, err := currentVersion(tx, docID)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Rollback(); err != nil {
		return 0, err
	}

	version := current + 1
	size, err := writeBody(v.bodyDir, docID, version, src)
	if err != nil {
		return 0, err
	}

	tx, err = v.db.Begin()
	if err != nil {
		removeBody(v.bodyDir, docID, version)
		return 0, err
	}
	if err := insertRevision(tx, docID, version, size); err != nil {
		tx.Rollback()
		removeBody(v.bodyDir, docID, version)
		return 0, err
	}
	if err := v.gcDoc(tx, docID); err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return version, nil
}

// RequestApproval 创建 pending 审批，返回审批 ID 及绑定版本。
// 审批绑定完整的当前已发布版本；同一版本允许存在多个独立审批。
func (v *VaultGate) RequestApproval(docID string) (approvalID string, version int64, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	tx, err := v.db.Begin()
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()

	version, err = currentVersion(tx, docID)
	if err != nil {
		return "", 0, err
	}
	if version == 0 {
		return "", 0, ErrNoPublishedRevision
	}
	approvalID, err = newID()
	if err != nil {
		return "", 0, err
	}
	if err := insertApproval(tx, approvalID, docID, version); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return approvalID, version, nil
}

// reviewHandle 是 OpenReview 返回的阅读句柄。
// Close 之前持续有效，即使审批结束或文档继续发布新版。
type reviewHandle struct {
	v      *VaultGate
	id     string
	file   *os.File
	state  readerState
	closed bool
}

func (h *reviewHandle) Read(p []byte) (int, error) {
	return h.file.Read(p)
}

// Close 释放该句柄对绑定版本的保留理由，并触发正文回收。可重复调用。
func (h *reviewHandle) Close() error {
	h.v.mu.Lock()
	defer h.v.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	delete(h.v.readers, h.id)

	tx, err := h.v.db.Begin()
	if err == nil {
		if gcErr := h.v.gcDoc(tx, h.state.docID); gcErr != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}
	return h.file.Close()
}

// OpenReview 仅对 pending 审批返回绑定版本号和 io.ReadCloser，
// 读取该版本的完整正文。
func (v *VaultGate) OpenReview(approvalID string) (version int64, r io.ReadCloser, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	tx, err := v.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	docID, version, status, err := getApproval(tx, approvalID)
	tx.Rollback()
	if errors.Is(err, errNotFound) {
		return 0, nil, ErrApprovalNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	if status != statusPending {
		return 0, nil, ErrApprovalNotPending
	}

	file, err := openBody(v.bodyDir, docID, version)
	if err != nil {
		return 0, nil, fmt.Errorf("vaultgate: open body: %w", err)
	}
	id, err := newID()
	if err != nil {
		file.Close()
		return 0, nil, err
	}
	v.readers[id] = &readerState{docID: docID, version: version}
	h := &reviewHandle{v: v, id: id, file: file, state: readerState{docID: docID, version: version}}
	return version, h, nil
}

// Respond 将 pending 审批一次性转为 approved、rejected 或
// revision_requested，返回绑定版本及终态。
// 决定只属于审批绑定的版本，不漂移到当前版。
func (v *VaultGate) Respond(approvalID string, decision Decision) (version int64, final Decision, err error) {
	switch decision {
	case DecisionApproved, DecisionRejected, DecisionRevisionRequested:
	default:
		return 0, "", ErrInvalidDecision
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	tx, err := v.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	docID, version, ok, err := finalizeApproval(tx, approvalID, decision)
	if err != nil {
		return 0, "", err
	}
	if !ok {
		_, _, _, err = getApproval(tx, approvalID)
		if errors.Is(err, errNotFound) {
			return 0, "", ErrApprovalNotFound
		}
		if err != nil {
			return 0, "", err
		}
		return 0, "", ErrApprovalNotPending
	}
	if err := v.gcDoc(tx, docID); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	return version, decision, nil
}
