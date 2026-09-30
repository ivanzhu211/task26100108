package vaultgate

import "database/sql"

// 保留集合计算与正文回收。

// keepHistory 是当前版之外保留的最近历史版数量。
const keepHistory = 10

// gcDoc 在事务内回收 docID 已失去全部保留理由的正文。
// 保留集合 = 当前版 ∪ 最近 10 个历史版 ∪ pending 审批绑定版 ∪ 未关闭阅读句柄绑定版。
// 调用方须已持有 v.mu。
func (v *VaultGate) gcDoc(tx *sql.Tx, docID string) error {
	current, err := currentVersion(tx, docID)
	if err != nil {
		return err
	}
	keep := map[int64]bool{}
	if current > 0 {
		for ver := current - keepHistory; ver <= current; ver++ {
			if ver > 0 {
				keep[ver] = true
			}
		}
	}
	pending, err := pendingApprovalVersions(tx, docID)
	if err != nil {
		return err
	}
	for _, ver := range pending {
		keep[ver] = true
	}
	for _, r := range v.readers {
		if r.docID == docID {
			keep[r.version] = true
		}
	}
	versions, err := listVersions(tx, docID)
	if err != nil {
		return err
	}
	for _, ver := range versions {
		if !keep[ver] {
			if err := removeBody(v.bodyDir, docID, ver); err != nil {
				return err
			}
		}
	}
	return nil
}
