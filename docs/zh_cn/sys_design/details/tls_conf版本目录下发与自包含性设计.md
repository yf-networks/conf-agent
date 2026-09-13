# tls_conf 版本目录下发与自包含性设计

## 1. 背景与定位

`tls_conf`（TLS 配置）是 conf-agent 管理的配置单元中最特殊的一个：BFE 的热加载入口 `/reload/tls_conf?path=...` 以**整个版本目录**为配置单元（tls_conf 的 reloader 不配置 `ReloadFile`），而不是单个配置文件。这要求每个版本目录都是一份**自包含的完整 TLS 配置**：BFE 只凭 `path` 指向的目录就能完成全部配置输入的加载校验。

本文档描述：

- tls_conf 版本目录的内容构成与来源；
- CopyFiles 复制语义（文件项与目录项的区别）；
- 版本目录自包含性与 BFE `path` 重定向之间的契约；
- 热加载失败的死锁风险（[conf-agent#19](https://github.com/rainway-ai-gateway/conf-agent/issues/19)）与可观测性设计。

## 2. tls_conf 与其他配置单元的差异

| 维度 | 普通模块（如 mod_ai_token_auth） | tls_conf |
|------|----------------------------------|----------|
| BFE reload 目标 | 版本目录下的单个配置文件（`path=...&...` 带 `ReloadFile`） | 整个版本目录 |
| BFE 读取的输入 | 一份规则文件 | 四类输入：`server_cert_conf.data`、`tls_rule_conf.data`、client CA 目录、client CRL 目录 |
| 附加文件 | 视模块而定 | 证书/私钥（ExtraFileTask 下载） |
| 目录自包含要求 | 低（BFE 只读指定文件） | **高**（BFE 按 `path` 解析全部输入） |

## 3. 版本目录内容构成

以版本 `20260905193752` 为例，conf-agent 生成的 `tls_conf_20260905193752/` 包含：

```text
tls_conf_20260905193752/
├── .conf-agent-version        # 版本标识（清理判据）
├── server_cert_conf.data      # NormalFileTask：InnerAPI /configs/protocol/server_cert_conf
├── tls_rule_conf.data         # CopyFiles：运维维护的 TLS 规则（tls_rule 不由 InnerAPI 下发）
├── session_ticket_key.data    # CopyFiles：会话票据密钥
├── client_ca/                 # CopyFiles：客户端 CA 目录
│   ├── example_ca.crt
│   └── sc27_ca.crt
├── client_crl/                # CopyFiles：客户端 CRL 目录（可能为空）
└── bfe/                       # ExtraFileTask 下载的服务器证书/私钥
    ├── example.org.crt
    ├── example.org.key
    ├── example.org-v2.crt
    └── example.org-v2.key
```

来源分三类：

1. **NormalFileTask**：从 InnerAPI 拉取的 `server_cert_conf.data`（引用条目中内嵌 `tls_conf_<version>/bfe/<name>.crt` 形式的证书路径，见第 5 节）；
2. **ExtraFileTask**：解析 `server_cert_conf.data` 中 `ServerCertFile` / `ServerKeyFile` 路径（`ExtraFileJSONPaths`），从 extra_files 接口下载证书/私钥内容写入版本目录；
3. **CopyFiles**：从当前激活目录复制的运维维护文件（`tls_rule_conf.data`、`session_ticket_key.data`、`client_ca/`、`client_crl/`）。

## 4. CopyFiles 复制语义

`StoreFile2TmpDir` 对 `CopyFiles` 中每个条目的处理（`conf_reload/file_store/file_store.go`）：

| 条目类型 | 目标位置 | 说明 |
|----------|----------|------|
| 文件 | `tmpDir/<文件名>` | 与旧行为一致 |
| 目录 | `tmpDir/<条目名>/` | **保留条目名**，目录内容递归复制，空目录也会创建 |

两点关键约束：

- `xfile.FileCopyRecursive` 对目录的语义是"把源目录的*内容*复制进目标"，因此目录项必须显式指定目标为 `tmpDir/<条目名>`，否则 `client_ca` 的内容会被拍平到版本目录根，目录结构丢失；
- `copyDir` 在复制任何条目之前先创建目标目录，空目录（如未配置吊销列表时的 `client_crl/`）也能完整复制。

> 注意：`file_store.renameDir` 的 EXDEV 回退（跨设备 rename 失败时 copy + delete）依赖"目录内容复制进已存在的目标"的旧语义，调用点传入的是最终目录路径，不受上述目录项处理影响。

## 5. 与 BFE 的契约：path 重定向与自包含性

### 5.1 BFE 侧的加载行为

BFE `/reload/tls_conf?path=<版本目录>` 时，会把四类配置输入统一按版本目录解析（`bfe/bfe_server/bfe_confdata_load.go`）：

```text
server_cert_conf.data  ← <path>/server_cert_conf.data
tls_rule_conf.data     ← <path>/tls_rule_conf.data
ClientCABaseDir        ← <path>/client_ca   （位于 <confRoot>/tls_conf 下时重定向）
ClientCRLBaseDir       ← <path>/client_crl  （位于 <confRoot>/tls_conf 下时重定向）
```

`server_cert_conf.data` 中内嵌的证书路径（`tls_conf_<version>/bfe/<name>.crt`）相对于 BFE 配置根解析，恰好指向版本目录内的副本。

### 5.2 自包含性契约

conf-agent 的职责：版本目录必须包含 BFE 全部输入的完整副本（第 3 节）。
BFE 的职责：带 `path` 时只读版本目录，不再回退激活目录。

任何一侧违约都会造成 reload 失败 → conf-agent 不切换符号链接 → 下一轮以同样原因失败的**死循环**（#19 即 BFE 侧违约：client CA/CRL 未随 `path` 重定向）。

### 5.3 运维修改激活目录的下发路径

`tls_rule_conf.data`、`client_ca/` 由运维直接改激活目录。由于 `ConfDir` 是符号链接，CopyFiles 从链接目标读取，运维修改会随下一次版本切换被复制进新版本目录——这是"运维改激活目录 + 控制面改证书"组合下发路径的基础，也要求**运维修改后必须有一次版本变更**（如更新证书）才会生效。

## 6. 热加载失败的可观测性

不改 `reload()` 的步骤顺序（"切换在 reload 成功之后"保证激活目录始终是 BFE 校验通过的版本，是合理的安全语义），仅增强持久失败的可观测性（`conf_reload/reloader.go`）：

- `Reloader` 维护 `consecutiveTriggerFailures` 计数；
- `TriggerBFEReload` 失败时计数 +1；成功或本轮无更新（`without_update`）时清零；
- 计数达到 10 的倍数时额外输出一条汇总 ERROR：`tls_conf hot reload keeps failing (N consecutive), version=<v>, last_err=<err>，符号链接未切换，BFE 仍在使用旧配置`；
- 从失败状态恢复（计数非零时成功）输出一条恢复 INFO。

#19 的线上现象（版本目录堆积、TLS 配置长期未生效）正是"每轮都失败但日志里没有汇总信号"，该计数使这类死锁在 10 个周期内即可被发现。

## 7. 边界情况

| 场景 | 处理 |
|------|------|
| 版本目录缺少 CopyFiles 源（如激活目录无 `client_crl/`） | 记 Info 日志并跳过，不中断本轮 |
| CopyFiles 源是空目录 | 正常复制（`copyDir` 先建目录） |
| BFE reload 返回错误 | 不切换符号链接，计数 +1，按间隔重试 |
| BFE 长时间不升级（旧 BFE 不重定向 client CA/CRL） | 涉及新 client CA 的变更会再次死锁——**存量环境必须先升级 BFE** |
| 版本目录缺少 `.conf-agent-version` | 清理时跳过该目录，不会被误删 |

## 8. 相关文档

- [配置加载与热加载流程](../配置加载与热加载流程.md)
- [配置目录版本管理与清理](../配置目录版本管理与清理.md)
- [conf-agent#19](https://github.com/rainway-ai-gateway/conf-agent/issues/19)
- `conf-agent/docs/zh_cn/modifications/2026-09-05-tls-conf-reload-path-fix/`（本次修复的变更记录与线上恢复手册）
- `bfe/docs/zh_cn/sys_design/tls_conf_reload_path.md`（BFE 侧设计）
