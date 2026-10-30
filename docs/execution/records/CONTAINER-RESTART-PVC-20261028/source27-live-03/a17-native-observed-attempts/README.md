# A17：未捕获 observedGeneration 负态的两次 admission 尝试

本目录只收回两份既有远端 state 的安全投影，没有新增业务请求、集群读取或测试。每份文件的 `_evidence_source` 记录原路径、原始 SHA256、字节数及脱敏变更；本次未发现需要替换的敏感键值。

| 既有尝试 | Stop intentVersion | 原 Stop operation ID | 正常 Stop 终态 | 注入窗口结果 |
|---|---:|---|---|---|
| [原 native UID 策略尝试](native-uid-state.safe.json) | `26` | `f322c4b1-c852-48e2-8868-4c21d0adc71d` | `completed/done` | `NOT_VERIFIED_NEGATIVE_WINDOW_ENDED` |
| [精确名称策略尝试](exact-name-state.safe.json) | `28` | `5fb0e006-a979-4dbd-a9d4-01f7c61a279f` | `completed/done` | `NOT_VERIFIED_NEGATIVE_WINDOW_ENDED` |

两次 `stop-observe` 都未取得所要求的注入负态窗口，保持 **NOT_VERIFIED**；不能据 policy/binding 创建成功声称注入实际生效，也不能据正常 Stop 完成声称产品提前完成。原失败字段、全部已保存观察及各步结果均保留。

两份 state 都记录 `release_result=OWN_BINDING_AND_POLICY_ABSENT`，随后由原 Stop 正常完成；`resume_result=NOT_VERIFIED_NEGATIVE_NORMAL_STOP_CLEANUP_ONLY`，`additional_stop_intents=0`。正常收尾与自己的 policy/binding 已移除，分别是清理事实，不改变注入窗口结果。

A17 observedGeneration 子项另有 [Root 的真实 watch 时间顺序证明](../a17-natural/observed-lag-result.safe.json)。该证明引用已有正常 Stop 的真实 lag 事件及后续正式查询，不将本目录两次 admission 尝试改标为成功注入。
