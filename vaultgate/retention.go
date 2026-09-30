package vaultgate

import "sort"

// keepRecent 是“当前版 + 最近历史版”保留窗口的大小。
const keepRecent = 11

// pinTracker 跟踪未关闭阅读句柄对版本的引用（纯内存，句柄生命周期不跨进程）。
type pinTracker struct {
	next    int64
	handles map[int64]pin
}

type pin struct {
	docID   string
	version int64
}

func newPinTracker() *pinTracker {
	return &pinTracker{handles: map[int64]pin{}}
}

func (t *pinTracker) add(docID string, version int64) int64 {
	t.next++
	t.handles[t.next] = pin{docID: docID, version: version}
	return t.next
}

func (t *pinTracker) remove(id int64) (pin, bool) {
	p, ok := t.handles[id]
	if ok {
		delete(t.handles, id)
	}
	return p, ok
}

// versionsFor 返回指定文档当前被未关闭句柄引用的版本集合。
func (t *pinTracker) versionsFor(docID string) map[int64]bool {
	out := map[int64]bool{}
	for _, p := range t.handles {
		if p.docID == docID {
			out[p.version] = true
		}
	}
	return out
}

// retainSet 计算文档正文的保留集合：
// 最近 keepRecent 个版本 ∪ pending 审批绑定版本 ∪ 未关闭句柄绑定版本。
func retainSet(versions []int64, pending, handles map[int64]bool) map[int64]bool {
	retain := map[int64]bool{}
	sorted := append([]int64(nil), versions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	start := len(sorted) - keepRecent
	if start < 0 {
		start = 0
	}
	for _, v := range sorted[start:] {
		retain[v] = true
	}
	for v := range pending {
		retain[v] = true
	}
	for v := range handles {
		retain[v] = true
	}
	return retain
}
