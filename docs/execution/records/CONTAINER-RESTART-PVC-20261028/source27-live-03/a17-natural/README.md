# A17：正常 Stop 的两个 STS 瞬态窗口

[sts02-state.safe.json](sts02-state.safe.json) 是远端 `a17-sts-remaining-live-02/state.json` 的既有脱敏投影。`_evidence_source` 保留原始路径、SHA256 和敏感值替换记录；本次仅复制文件，没有重新请求业务或读取集群。

真实对象是任务 CPU `9b2e5371-5be3-4cac-8766-55147d8041fb`。此观察复用既有 A14 cleanup-stop 作为唯一业务 writer，没有新增 Start/Stop 意图：

| 操作 | intentVersion | operation ID | 实际结果 |
|---|---:|---|---|
| 原普通 Stop | 16 | `7316a712-20a9-4271-b432-2c68a34cf906` | `completed/done`，三态 Stopped、active 为空、Pending/Fence 空、原 workspace UID 保持；applied version 14 |

两个 fresh 子项均保持 **NOT_VERIFIED**：

- Notebook 已 stopped、同 STS `spec.replicas != 0`：`NOT_VERIFIED_WINDOW_NOT_CAPTURED`。
- Notebook 已 stopped、同 STS `status.observedGeneration < metadata.generation`：`NOT_VERIFIED_WINDOW_NOT_CAPTURED`。

watch 中出现的候选事件不足以替代同一操作仍未终态、实例尚未 observed stopped 的完整 fresh bookend，所以没有将候选事件改标为 PASS。正常 Stop 收尾完成也不代表捕获了这两个负态窗口。原始 watch、候选样本、fresh bookends 和实际终态均保留；证据时实际借卷配置保留，不按空挂载重述。

本份 state 未记录 step failure；原 cleanup 客户端 exit 0，logout `CONFIRMED`。本目录未改变其他 A17 已有证据的结果。

## 按原 A17 断言补充的时间顺序证明

[observed-lag-result.safe.json](observed-lag-result.safe.json) 是 Root 按原 A17 断言整理的补充：真实 STS watch 对象记录 `generation=19/observedGeneration=18`，紧接着的正式查询仍返回同一 Stop `7316a712-20a9-4271-b432-2c68a34cf906` 为 `accepted/stopping`、实例 `observed=pending`。结合已核实的操作终态单调性，排除该 lag 期间已经完成；Root 据此记录 `PASS_LIVE_OBSERVED_GENERATION_LAG_ORIGINAL_STOP_NONTERMINAL`。

这一方法不是与短暂 lag 同时发生的 fresh GET，不宣称隔离了某个内部 guard 分支，也不宣称后续 admission 注入策略实际生效。上文及原 state 中的两个 **fresh-window NOT_VERIFIED** 历史结果保持不变；补充结论使用原 A17 断言，而非额外加入的同时 GET、两样本或独立 PodGone 阈值。

另见 [两次未验证的 admission 窗口与正常清理](../a17-native-observed-attempts/README.md)，原始失败记录没有被正常 Stop 收尾覆盖。
