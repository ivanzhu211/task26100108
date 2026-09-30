package vaultgate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) (*Gate, string) {
	t.Helper()
	dir := t.TempDir()
	g, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g, dir
}

func bodyExists(dir, docID string, version int64) bool {
	_, err := os.Stat(filepath.Join(dir, "bodies", docID, fmt.Sprintf("%d.body", version)))
	return err == nil
}

func publish(t *testing.T, g *Gate, docID, content string) int64 {
	t.Helper()
	v, err := g.Publish(docID, strings.NewReader(content))
	if err != nil {
		t.Fatalf("Publish(%q): %v", docID, err)
	}
	return v
}

// 要求的场景：旧版两个 pending 审批 + 12 个新版 + 跨审批结束的阅读句柄。
func TestRetentionHandoff(t *testing.T) {
	g, dir := openTemp(t)
	const doc = "handoff-doc"

	v1Content := "V1-BODY:" + strings.Repeat("0123456789abcdef", 4096) // 64 KiB
	if v := publish(t, g, doc, v1Content); v != 1 {
		t.Fatalf("first version = %d, want 1", v)
	}

	a1, bound1, err := g.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval a1: %v", err)
	}
	a2, bound2, err := g.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval a2: %v", err)
	}
	if bound1 != 1 || bound2 != 1 {
		t.Fatalf("approvals bound to v%d, v%d; want v1, v1", bound1, bound2)
	}

	// 发布 12 个新版，超出“当前版 + 最近 10 个历史版”窗口。
	for i := 2; i <= 13; i++ {
		if v := publish(t, g, doc, fmt.Sprintf("V%d-BODY", i)); v != int64(i) {
			t.Fatalf("version = %d, want %d", v, i)
		}
	}
	if !bodyExists(dir, doc, 1) {
		t.Fatal("v1 body collected while two approvals are pending")
	}

	// 打开旧版阅读句柄（通过 a1）。
	rv, rc, err := g.OpenReview(a1)
	if err != nil {
		t.Fatalf("OpenReview: %v", err)
	}
	if rv != 1 {
		t.Fatalf("OpenReview version = %d, want 1", rv)
	}

	// 先读一部分，剩余部分跨审批结束继续读。
	var got bytes.Buffer
	buf := make([]byte, 8192)
	if _, err := io.CopyN(&got, rc, int64(len(buf))); err != nil {
		t.Fatalf("initial read: %v", err)
	}

	// 结束第一个审批：不应回收 v1（a2 仍 pending，且句柄未关）。
	v, final, err := g.Respond(a1, Approved)
	if err != nil {
		t.Fatalf("Respond a1: %v", err)
	}
	if v != 1 || final != Approved {
		t.Fatalf("Respond a1 = (v%d, %s), want (v1, approved)", v, final)
	}
	if !bodyExists(dir, doc, 1) {
		t.Fatal("v1 body collected after first approval ended; a2 and reader still retain it")
	}
	publish(t, g, doc, "V14-BODY")
	if _, err := io.CopyN(&got, rc, int64(len(buf))); err != nil {
		t.Fatalf("read after a1 ended: %v", err)
	}

	// 结束第二个审批：仍不应回收 v1（句柄未关）。
	if _, _, err := g.Respond(a2, Rejected); err != nil {
		t.Fatalf("Respond a2: %v", err)
	}
	if !bodyExists(dir, doc, 1) {
		t.Fatal("v1 body collected after both approvals ended; open reader still retains it")
	}
	publish(t, g, doc, "V15-BODY")

	// 逐字节读完旧正文，必须与原内容完全一致。
	if _, err := io.Copy(&got, rc); err != nil {
		t.Fatalf("final read: %v", err)
	}
	if got.String() != v1Content {
		t.Fatalf("read %d bytes, content mismatch (want %d bytes of v1)", got.Len(), len(v1Content))
	}

	// Close 后 v1 失去最后保留理由，正文实际释放。
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bodyExists(dir, doc, 1) {
		t.Fatal("v1 body still on disk after last retention reason released")
	}
	// 当前版与最近历史版仍在窗口内。
	for v := int64(15); v >= 6; v-- {
		if !bodyExists(dir, doc, v) {
			t.Fatalf("recent v%d body missing", v)
		}
	}
	if bodyExists(dir, doc, 4) {
		t.Fatal("v4 outside retention window but still on disk")
	}
}

func TestBasicOps(t *testing.T) {
	g, _ := openTemp(t)
	const doc = "basic"

	if _, _, err := g.RequestApproval(doc); err != ErrNoPublished {
		t.Fatalf("RequestApproval without publish: %v", err)
	}

	publish(t, g, doc, "one")
	publish(t, g, doc, "two")
	a, bound, err := g.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if bound != 2 {
		t.Fatalf("approval bound to v%d, want v2 (current)", bound)
	}

	if _, _, err := g.OpenReview(a); err != nil {
		t.Fatalf("OpenReview pending: %v", err)
	}

	if _, _, err := g.Respond(a, "nonsense"); err != ErrInvalidDecision {
		t.Fatalf("invalid decision: %v", err)
	}
	v, final, err := g.Respond(a, RevisionRequested)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if v != 2 || final != RevisionRequested {
		t.Fatalf("Respond = (v%d, %s), want (v2, revision_requested)", v, final)
	}
	// 决定不漂移：绑定版本仍是 2，即使当前版继续前进。
	publish(t, g, doc, "three")

	if _, _, err := g.Respond(a, Approved); err != ErrApprovalClosed {
		t.Fatalf("second Respond: %v", err)
	}
	if _, _, err := g.OpenReview(a); err != ErrApprovalClosed {
		t.Fatalf("OpenReview decided: %v", err)
	}
	if _, _, err := g.Respond("missing-approval", Approved); err != ErrApprovalNotFound {
		t.Fatalf("Respond missing: %v", err)
	}
	if _, _, err := g.OpenReview("missing-approval"); err != ErrApprovalNotFound {
		t.Fatalf("OpenReview missing: %v", err)
	}
}

// 同一版本的多个独立审批互不影响。
func TestIndependentApprovals(t *testing.T) {
	g, dir := openTemp(t)
	const doc = "multi"

	publish(t, g, doc, "shared-v1")
	a1, _, _ := g.RequestApproval(doc)
	a2, _, _ := g.RequestApproval(doc)

	if _, _, err := g.Respond(a1, Approved); err != nil {
		t.Fatalf("Respond a1: %v", err)
	}
	// a2 仍 pending，v1 正文必须保留，且 a2 仍可打开阅读。
	if !bodyExists(dir, doc, 1) {
		t.Fatal("v1 collected while a2 still pending")
	}
	rv, rc, err := g.OpenReview(a2)
	if err != nil {
		t.Fatalf("OpenReview a2: %v", err)
	}
	if rv != 1 {
		t.Fatalf("a2 review version = %d, want 1", rv)
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "shared-v1" {
		t.Fatalf("read %q, want %q", data, "shared-v1")
	}
	// 发布足够新版，把 v1 挤出“当前版 + 最近 10 个历史版”窗口。
	for i := 2; i <= 12; i++ {
		publish(t, g, doc, fmt.Sprintf("v%d", i))
	}
	if !bodyExists(dir, doc, 1) {
		t.Fatal("v1 collected while a2 pending and reader open")
	}
	if _, _, err := g.Respond(a2, Rejected); err != nil {
		t.Fatalf("Respond a2: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bodyExists(dir, doc, 1) {
		t.Fatal("v1 still on disk after all retention reasons released")
	}
}
