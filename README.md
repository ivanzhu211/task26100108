# VaultGate 文档修订与审批保留组件

Go 库，元数据存 SQLite（`metadata.sqlite`），正文存本地目录（`bodies/<docID>/<version>.body`）。

## API

- `Open(dir) / Close()`
- `Publish(docID, src io.Reader) (version, error)` — 流式写入，版本号从 1 连续递增，修订不可变
- `RequestApproval(docID) (approvalID, version, error)` — 创建 pending 审批，绑定当前已发布版本；同版本允许多个独立审批
- `OpenReview(approvalID) (version, io.ReadCloser, error)` — 仅 pending 审批可开；句柄 Close 前持续有效
- `Respond(approvalID, decision) (version, final, error)` — pending 一次性转为 `approved` / `rejected` / `revision_requested`，决定不漂移

## 运行测试

```sh
go test ./...
```

本机沙箱网络受限时，使用仓库内的本地模块缓存：

```sh
source goenv.sh && go test -race -count=1 ./...
```

## 保留责任交接

每个文档的正文保留集合 = 当前版 ∪ 最近 10 个历史版 ∪ 所有 pending 审批绑定版本 ∪ 所有未关闭阅读句柄绑定版本。

保留理由是**按主体独立计数**的：

- 审批结束（`Respond`）只移除该审批自己的保留理由；同版本的其他 pending 审批与未关闭句柄继续保留正文。
- 阅读句柄 `Close` 只移除该句柄的理由。
- 每次状态变化（发布 / 审批结束 / 句柄关闭）后重算保留集合并回收：版本失去最后一个保留理由时，删除其正文文件与元数据行，释放磁盘空间。

因此旧版正文在“两个审批 + 一个读者”场景下，会沿 审批1结束 → 审批2结束 → 读者Close 逐步交接，直到最后理由消失才真正删除；单次阅读只打开绑定版本的文件句柄，不会混入其他版本字节。
