package vaultgate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*VaultGate, string) {
	t.Helper()
	dir := t.TempDir()
	bodyDir := filepath.Join(dir, "bodies")
	vg, err := Open(filepath.Join(dir, "meta.db"), bodyDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { vg.Close() })
	return vg, bodyDir
}

// content 为每个版本生成可区分、可校验的正文。
func content(docID string, version int64) []byte {
	return []byte(fmt.Sprintf("doc=%s version=%d payload=%s",
		docID, version, string(bytes.Repeat([]byte{byte('a' + version%26)}, 4096))))
}

func publish(t *testing.T, vg *VaultGate, docID string, version int64) {
	t.Helper()
	got, err := vg.Publish(docID, bytes.NewReader(content(docID, version)))
	if err != nil {
		t.Fatalf("Publish(%s) expect version %d: %v", docID, version, err)
	}
	if got != version {
		t.Fatalf("Publish(%s) = version %d, want %d", docID, got, version)
	}
}

func bodyExists(t *testing.T, bodyDir, docID string, version int64) bool {
	t.Helper()
	_, err := os.Stat(bodyPath(bodyDir, docID, version))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat body: %v", err)
	return false
}

// TestRetentionHandover 是题目要求的核心场景：
// 旧版两个 pending 审批，发布 12+ 新版后打开旧版阅读句柄，
// 依次结束审批并继续发布，验证单个审批结束不会过早回收，
// 读者逐字节读完旧正文，Close 后旧正文实际释放。
func TestRetentionHandover(t *testing.T) {
	vg, bodyDir := openTemp(t)
	doc := "doc-A"

	publish(t, vg, doc, 1)

	a1, v1, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval a1: %v", err)
	}
	a2, v2, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval a2: %v", err)
	}
	if v1 != 1 || v2 != 1 {
		t.Fatalf("approvals bound to %d,%d, want 1,1", v1, v2)
	}

	// 发布 12 个新版，当前版到 13，保留窗口为 [3,13]。
	for ver := int64(2); ver <= 13; ver++ {
		publish(t, vg, doc, ver)
	}
	if !bodyExists(t, bodyDir, doc, 1) {
		t.Fatal("v1 body must survive: retained by two pending approvals")
	}

	// 打开旧版阅读句柄。
	rv, reader, err := vg.OpenReview(a1)
	if err != nil {
		t.Fatalf("OpenReview(a1): %v", err)
	}
	if rv != 1 {
		t.Fatalf("OpenReview(a1) version = %d, want 1", rv)
	}

	// 先读一部分，制造“边发布边读”的交错。
	want := content(doc, 1)
	head := make([]byte, 1000)
	if _, err := io.ReadFull(reader, head); err != nil {
		t.Fatalf("read head: %v", err)
	}
	if !bytes.Equal(head, want[:1000]) {
		t.Fatal("head bytes mismatch")
	}

	// 结束第二个审批：v1 仍被 a1(pending) 与读者保留。
	bv, final, err := vg.Respond(a2, DecisionApproved)
	if err != nil {
		t.Fatalf("Respond(a2): %v", err)
	}
	if bv != 1 || final != DecisionApproved {
		t.Fatalf("Respond(a2) = (%d,%s), want (1,approved)", bv, final)
	}
	for ver := int64(14); ver <= 16; ver++ {
		publish(t, vg, doc, ver)
	}
	if !bodyExists(t, bodyDir, doc, 1) {
		t.Fatal("v1 body must survive after a2 ended: a1 and reader still retain it")
	}

	// 结束第一个审批：v1 仍被未关闭的读者保留。
	bv, final, err = vg.Respond(a1, DecisionRevisionRequested)
	if err != nil {
		t.Fatalf("Respond(a1): %v", err)
	}
	if bv != 1 || final != DecisionRevisionRequested {
		t.Fatalf("Respond(a1) = (%d,%s), want (1,revision_requested)", bv, final)
	}
	for ver := int64(17); ver <= 18; ver++ {
		publish(t, vg, doc, ver)
	}
	if !bodyExists(t, bodyDir, doc, 1) {
		t.Fatal("v1 body must survive after both approvals ended: open reader retains it")
	}

	// 读完剩余字节，必须逐字节等于 v1 正文，不混入其他版本。
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	got := append(head, rest...)
	if !bytes.Equal(got, want) {
		t.Fatalf("reader bytes mismatch: got %d bytes, want %d", len(got), len(want))
	}

	// Close 后 v1 失去最后保留理由，正文实际释放。
	if err := reader.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}
	if bodyExists(t, bodyDir, doc, 1) {
		t.Fatal("v1 body must be released after last retention reason is gone")
	}

	// 保留窗口检查：当前版 18，窗口 [8,18] 存在，更早版本已回收。
	for ver := int64(8); ver <= 18; ver++ {
		if !bodyExists(t, bodyDir, doc, ver) {
			t.Fatalf("v%d body must exist (current + recent 10)", ver)
		}
	}
	for ver := int64(2); ver <= 7; ver++ {
		if bodyExists(t, bodyDir, doc, ver) {
			t.Fatalf("v%d body must be collected (outside retention set)", ver)
		}
	}
}

