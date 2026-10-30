# 容器 Restart 与 PVC 挂卸执行证据

当前进度仅见 [status](../../status.md#container-restart-pvc-20261028)，不在此建立第二份台账。固定需求及 A01–A41 索引见 [v2.0](../../../plans/container-instance-restart-pvc-20261028.md)。原目标授权文件已读取，不复制其中凭据。

来源核验：2026-10-28，方案 SHA-256 `b6dfb17aef74f3d99cc969ecd2ef51b19c77b0a984f82fbe6afd28aa94d41f7a` PASS。Resource SSH/22 首次 fetch 因连接关闭 FAIL，随后使用单命令 SSH/443 成功，未更改 remote 或全局配置；main 为 `55092dbf05c284875555d422e75eeb5691cb9cfd`。Gov SSH fetch dev PASS：`36707b8d03442b717b6ee9f8a298534b6350d89d`。

原 Resource checkout 为 `codex/image-mvp-20260930` / `d32c4f438b43b8cb7534486c73e116e4d26c890a`，有既有文档修改及未跟踪资料，未复制或覆盖。Gov 主 checkout `main` / `323eb749b8a3d607b26687797b372919052895c7` clean。实施使用各自隔离工作树。

环境盘点由只读记录承接；CODE/PG/BOUNDARY/LIVE 的命令和结果在执行后追加同目录文件，所有未执行项不认作通过。

原始收据索引：[停止态卸载、卸空启动、重挂读回及版本/原账证明](cp02-stopped-continuation-03.safe.json)（含逐文件SHA）；[正式终端SG边界](terminal-exec-diag-20261029-01.safe.json)。当前结论仍只维护在上方status入口。

本轮新增原始入口：[Source21构建与发布制品](source21-build-publish.safe.json)、[当前259554静态来源核对](resource-259554-static-buildinfo.safe.json)、[A动作权限/候选证据](a05-auth-return-02/SHA256-manifest.safe.json)、[B跨租户/套餐证据](a05-cross-plan-return-01/SHA256-manifest.safe.json)、[用户授权占卷释放证明](cpu-volume-release-proof-01.safe.json)。结果和当前恢复动作只在status维护。

后续原始入口：[Restart18失败及PG安全停止](cpu-restart18-final-pg.safe.json)、[真实停止/持久盘身份](cpu-restart18-final-stopped.safe.json)、[Source22合main完整门禁](source22-main792-verify.safe.json)、[GPU原CREATE](gpu-create01.safe.json)。

GPU与发布后续原始入口：[GPU安全回件](gpu-workspace-return-01/return-manifest.safe.json)、[Source23构建](source23-build-publish.safe.json)、[维护前安全状态](source23-gov450-maintenance-preflight.safe.json)、[Gov当前dev祖先](gov-current-dev-ancestry.safe.json)。

- [Source23 Resource实际部署/正常鉴权查询/开闸原始回执](source23-resource-release-01/index.safe.json)；[Restart20严格RV写冲突窄诊断](cpu-source23-restart20-diag-01.safe.json)。部署通过与CPU重启尚未通过分别记录。

- [Source24 RV写确认修复：RED/GREEN、原make环境失败、组成续验及窄PG原件](source24-rv-cas-01/index.safe.json)。已PASS组成不重复执行；真实重启另行记录。
- [Gov main450快进及两部署实际发布](gov-main-release-154003.safe.json)。本次按用户要求未运行测试/门禁。
- [Source27 最后实际故障恢复、正常清理及正式镜像恢复](source27-live-03/README.md)。失败工具尝试及证据局限保留，当前状态仍只见本页顶部 status 入口。
