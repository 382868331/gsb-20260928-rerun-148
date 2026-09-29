# 区块链轻客户端研究库（Go 第一版）

这是一个从零实现的教学型区块链轻客户端，不连接真实节点，只接收人工准备的区块头、交易和 Merkle 包含证明。

## 明确的信任模型

信任起点是 `store.TrustAnchor`：人工指定一个可信区块高度、区块哈希，以及可选的完整区块头。信任起点单独保存在 `anchor.json`，重开时必须再次提供完全相同的值；程序不会把当前链头提升为新的信任起点。

持久化数据分为三个文件：

- `anchor.json`：不可变信任起点。
- `chain.json`：区块头、父子边、累计工作量和当前认可链头。
- `results.json`：交易包含证明的验证结果。

重开时，`store.OpenJSON` 会重新验证全部区块头的哈希、高度、父哈希、时间顺序、工作量、累计工作量、分叉集合和当前链头，而不是因为数据曾保存过就信任它。

## 教学共识规则

`consensus.TeachingRules` 固定第一版规则，后续可通过实现 `consensus.Rules` 替换：

- 区块头版本必须为 `0`。
- 高度必须比父块高 `1`。
- `ParentHash` 必须是父块完整头部的 SHA-256 摘要。
- 时间戳必须严格递增。
- 区块头哈希必须满足简单 PoW：首字节为零。
- 每个有效区块累计工作量增加 `1`。
- 链选择首先比较累计工作量；相同工作量时选择头部哈希字典序较小的链头。

只有连续连接到人工信任锚的头部才会被接受。缺少任一祖先时，即使该头部自身满足 PoW，也会以 `missing ancestor` 拒绝。

## 交易包含证明

交易使用域分隔哈希，Merkle 树固定为深度至少为 1 的二叉树，不足位置填充空叶子。`types.BuildInclusionProof` 生成叶子索引和同级路径，`InclusionProof.Root` 按索引决定左右组合。

`VerifyTransactionInclusion` 会先确认完整交易的哈希等于证明中的 `TxHash`，再调用 `VerifyInclusion`。`VerifyInclusion` 不验证孤立证明，而是要求：

- 证明绑定的区块哈希已经由完整头部链连接到信任锚。
- 该区块是当前认可链头，或是当前认可链头的祖先。
- Merkle 路径重算出的根必须等于区块头中的 `TxRoot`。

因此，伪造路径、越界路径或绑定旧分支的证明都会失败。
提交与证明中交易哈希不一致的交易也会失败。

## 分叉切换与撤回

收到更高优先级链头时，轻客户端切换当前认可头。若旧确认所在区块不在新认可链上，对应结果会从 `valid` 标记为 `revoked-by-reorg`，该历史确认立即失效。只有当包含该证明的区块后来重新成为认可链的一部分，并重新调用验证成功后，结果才会再次写成 `valid`。

## 使用

核心流程：

```go
genesis := consensus.MineGenesis(1)
anchor := store.TrustAnchor{
    Height: genesis.Height,
    Hash:   genesis.Hash(),
    Header: &genesis,
}
data, _ := store.InitializeJSON("/tmp/teaching-lc", anchor)
lc := client.New(consensus.NewTeachingRules(), data)

header := consensus.MineNext(genesis, types.MerkleRootFromHashes(nil), 2)
_, err := lc.AddHeader(header)
```

重开：

```go
data, err := store.OpenJSON("/tmp/teaching-lc", anchor, consensus.NewTeachingRules())
lc := client.New(consensus.NewTeachingRules(), data)
```

## 测试

```sh
go test ./...
```

测试覆盖：有效头部和证明、伪造 Merkle 路径、旧分支证明、缺失祖先、非法 PoW、分叉撤回、重开保持边界，以及篡改已保存链头后拒绝打开。
