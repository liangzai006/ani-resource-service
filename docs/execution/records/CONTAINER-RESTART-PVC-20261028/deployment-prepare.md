# A35 旧 Storage 切换取证

2026-10-28 09:56–09:59 UTC，只读通过指定 `ssh kubercloud@100.64.128.12`。原始投影及对象 UID 见 [legacy-storage-cutover-safe.json](legacy-storage-cutover-safe.json)。未改 Go、SQL、部署、权限或集群对象。

**没有取得旧 Storage 在途/未知删除已收敛的证明，不作历史请求数为零的声明。** 三台实际 apiserver 均无 audit 参数/挂载；node1 常见 audit 路径不存在。Resource 当前可取日志仅覆盖约 17 分钟，DeleteVolume 匹配 0，无 previous 容器日志；Pod 在 node3，轮转文件不在入口 node1。Fluent Bit 指向认证/TLS OpenSearch，但本次未建立其认证读会话，中央历史日志为 NOT_VERIFIED，并非不存在。Instance active-op/work-lease 为零不能补足 Storage 证明。

旧部署镜像 digest `sha256:5a211f66a6eff0f7fb0df0e0bacec13bee0764f8f9d2759c19ba9e97c2cb53e7` 已由根代理从 Harbor 拉至 Fedora task localbase，`docker image inspect Config.Labels` 的 revision 为 `f6d7f7b8e90f5def80e937cf2d40780da96f0d50`，source 为本 Resource 仓库，release.parents 已录入 JSON。本代理只读核对该精确 Git 对象 `internal/data/storage/k8s.go:153–155`，确认 DELETE 携带原 UID precondition。镜像 digest→OCI revision→精确源码链已补齐；它不能补出历史请求身份或收敛结果。固定基线中的同步调用无持久请求/专用 begin/end，且 Storage 请求日志正文被脱敏。

当前两个可信 tenant Namespace 共四个 PVC，可挂卸既有候选 **0**：两个 Filesystem workspace 有实例专属标签，必须排除 borrowed；两个 CDI DataVolume 磁盘均为 Block，必须以 Filesystem 规则排除。四个对象均无 Storage ownership 标签，未处于 deleting；其名称、UID、ownerUID 和标签已冻结于 JSON。另一租户 Namespace 当前 PVC 为 0。现有四个名称继续保持资格排除，不能以本次盘点授权 Attach 或 DELETE。

CP-01 可在 borrowed PVC 写入口关闭时继续。CP-02 的最小安全前进范围是：排空 Storage 入口并确认所有旧 writer/不兼容进程退出后，使用正常授权 Storage API 创建**唯一新名称、新 UID、Filesystem** 测试 PVC，复核租户/owner 并由正式 resolver 冻结 UID；初次挂卸仅使用该新对象。旧镜像 UID precondition 已由上述精确来源链确认，还须确保旧 writer 无机会重新选择新对象。旧 UID 条件请求不能命中新 UID；进程退出本身仍不构成旧请求历史收敛证明。

不伪造 legacy delete ticket、write_started 或隔离记录，不给恢复 worker 新的删除授权；若将来接入切换前的旧名称，须先取得真实请求/目标 UID/外部结果证据再决定资格。A36 保持用户明确的 DEFERRED_USER_SCOPE，本次未重查 admission/RBAC。此结论是部署准备边界，不是 A35/LIVE 验收 PASS。
