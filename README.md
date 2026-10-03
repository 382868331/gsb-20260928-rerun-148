# 区块链轻客户端研究库（Go 教学版）

这是一个完全离线、可替换共识规则的 Go 轻客户端第一版。仓库不连接真实节点，所有区块头和交易均由人工构造或测试夹具提供。

## 明确的教学共识

`TeachingProofOfWork` 固定使用以下规则：

- 创世块高度为 `0`，父哈希为全零。
- 每个后续块的高度必须严格等于父块高度加一。
- `ParentRoot` 必须等于完整父区块头哈希，不能只保存前一个摘要。
- 时间戳必须严格递增。
- 所有块使用固定难度 `8`，区块头哈希至少有 8 个前导零位。
- 链选择使用累计工作量；累计工作量相同则比较高度，再相同选择较小区块头哈希。

这些规则通过 `Consensus` 接口暴露。后续可以用 PoS、检查点或其他教学规则替换，而 Merkle 证明、存储和审计结果逻辑不需要重写。

## 区块头与交易证明

区块头提交 `TransactionsRoot`。证明结构包含：

- 交易原文；
- 被引用区块的完整区块头哈希；
- 交易索引和交易总数；
- 固定 64 层二进制 Merkle 路径。

叶子节点和内部节点使用不同域前缀，避免叶子与中间节点互相伪造。空位置使用全零哈希。轻节点不仅验证 Merkle 路径，还要求：

1. 证明命名的区块头已经从信任起点连续验证；
2. 该区块位于当前认可的规范链；
3. 确认数达到调用者要求；
4. 证明中的交易根与当前认可区块头一致。

因此，旧分支上的证明即使本身路径正确，也会在分叉切换后返回 `ErrStaleFork`；伪造路径或把证明绑定到其他区块会返回 `ErrProofNotBound`。

## 联合证明与确认策略

`BatchInclusionProof` 把同一区块中多笔交易的证明合并：块哈希和交易总数只携带一次，每笔交易仍自带原文、索引和 Merkle 路径。`VerifyBatchInclusion` / `VerifyInclusionBatch` 逐笔独立验证并返回逐笔结果；任何一笔缺失或无效都会使整体返回 `ErrBatchIncomplete`，缺项不会被当作全部通过。重复出现的相同交易各自绑定自己的索引位置。

确认数要求来自两处：调用方显式传入的值优先；传 `0` 时使用链上可更新的 `ConfirmationPolicy`（`SetConfirmationPolicy`，至少为 1，默认 `DefaultConfirmations`）。每次验证结果记录当时生效的 `RequiredConfirmations` 供审计。

历史结果只是历史证据。`RecheckResult` 按当前规范链和当前策略重新检查一笔历史结果：链重组后旧块离开规范链会返回 `ErrStaleFork`，确认数不足返回 `ErrInsufficientConfirmations`，结果文件里自报的区块若不在已验证头链中则返回 `ErrProofNotBound`。重开后规范链判断只从信任起点和经验证的区块头重建，不从结果文件或旧结论扩展信任。

## 分叉切换

链状态保存所有已连接的有效分叉头，但只把符合链选择规则的一条链标记为 canonical。切换到更长累计工作量分支后：

- 旧链头仍可作为审计数据存在；
- 旧链中的交易包含证明不再通过；
- 新链中的证明必须绑定新链区块哈希和交易根；
- 确认数按当前规范链头高度重新计算。

## 存储边界

`FileStore` 将三类数据写入不同文件：

- `trust-root.json`：人工信任起点，只能写入一次；
- `header-chain.json`：已验证并连接到信任起点的区块头；
- `verification-results.json`：验证审计结果。

结果文件不是新的信任来源。重开后必须先读取并验证不可变信任起点，再按父子关系和共识规则重建头部链，最后重新运行证明检查。历史结果只用于审计，不能让新头部或新信任起点被接受。

## 证明收集任务

`CollectionTask` 把一次审计声明为一个固定需求集合：创建时给出全部 `EvidenceRequirement`（每条绑定一个交易哈希）和任务级确认数，之后需求集合不再增删。材料可以分任意多批、按任意顺序经 `SubmitBatch` 送达：

- 每批记录来源标签和当时的规范链头，已收证据永久保留其批次来源；
- 同一需求的重复材料记为 `duplicate`，不重复计入；
- 与已收证据矛盾的后到材料记为 `conflict` 并附在原证据上，先到证据不被覆盖；
- 需求之外的材料记为 `unrequested`，只留审计痕迹，不影响完成度。

`Missing()` 始终返回精确的缺口；只有全部需求都持有当前有效的已接受证据时 `Complete()` 才为真，部分有效不替代整体完整。每次批次处理和重检都会追加一个 `TaskDecision` 版本，记录当时的规范链头与确认要求——旧版本结论保留原依据，不被改写。

链重组或确认约定变化后调用 `Revalidate`：仍成立的证据保持接受，失效的证据标记为 `needs-recheck` 并记录原因，返回需要重新核对的需求列表，同时形成新的决策版本。任务可通过 `FileStore.SaveTask` / `LoadTask` 保存与重开，重开时校验任务仍绑定当前不可变信任起点。`Abandon` 放弃任务后不再接受新材料，但 `Explain()` 仍能逐条说明已检查的证据、来源批次、冲突记录和最终未完成原因。

## 快速开始

```go
genesis, anchor, err := lightclient.MineGenesis(1000, [][]byte{[]byte("genesis-tx")})
anchor.CreatedAt = time.Now()
chain, err := lightclient.NewChain(anchor, lightclient.TeachingProofOfWork{})

block, err := lightclient.MineChild(genesis.Header, 1001, [][]byte{[]byte("tx-1")})
err = chain.AddHeader(block.Header)

proof := lightclient.NewInclusionProofForHeader(block.Header, block.Transactions, 0)
result, err := chain.VerifyInclusion(proof, 1)
```

人工构造无效块时，可直接构造 `BlockHeader`；`MineGenesis` 和 `MineChild` 只用于快速生成合法测试数据。

## 已覆盖的反例

运行 `go test ./...` 可验证：

- 篡改 Merkle 路径中的兄弟哈希；
- 把证明绑定到交易根不同的其他区块；
- 缺少父块时直接提交高块；
- 旧分支证明先通过、分叉切换后被撤回；
- 联合证明中篡改或缺失单笔不会拖垮或冒充其他笔；
- 相同交易重复出现时各自绑定索引，越界索引被拒绝；
- 确认数边界（恰好达到通过、少一拒绝）与可更新确认策略；
- 重组后历史通过结果重检失效，但历史记录仍作为证据保留；
- 重开后不能替换信任起点；
- 被篡改或缺少祖先的持久化头不能加载。
