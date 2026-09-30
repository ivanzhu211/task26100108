package vaultgate

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// SQLite 元数据层。

const schema = `
CREATE TABLE IF NOT EXISTS documents (
	doc_id          TEXT PRIMARY KEY,
	current_version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS revisions (
	doc_id   TEXT NOT NULL,
	version  INTEGER NOT NULL,
	size     INTEGER NOT NULL,
	PRIMARY KEY (doc_id, version)
);
CREATE TABLE IF NOT EXISTS approvals (
	approval_id TEXT PRIMARY KEY,
	doc_id      TEXT NOT NULL,
	version     INTEGER NOT NULL,
	status      TEXT NOT NULL
);
`

const (
	statusPending = "pending"
)

var errNotFound = errors.New("vaultgate: not found")

func openStore(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// currentVersion 返回文档当前已发布版本；无记录时返回 0。
func currentVersion(tx *sql.Tx, docID string) (int64, error) {
	var v int64
	err := tx.QueryRow(`SELECT current_version FROM documents WHERE doc_id = ?`, docID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// insertRevision 在事务内发布新版本并推进当前版本指针。
func insertRevision(tx *sql.Tx, docID string, version, size int64) error {
	if _, err := tx.Exec(`INSERT INTO revisions (doc_id, version, size) VALUES (?, ?, ?)`, docID, version, size); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO documents (doc_id, current_version) VALUES (?, ?)
		ON CONFLICT(doc_id) DO UPDATE SET current_version = excluded.current_version`, docID, version)
	return err
}

// insertApproval 记录一条 pending 审批。
func insertApproval(tx *sql.Tx, approvalID, docID string, version int64) error {
	_, err := tx.Exec(`INSERT INTO approvals (approval_id, doc_id, version, status) VALUES (?, ?, ?, ?)`,
		approvalID, docID, version, statusPending)
	return err
}

// getApproval 读取审批；不存在时返回 errNotFound。
func getApproval(tx *sql.Tx, approvalID string) (docID string, version int64, status string, err error) {
	err = tx.QueryRow(`SELECT doc_id, version, status FROM approvals WHERE approval_id = ?`, approvalID).
		Scan(&docID, &version, &status)
	if errors.Is(err, sql.ErrNoRows) {
		err = errNotFound
	}
	return
}

// finalizeApproval 将 pending 审批一次性原子地转为终态。
// 若审批不存在或已结束，返回 ok=false。
func finalizeApproval(tx *sql.Tx, approvalID string, final Decision) (docID string, version int64, ok bool, err error) {
	res, err := tx.Exec(`UPDATE approvals SET status = ? WHERE approval_id = ? AND status = ?`,
		string(final), approvalID, statusPending)
	if err != nil {
		return "", 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", 0, false, err
	}
	if n == 0 {
		return "", 0, false, nil
	}
	err = tx.QueryRow(`SELECT doc_id, version FROM approvals WHERE approval_id = ?`, approvalID).
		Scan(&docID, &version)
	if err != nil {
		return "", 0, false, err
	}
	return docID, version, true, nil
}

// listVersions 返回文档所有已记录修订的版本号。
func listVersions(tx *sql.Tx, docID string) ([]int64, error) {
	rows, err := tx.Query(`SELECT version FROM revisions WHERE doc_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// pendingApprovalVersions 返回文档所有 pending 审批绑定的版本号。
func pendingApprovalVersions(tx *sql.Tx, docID string) ([]int64, error) {
	rows, err := tx.Query(`SELECT version FROM approvals WHERE doc_id = ? AND status = ?`, docID, statusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
