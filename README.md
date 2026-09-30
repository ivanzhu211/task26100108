# VaultGate — 文档修订与审批保留组件

Go 库。元数据保存在 SQLite（`modernc.org/sqlite`，纯 Go 无 cgo），正文保存在本地目录。

## API

- `Open(dbPath, bodyDir)` / `Close()`
- `Publish(docID, src io.Reader) (version int64, err error)` — 流式发布不可变修订，版本号从 1 连续递增
- `RequestApproval(docID) (approvalID string, version int64, err error)` — 创建 pending 审批，绑定当前已发布版本；同版本允许多个独立审批
- `OpenReview(approvalID) (version int64, r io.ReadCloser, err error)` — 仅 pending 审批可打开，句柄 Close 前持续有效
- `Respond(approvalID, decision) (version int64, final Decision, err error)` — pending 一次性转为 `approved` / `rejected` / `revision_requested`，决定只属于绑定版本

## 运行测试

```sh
go test ./...
```

本仓库环境（沙箱、无网络）下使用本地模块缓存：

```sh
GOMODCACHE=$PWD/.gomodcache GOCACHE=$PWD/.gocache GOFLAGS=-mod=mod go test -race -count=1 -v ./...
```

## 保留责任交接

每个文档的正文保留集合为四类的并集：**当前版**、**最近 10 个历史版**、
**所有 pending 审批绑定版本**、**所有未关闭阅读句柄绑定版本**。

保留理由是独立计数、独立释放的：

- `Publish` 推进当前版后，滑出“当前 + 最近 10”窗口的版本若仍被 pending
  审批或打开中的读者引用，正文继续保留。
- `Respond` 只释放该审批自己的保留理由；同一版本上的其他 pending 审批和
  读者不受影响（`UPDATE ... WHERE status='pending'` 保证一次性转换）。
- 阅读句柄在 `OpenReview` 时登记、在 `Close` 时注销；句柄打开期间即使审批
  结束、文档继续发布，绑定的正文也不会被回收，读取不会混入其他版本的字节。
- 每次状态变化（发布 / 审批结束 / 句柄关闭）后，在持有互斥锁的同一事务内
  重算保留集合，失去最后保留理由的版本正文文件立即从磁盘删除。
