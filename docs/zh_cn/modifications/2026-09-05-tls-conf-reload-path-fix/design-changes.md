# tls_conf 热加载死锁修复设计细节

## 1. 死锁机制复盘

### 1.1 conf-agent 侧流程

`conf_reload/reloader.go` 的 `reload()` 顺序：

```
Probe → StoreFile2TmpDir → TriggerBFEReload → UpdateDefaultConfDir
  失败即 return，后续步骤不执行
```

- `StoreFile2TmpDir` 把新配置（含 ExtraFileTasks 下载的证书/私钥、CopyFiles 复制的 `client_ca`、`client_crl` 等）写入版本目录 `tls_conf_<version>`（`conf_reload/file_store/file_store.go:242`）。
- `TriggerBFEReload` 请求 BFE `GET /reload/tls_conf?path=tls_conf_<version>`（`conf_reload/trigger/trigger.go:39-47`）。tls_conf 的 reloader 未配置 `ReloadFile`，`path` 为版本目录。
- `UpdateDefaultConfDir` 切换符号链接并清理过期版本目录（`file_store.go:181`），**只在 trigger 成功后执行**。

版本号的本地来源：prober 从 `ConfDir/<file>`（即当前符号链接目标）读取本地版本号（`conf_reload/prober/task_normal.go:55`）。切换失败 → 本地版本号停留在旧版本 → conf server 持续认为有新配置 → 每个周期重试并重新写入版本目录。这解释了线上 14 个版本目录的堆积（每个服务端新版本一个目录，同名目录每轮被 `RemoveAll` 重建刷新 mtime）。

### 1.2 BFE 侧路径处理不一致（根因）

`bfe/bfe_server/bfe_confdata_load.go` 的 `TLSConfReload`（约 190-208 行）：

```go
certConfFile := srv.Config.HttpsBasic.ServerCertConf   // 启动时已 ConfPathProc 绝对化
tlsRuleFile := srv.Config.HttpsBasic.TlsRuleConf
if path := query.Get("path"); path != "" {
    certConfFile = joinPath(path, certConfFile)        // 重定位到版本目录
    tlsRuleFile = joinPath(path, tlsRuleFile)          // 重定位到版本目录
}
return srv.tlsConfLoad(certConfFile, tlsRuleFile)
```

而 `tlsConfLoad` 内部：

```go
clientCABaseDir := srv.Config.HttpsBasic.ClientCABaseDir    // <confRoot>/tls_conf/client_ca（绝对路径）
clientCRLBaseDir := srv.Config.HttpsBasic.ClientCRLBaseDir  // <confRoot>/tls_conf/client_crl（绝对路径）
clientCAMap, err := tls_rule_conf.ClientCALoad(tlsRule.Config, clientCABaseDir)
clientCRLPoolMap, err := tls_rule_conf.ClientCRLLoad(clientCAMap, clientCRLBaseDir)
```

`ClientCABaseDir`/`ClientCRLBaseDir` 在启动时经 `ConfPathProc` 绝对化（`bfe/bfe_config/bfe_conf/conf_https_basic.go:139,211`），**reload 时不受 `path` 参数影响**，永远指向 `<confRoot>/tls_conf/client_ca|crl`，即当前激活目录（死锁期间是 7月16日的旧普通目录）。

### 1.3 死锁闭环

1. 新配置引用新的 client CA 名称（`ClientCAName.crt` 只存在于版本目录的 `client_ca/` 中，由 CopyFiles 复制）。
2. BFE reload：cert/tls_rule 从版本目录读取成功，`ClientCALoad` 在旧 `tls_conf/client_ca` 下找不到 `.crt` → 报错 → monitor 返回错误 JSON。
3. conf-agent `TriggerBFEReload` 看到 `rsp.Error != ""` → 返回错误 → `UpdateDefaultConfDir` 不执行 → 符号链接不切换。
4. 回到 1，无限循环。BFE 持续使用旧配置，新证书/规则全部不生效。

同样的机制也适用于：新 CRL 文件、`ServerCertParse` 校验失败（`CheckTlsConf`）等任何使 `tlsConfLoad` 报错的情形——但那些属于"配置本身有问题，正确地不切换"；client CA/CRL 是"文件其实已就位，只是 BFE 看错了目录"，属于 BFE 的缺陷。

## 2. BFE 修复设计

### 2.1 修改内容

`bfe/bfe_server/bfe_confdata_load.go` 的 `TLSConfReload`：

```go
// reload tls conf
certConfFile := srv.Config.HttpsBasic.ServerCertConf
tlsRuleFile := srv.Config.HttpsBasic.TlsRuleConf
clientCABaseDir := srv.Config.HttpsBasic.ClientCABaseDir
clientCRLBaseDir := srv.Config.HttpsBasic.ClientCRLBaseDir
if p := query.Get("path"); p != "" {
    certConfFile = joinPath(p, certConfFile)
    tlsRuleFile = joinPath(p, tlsRuleFile)

    // NEW: client CA / CRL 目录与 cert/tls_rule 同样重定位到版本目录
    tlsConfRoot := filepath.Join(srv.ConfRoot, "tls_conf")
    if strings.HasPrefix(clientCABaseDir, tlsConfRoot+string(filepath.Separator)) {
        clientCABaseDir = joinPath(p, clientCABaseDir)
    }
    if strings.HasPrefix(clientCRLBaseDir, tlsConfRoot+string(filepath.Separator)) {
        clientCRLBaseDir = joinPath(p, clientCRLBaseDir)
    }
}

return srv.tlsConfLoad(certConfFile, tlsRuleFile, clientCABaseDir, clientCRLBaseDir)
```

配套：`tlsConfLoad` 增加 `clientCABaseDir`、`clientCRLBaseDir` 两个参数（其内部不再从 `srv.Config` 取）。

