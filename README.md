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
- 重开后不能替换信任起点；
- 被篡改或缺少祖先的持久化头不能加载。

## 多笔交易的联合证明

`BuildJointProofPacket` 按需为多个 `ProofRequest` 取得证明。每个请求显式携带交易原文、区块哈希、交易索引和信任起点哈希：

- 同一区块的 Merkle 层级只计算一次，各位置共享兄弟哈希材料；
- 每个条目仍绑定自己的位置、区块和信任起点，重复交易不能占用另一个索引的证明；
- 找不到区块、索引越界或信任起点不一致的请求保留为 rejected 缺项，批量结果不会把缺项算作成功；
- 数据包中的 `ClaimedTransactionsRoot` 只是元数据，验证时只使用本地连续验证过的区块头中的交易根。

`Chain.VerifyJointProof` 在当前规范链上逐项重新执行 Merkle、绑定、规范链和确认数检查。`BatchVerificationResult.AllAccepted` 只有在全部条目通过时才为真；错误使用 `errors.Join` 汇总，可通过 `errors.Is` 检查 `ErrProofMissing`、`ErrStaleFork` 等具体原因。

## 可更新的确认策略

确认数要求通过 `ConfirmationPolicy` 接口提供：

- `ConstantConfirmations` 提供固定确认数；
- `HeightConfirmations` 支持按区块高度更新要求；
- `ConfirmationPolicyFunc` 适配任意函数。

调用 `Chain.SetConfirmationPolicy` 不会改变信任起点或头部链，只影响之后的验证。`VerifyInclusion`/`VerifyJointProof` 传入非零确认数时以调用者显式值为准；传 `0` 时查询当前策略。结果记录同时保存观察到的确认数和本次要求的确认数，便于审计边界。

## 重组复检与历史证据

`FileStore.SaveJointProof` 把数据包保存到 `joint-proofs.json`，保存和加载都会重算数据包身份，且必须绑定不可变信任起点。`RecheckJointProof` 重开后：

1. 先从信任起点和父子关系重建头部链与规范链；
2. 再对保存的每一项在当前规范链上重新验证；
3. 旧的 accepted 审计记录保留不动，新的 rejected 记录追加到结果文件。

历史结论、数据包自报根和自报身份都不是新的信任来源；切换到人工构造的更重分叉后，旧分支条目必须返回 `ErrStaleFork`。