// TestApprovalBoundVersionNoDrift 验证决定只属于绑定版本。
func TestApprovalBoundVersionNoDrift(t *testing.T) {
	vg, _ := openTemp(t)
	doc := "doc-B"

	publish(t, vg, doc, 1)
	a, v, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if v != 1 {
		t.Fatalf("approval bound to %d, want 1", v)
	}
	publish(t, vg, doc, 2)
	publish(t, vg, doc, 3)

	bv, final, err := vg.Respond(a, DecisionRejected)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if bv != 1 || final != DecisionRejected {
		t.Fatalf("Respond = (%d,%s), want (1,rejected): decision must not drift to current version", bv, final)
	}
}

// TestIndependentApprovals 验证同一版本的多个审批相互独立。
func TestIndependentApprovals(t *testing.T) {
	vg, bodyDir := openTemp(t)
	doc := "doc-C"

	publish(t, vg, doc, 1)
	a1, _, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval a1: %v", err)
	}
	a2, _, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval a2: %v", err)
	}
	for ver := int64(2); ver <= 15; ver++ {
		publish(t, vg, doc, ver)
	}

	if _, _, err := vg.Respond(a1, DecisionApproved); err != nil {
		t.Fatalf("Respond(a1): %v", err)
	}
	// a1 结束不影响 a2：v1 仍保留，a2 仍可打开审阅。
	if !bodyExists(t, bodyDir, doc, 1) {
		t.Fatal("v1 must survive: a2 still pending")
	}
	rv, reader, err := vg.OpenReview(a2)
	if err != nil {
		t.Fatalf("OpenReview(a2): %v", err)
	}
	if rv != 1 {
		t.Fatalf("OpenReview(a2) version = %d, want 1", rv)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content(doc, 1)) {
		t.Fatal("bytes mismatch")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpenReviewRequiresPending 验证已结束审批不能再打开审阅。
func TestOpenReviewRequiresPending(t *testing.T) {
	vg, _ := openTemp(t)
	doc := "doc-D"

	publish(t, vg, doc, 1)
	a, _, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if _, _, err := vg.Respond(a, DecisionApproved); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if _, _, err := vg.OpenReview(a); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("OpenReview after Respond = %v, want ErrApprovalNotPending", err)
	}
	if _, _, err := vg.Respond(a, DecisionRejected); !errors.Is(err, ErrApprovalNotPending) {
		t.Fatalf("second Respond = %v, want ErrApprovalNotPending", err)
	}
	if _, _, err := vg.OpenReview("no-such-approval"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("OpenReview unknown = %v, want ErrApprovalNotFound", err)
	}
	if _, _, err := vg.Respond("no-such-approval", DecisionApproved); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("Respond unknown = %v, want ErrApprovalNotFound", err)
	}
	if _, _, err := vg.Respond(a, "nonsense"); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("Respond invalid decision = %v, want ErrInvalidDecision", err)
	}
}

// TestRequestApprovalRequiresRevision 验证无已发布版本时不能创建审批。
func TestRequestApprovalRequiresRevision(t *testing.T) {
	vg, _ := openTemp(t)
	if _, _, err := vg.RequestApproval("empty-doc"); !errors.Is(err, ErrNoPublishedRevision) {
		t.Fatalf("RequestApproval = %v, want ErrNoPublishedRevision", err)
	}
}

// TestRetentionWindow 验证无审批无读者时只保留当前版与最近 10 个历史版。
func TestRetentionWindow(t *testing.T) {
	vg, bodyDir := openTemp(t)
	doc := "doc-E"

	for ver := int64(1); ver <= 15; ver++ {
		publish(t, vg, doc, ver)
	}
	for ver := int64(5); ver <= 15; ver++ {
		if !bodyExists(t, bodyDir, doc, ver) {
			t.Fatalf("v%d must exist (current + recent 10)", ver)
		}
	}
	for ver := int64(1); ver <= 4; ver++ {
		if bodyExists(t, bodyDir, doc, ver) {
			t.Fatalf("v%d must be collected", ver)
		}
	}
}

// TestStreamingPublish 验证大正文流式发布与逐字节一致。
func TestStreamingPublish(t *testing.T) {
	vg, _ := openTemp(t)
	doc := "doc-F"

	big := make([]byte, 8<<20)
	for i := range big {
		big[i] = byte(i * 31)
	}
	if _, err := vg.Publish(doc, bytes.NewReader(big)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	a, _, err := vg.RequestApproval(doc)
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	_, reader, err := vg.OpenReview(a)
	if err != nil {
		t.Fatalf("OpenReview: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("streamed bytes mismatch")
	}
}
