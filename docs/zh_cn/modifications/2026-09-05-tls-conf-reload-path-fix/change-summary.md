# tls_conf 热加载死锁修复摘要

## 1. 背景

线上环境（[rainway-ai-gateway/conf-agent#19](https://github.com/rainway-ai-gateway/conf-agent/issues/19)）出现：`tls_conf` 是普通目录而不是符号链接，本应指向最新版本目录 `tls_conf_20260904205703`；同时版本目录从 8/1 堆积到 9/4 共 14 个无人清理，目录 mtime 停留在 7月16日（部署日）。

根因分析（详见 [design-changes.md](design-changes.md)）：

- conf-agent 的 reload 流程为 `Probe → StoreFile2TmpDir → TriggerBFEReload → UpdateDefaultConfDir`（`conf_reload/reloader.go`），前一步失败即中断。**BFE reload 失败会导致符号链接切换永远不执行**，且版本清理也只在切换成功后运行，因此形成"一次失败、永久卡死"的状态。
- 对 tls_conf 而言死锁是结构性的：BFE 的 `TLSConfReload` 在收到 `?path=tls_conf_<version>` 时，只把 `server_cert_conf.data` 和 `tls_rule_conf.data` 定位到版本目录，而 **client CA / CRL 仍从启动时解析的 `<confRoot>/tls_conf/client_ca|crl` 读取**（`bfe/bfe_server/bfe_confdata_load.go`）。新配置引用的 CA/CRL 文件只存在于版本目录中，旧目录没有，reload 必然失败；而切换又被 reload 阻塞，新文件永远进不了 `tls_conf/` 目录，每次 reload 以同样原因失败。
- 版本目录持续堆积说明 `Probe`/`StoreFile2TmpDir` 一直成功、重试一直在发生（本地版本号从当前符号链接目标读取，失败后仍是旧版本，conf server 会继续下发新配置），与线上现象吻合。

影响：BFE 实际使用的 TLS 配置是 7月16日的旧内容，期间的 TLS 配置变更（证书、规则）全部未生效，且 reload 持续报错。

## 2. 目标

- 修复 BFE `TLSConfReload` 的路径处理，使 client CA / CRL 目录随 `path` 参数定位到版本目录，打破死锁。
- 保持 conf-agent "切换在 reload 成功之后" 的既有语义不变（当前激活目录始终是 BFE 校验通过的版本）。
- 提供线上环境的恢复操作步骤。
- 增强持久失败状态的可观测性，避免再次长期无人发现。

## 3. 范围

| 范围 | 说明 |
|------|------|
| 涉及仓库 | `bfe`（主修复）、`conf-agent`（可观测性增强 + 部署建议） |
| 涉及模块 | `bfe/bfe_server`（`bfe_confdata_load.go`）、`conf-agent/conf_reload` |
| 变更类型 | 缺陷修复 |
| 接口契约 | BFE monitor 接口不变（`/reload/tls_conf` 语义增强，向后兼容） |
| 数据迁移 | 无 |

## 4. 关键决策

| 决策 | 说明 |
|------|------|
| 主修复在 BFE 侧 | 死锁根源是 BFE 忽略 `path` 参数读取附属文件；conf-agent 的流程顺序本身是合理的（防止未通过校验的配置成为激活目录），不改。 |
| client CA/CRL 与 cert/tls_rule 同样处理 | `TLSConfReload` 中当 `path` 非空时，用现有 `joinPath` 把 `ClientCABaseDir`、`ClientCRLBaseDir` 重定位到版本目录下（版本目录经 CopyFiles 已包含 `client_ca`、`client_crl` 副本）。 |
| 仅重定向 tls_conf 下的默认目录 | 若用户在 bfe.conf 中自定义了绝对路径的 CA/CRL 目录，保持从原位置读取，不做重定向，避免破坏自定义部署。 |
| conf-agent 只加日志、不改流程 | 连续 reload 失败计数，每累计 10 次输出一次包含版本号和最后错误的高可见度 ERROR，避免每轮刷日志又保证状态可见。 |
| 部署顺序 | 先升级 BFE（含本修复），再按恢复步骤处理存量机器；conf-agent 建议同时升级到 v0.0.6+ 以获得目录备份与过期清理能力。 |

## 5. 改动点

| 仓库 | 文件 | 修改内容 |
|------|------|----------|
| bfe | `bfe_server/bfe_confdata_load.go` | `TLSConfReload` 中 `path` 非空时，将 `ClientCABaseDir`、`ClientCRLBaseDir` 用 `joinPath` 重定位到版本目录；仅当配置值位于 `<confRoot>/tls_conf` 下时才重定向。 |
| bfe | `bfe_server/bfe_confdata_load_test.go`（新增或扩展） | 覆盖：带 path 时 CA/CRL 从版本目录读取；自定义绝对路径时不重定向；不带 path 时行为不变。 |
| conf-agent | `conf_reload/reloader.go` | 增加连续 `TriggerBFEReload` 失败计数与降级日志（每 10 次汇总输出一次，含 version 与最后错误）。 |
| conf-agent | `conf_reload/reloader_test.go`（新增） | 覆盖连续失败计数与日志触发逻辑。 |

详细设计见 [design-changes.md](design-changes.md)。

## 6. 影响面

| 项目 | 说明 |
|------|------|
| BFE 热加载 | `/reload/tls_conf` 带 `path` 时 client CA/CRL 改从版本目录读取，与 cert/tls_rule 行为一致；不带 `path` 行为完全不变。 |
| 存量死锁环境 | 升级 BFE 后，按第 7 节步骤恢复一次即可自愈，之后新配置可正常热加载。 |
| 回滚能力 | 不改变版本目录结构、符号链接机制和 `VersionKeepCount` 语义，回滚方式与现状一致。 |
| 兼容性 | 新 BFE + 旧 conf-agent 可正常工作；旧 BFE + 新 conf-agent 在"新增 client CA"场景仍可能死锁，因此 BFE 必须先升级。 |

## 7. 线上恢复步骤（存量机器）

前提：BFE 已升级含本修复的版本，conf-agent 已升级 v0.0.6+。

1. 确认失败原因已消除：查看 conf-agent 日志中 `reload_bfe ... reload fail` 是否消失，或 BFE log 中 `TLSConfReload` 报错阶段（`ServerCertParse` / `ClientCALoad` / `CheckTlsConf`）。
2. 停掉 conf-agent 进程，避免与人工操作竞争。
3. 备份并替换 `tls_conf`：
   ```bash
   cd /home/work/bfe/conf
   mv tls_conf tls_conf.manual-backup.$(date +%Y%m%d%H%M%S)
   ln -s tls_conf_20260904205703 tls_conf   # 指向最新版本目录
   ```
4. 启动 conf-agent。其下一轮 reload 会从当前符号链接读取本地版本号，conf server 如有更新则正常走完整流程；`UpdateDefaultConfDir` 遇到已是符号链接的 `tls_conf` 会正常切换。
5. 观察 `UpdateDefaultConfDir succ` 日志，确认切换与过期目录清理恢复工作。

> 注意：若 BFE 未升级就执行上述步骤，当次可以恢复（符号链接指向的版本目录里文件齐全），但下一次"新增 client CA 文件"的配置变更会再次死锁，因此 BFE 升级是必选项。
