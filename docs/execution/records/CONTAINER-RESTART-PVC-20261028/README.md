# 容器 Restart 与 PVC 挂卸执行证据

当前进度仅见 [status](../../status.md#container-restart-pvc-20261028)，不在此建立第二份台账。固定需求及 A01–A41 索引见 [v2.0](../../../plans/container-instance-restart-pvc-20261028.md)。原目标授权文件已读取，不复制其中凭据。

来源核验：2026-10-28，方案 SHA-256 `b6dfb17aef74f3d99cc969ecd2ef51b19c77b0a984f82fbe6afd28aa94d41f7a` PASS。Resource SSH/22 首次 fetch 因连接关闭 FAIL，随后使用单命令 SSH/443 成功，未更改 remote 或全局配置；main 为 `55092dbf05c284875555d422e75eeb5691cb9cfd`。Gov SSH fetch dev PASS：`36707b8d03442b717b6ee9f8a298534b6350d89d`。

原 Resource checkout 为 `codex/image-mvp-20260930` / `d32c4f438b43b8cb7534486c73e116e4d26c890a`，有既有文档修改及未跟踪资料，未复制或覆盖。Gov 主 checkout `main` / `323eb749b8a3d607b26687797b372919052895c7` clean。实施使用各自隔离工作树。

环境盘点由只读记录承接；CODE/PG/BOUNDARY/LIVE 的命令和结果在执行后追加同目录文件，所有未执行项不认作通过。
