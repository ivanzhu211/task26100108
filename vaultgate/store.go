package vaultgate

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// store 封装 SQLite 元数据访问。调用方（Gate）持有互斥锁，store 本身不做并发控制。
type store struct {
	db *sql.DB
}

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS revisions (
			doc_id TEXT NOT NULL,
			version INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (doc_id, version)
		)`,
		`CREATE TABLE IF NOT EXISTS approvals (
			id TEXT PRIMARY KEY,
			doc_id TEXT NOT NULL,
			version INTEGER NOT NULL,
			status TEXT NOT NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("vaultgate: init store: %w", err)
		}
	}
	return &store{db: db}, nil
}

func (s *store) close() error {
	return s.db.Close()
}

// maxVersion 返回文档当前最大版本号；没有已发布版本时 ok=false。
func (s *store) maxVersion(docID string) (version int64, ok bool, err error) {
	row := s.db.QueryRow(`SELECT MAX(version) FROM revisions WHERE doc_id = ?`, docID)
	var v sql.NullInt64
	if err := row.Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

func (s *store) insertRevision(docID string, version int64, createdAt int64) error {
	_, err := s.db.Exec(
		`INSERT INTO revisions (doc_id, version, created_at) VALUES (?, ?, ?)`,
		docID, version, createdAt)
	return err
}

func (s *store) deleteRevision(docID string, version int64) error {
	_, err := s.db.Exec(`DELETE FROM revisions WHERE doc_id = ? AND version = ?`, docID, version)
	return err
}

// listVersions 返回文档全部仍有正文的版本号（升序）。
func (s *store) listVersions(docID string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT version FROM revisions WHERE doc_id = ? ORDER BY version`, docID)
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

func (s *store) insertApproval(id, docID string, version int64) error {
	_, err := s.db.Exec(
		`INSERT INTO approvals (id, doc_id, version, status) VALUES (?, ?, ?, 'pending')`,
		id, docID, version)
	return err
}

type approvalRow struct {
	docID   string
	version int64
	status  string
}

func (s *store) getApproval(id string) (approvalRow, error) {
	var a approvalRow
	err := s.db.QueryRow(`SELECT doc_id, version, status FROM approvals WHERE id = ?`, id).
		Scan(&a.docID, &a.version, &a.status)
	if err == sql.ErrNoRows {
		return a, ErrApprovalNotFound
	}
	return a, err
}

// decideApproval 仅当审批仍为 pending 时更新状态；返回是否更新成功。
func (s *store) decideApproval(id string, status string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE approvals SET status = ? WHERE id = ? AND status = 'pending'`, status, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// pendingVersions 返回文档所有 pending 审批绑定的版本号集合。
func (s *store) pendingVersions(docID string) (map[int64]bool, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT version FROM approvals WHERE doc_id = ? AND status = 'pending'`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}