### 2.2 关键说明

- **`joinPath` 语义**：现有 `joinPath(path, suffix)`（`bfe_confdata_load.go:78`）取 `suffix` 的最后一段拼接，即 `joinPath("tls_conf_v1", "/root/conf/tls_conf/client_ca")` → `tls_conf_v1/client_ca`。版本目录经 CopyFiles 已包含 `client_ca`、`client_crl` 完整副本，因此重定位后文件必然存在。这与 cert/tls_rule 的既有处理方式完全一致，不引入新机制。
- **自定义绝对路径保护**：若用户在 bfe.conf 中把 `ClientCABaseDir` 配成 `tls_conf` 之外的绝对路径（如 `/mnt/ca`），前缀判断不命中，保持从原位置读取，行为不变。
- **不带 `path` 的请求**：所有判断都在 `path != ""` 分支内，直接请求 `/reload/tls_conf`（无 conf-agent 的场景）行为完全不变。
- **不影响启动加载**：启动路径 `bfe_basic/...` 直接调用 `tlsConfLoad` 的调用点需同步改为传 `srv.Config.HttpsBasic.ClientCABaseDir/ClientCRLBaseDir`，语义不变。

### 2.3 测试要点（bfe）

- 带 `path` 时，版本目录中存在 `client_ca/<name>.crt` 的 CA 能被加载（核心回归场景）。
- 带 `path` 时，版本目录中缺少 CA 文件仍报错（校验逻辑不变，只是目录换了）。
- `ClientCABaseDir` 配置为 tls_conf 之外的绝对路径时，带 `path` 也不重定向。
- 不带 `path` 时，从 `<confRoot>/tls_conf/client_ca` 读取（行为不变）。

## 3. conf-agent 加固设计

不改 `reload()` 的步骤顺序（"切换在 reload 成功之后"保证激活目录始终是 BFE 校验通过的版本，是合理的安全语义）。仅增强持久失败的可观测性：

- `Reloader` 增加 `consecutiveTriggerFailures int` 字段。
- `TriggerBFEReload` 失败时计数 +1；成功或 `len(fileList)==0` 时清零。
- 计数达到 10 的倍数时，除原有错误日志外额外输出一条汇总 ERROR：`tls_conf hot reload keeps failing (N consecutive), version=<v>, last_err=<err>，符号链接未切换，BFE 仍在使用旧配置`。
- 成功切换后如计数非零，输出一条恢复 INFO。

测试要点（conf-agent）：模拟 trigger 连续失败，验证计数、第 10/20 次汇总日志、成功后清零与恢复日志。

### 3.1 集成测试暴露的附加缺陷：CopyFiles 目录被拍平到版本目录根

SC27 端到端集成测试（bfe + conf-agent）暴露出 `StoreFile2TmpDir` 的 CopyFiles 处理还有一个独立缺陷，会导致即使 BFE 侧修复生效，reload 仍失败：

- `xfile.FileCopyRecursive(from, to)` 对**目录**的语义是"把 from 的*内容*复制进 to"（`xfile/file.go:53-73`）。`StoreFile2TmpDir` 对 `client_ca`、`client_crl` 等目录项调用 `FileCopyRecursive(dir, tmpDir)`（旧 `file_store.go:264`），导致目录内容被拍平到版本目录根（如 `tls_conf_<v>/example_ca.crt`），版本目录里根本没有 `client_ca/`、`client_crl/` 子目录。
- 进一步地，`copyDir` 只在遇到文件条目时才隐式创建目标目录（`copyFile` 内 `MkdirAll`），**空目录**（如没有吊销列表时的 `client_crl/`）整体复制会在末尾 `os.Chmod(dst, ...)` 时报 "no such file or directory"。

后果：BFE 修复后按版本目录加载 `ClientCRLBaseDir` 时报 `ClientCRLBaseDir tls_conf_<v>/client_crl not exists`，reload 依然失败、符号链接依然不切换——死锁以一种新形式复现。

修复（`conf_reload/file_store/file_store.go` + `xfile/file.go`）：

1. `StoreFile2TmpDir` 复制前 `os.Stat` 判断：源是目录时目标改为 `filepath.Join(tmpDir, copyFile)`，保留条目名。
2. `copyDir` 开头先 `os.MkdirAll(dst, srcInfo.Mode())`，使空目录也能被复制。

注意 `renameDir` 的 EXDEV 回退（`file_store.go:160-178`）依赖"目录内容复制进已存在的 dst"的旧语义，dst 为最终版本目录路径、复制前不存在，上述改动不影响该调用点。

测试要点：`TestStoreFile2TmpDirCopyFilesKeepDirEntry`（file_store）验证 `client_ca/`、`client_crl/` 子目录及文件保留在版本目录内、内容不被拍平到根。

## 4. 部署与恢复

见 [change-summary.md](change-summary.md) 第 6、7 节。要点：

1. **必须先升级 BFE**，否则"新增 client CA"类变更会再次死锁。
2. 存量机器按操作步骤手工切换一次符号链接后即可自愈。
3. conf-agent 升级 v0.0.6+ 以获得普通目录自动备份（`*.backup`）、过期版本清理能力，以及本节 3.1 的 CopyFiles 目录保留修复，避免后续版本目录再次堆积。

## 5. 遗留问题（不在本次范围）

- `session_ticket_key.data` 由独立的 `/reload/tls_session_ticket_key` 处理，本修复不涉及。
- `ServerCertConf`/`TlsRuleConf` 自定义为 tls_conf 之外路径时，`joinPath` 同样会错误重定向——这是既有行为，本次保持一致，不做改动。
- conf-agent 无对外状态接口，持久失败只能靠日志发现；如需监控对接，后续可考虑暴露 HTTP 状态端点。
