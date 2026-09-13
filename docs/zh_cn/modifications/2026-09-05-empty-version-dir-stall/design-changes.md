# conf-agent#20 空版本目录卡死修复设计细节

## 1. 缺陷一：缺文件容错分支是死代码（无法自愈的直接原因）

### 1.1 代码现状

`conf_reload/file_store/file_store.go` `StoreFile2TmpDir` 的 CopyFiles 循环：

```go
if err := xfile.FileCopyRecursive(file, target); err != nil {
    if xfile.IsFileNotExistError(err) {   // ← 意图：源文件缺失 → 跳过继续
        xlog.Default.Info(...)
        continue
    }
    return err                            // ← 实际：永远走到这里
}
```

`xfile/file.go`：

```go
// :56 FileCopyRecursive 用 %v 包装，错误链被切断
return fmt.Errorf("FileCopyRecursive fail, from: %s, to: %s, err: %v", from, to, err)

// :27-33 IsFileNotExistError 用 os.IsNotExist，不解包 fmt 包装的错误
func IsFileNotExistError(err error) bool {
    ...
    return os.IsNotExist(err)
}
```

### 1.2 实证（临时验证程序，Go 1.x）

```
原始 ENOENT          os.IsNotExist: true    errors.Is(err, fs.ErrNotExist): true
经 %v 包装后          os.IsNotExist: false   errors.Is: false    ← 当前代码组合：两个判断都失效
经 %w 包装后          os.IsNotExist: false   errors.Is: true     ← 修复后组合：可判定
```

`%v` 包装使错误链彻底断裂（`errors.Is` 也无能为力），因此必须把**包装方式**和**判断方式**一起修。

### 1.3 为什么修好后必然自愈

卡死状态下 probe 每轮都能拿到全量数据：

1. `task_normal.go:126-130` `loadLocalVersion` 读活动目录的 `cluster_table.data`，缺失时返回 `""`——这里用裸错误判断 `os.IsNotExist(err)`，**没有包装问题，工作正常**；
2. 本地版本为空 → 服务端视为旧版本 → 返回**全量配置**，`files` map 里就装着完好的 `cluster_table.data` 内容；
3. 只要 CopyFiles 的缺失项被跳过（而不是中止），后续 `FileOverwrite` 写文件 → 写 marker → trigger → 切换符号链接一轮完成。

即：修复前是"拿着解药死在门口"，修复后任何同因卡死在一个 reload 周期（默认 5s）内自愈。

## 2. 缺陷二：`RemoveAll(tmpDir)` 可删除活目录（第一个空目录的来源）

### 2.1 代码现状

`StoreFile2TmpDir` 开头：

```go
tmpDir := fileStore.tmpDir(version)          // ConfDir + "_" + version
os.RemoveAll(tmpDir)                         // 无条件删除
os.MkdirAll(tmpDir, os.ModePerm)
```

`version` 的来源（`reloader.go:125-132`）：

```go
version := ""
for _, one := range fileList {
    if one.Version > version { version = one.Version }   // 模块内所有 task 版本的最大值
}
```

### 2.2 撞名的三条现实路径

前提：版本戳为秒级（`ai-gateway-api/model/iversion_control/version_control.go:28`，`Format("20060102150405")`），且"内容不变返回旧版本、内容变化才打新戳"：

1. **跨 topic 同秒戳**：cluster_conf 有两个 task（cluster_table.data、gslb.data），各自独立水位线。cluster_table 21:15:19 变更 → conf-agent 拉到并切到 `cluster_conf_20260904211519`；gslb 由任意 poller（另一台 BFE 的 conf-agent、e2e 收敛轮询）在同一秒打出同戳 → 下一轮本机拉到 gslb@20260904211519 → 模块版本 = max = 20260904211519 == 激活目录名 → `RemoveAll` 删活目录。高频变更窗口（如批量 E2E）同秒撞戳概率大增。
2. **同机双进程竞态**：两个 conf-agent 进程各 5s 一轮；服务端对 `local < V` 的请求持续返回同一版本 V，"慢一步"的进程拿到的就是先完成方刚切上的同名版本 → `RemoveAll` 删掉对方的活目录。
3. 两条路径殊途同归：删活目录 → 重建空目录 → keepFile 从空目录拷 `cluster_table.data` 失败 → 死于缺陷一 → 符号链接留在空目录上。

### 2.3 现场证据吻合点

`cluster_conf_20260904211519` 只有 6 字节（空目录）、**没有 `.conf-agent-version`**：marker 在成功路径末尾（`writeVersionMarker`）才写，一个成功完成的 Store 不可能产出无 marker 目录——它只能是"RemoveAll + MkdirAll 之后中止"的半成品。

### 2.4 为什么不能只做 P0-a

只修缺陷一，撞名时 `RemoveAll` 不再致命（内容可从服务端重新拉全量），但 **CopyFiles 的源文件随活目录一起被删了**：`tls_rule_conf.data`、`session_ticket_key.data`、`client_ca/` 等不在 InnerAPI 里的内容无法从服务端找回，新版本目录缺这些文件 → BFE reload 照样失败（tls_conf 场景即 #19 死锁复现）。因此 P0-b 是必须的。

## 3. 详细设计

### 3.1 P0-a：修复错误包装与判断（xfile/file.go）

```go
// FileCopyRecursive 两处包装：%v → %w（错误消息文本不变，只增加错误链）
return fmt.Errorf("FileCopyRecursive fail, from: %s, to: %s, err: %w", from, to, err)

// IsFileNotExistError：os.IsNotExist → errors.Is
import "errors" "io/fs"
func IsFileNotExistError(err error) bool {
    return errors.Is(err, fs.ErrNotExist)
}
```

`%w` 与 `%v` 的 `Error()` 输出格式一致，日志内容不变；`errors.Is` 既能穿透 `%w` 链，也能识别裸 `*PathError` / `syscall.ENOENT`，覆盖面是 `os.IsNotExist` 的超集。

顺手审计 `xfile` 与 `file_store` 中其余 `err: %v` 包装：日志用途的可保留，但**凡是要被调用方做错误类型判断的返回路径必须 %w**。

### 3.2 P0-b：撞名保护（file_store.go StoreFile2TmpDir）

```go
tmpDir := fileStore.tmpDir(version)

// P0-b: 禁止删除当前激活目录
activeTarget, evalErr := filepath.EvalSymlinks(fileStore.ConfDir)
absTmp, _ := filepath.Abs(tmpDir)
inPlace := evalErr == nil && activeTarget == absTmp   // 撞名：tmpDir 就是活目录

if !inPlace {
    if err := os.RemoveAll(tmpDir); ...   // 原有逻辑
    if err := os.MkdirAll(tmpDir, ...); ...
} else {
    xlog.Default.Warn(... "tmp dir equals active conf dir, rebuild in place" ...)
}

for _, copyFile := range fileStore.CopyFiles {
    file := filepath.Join(fileStore.ConfDir, copyFile)
    // P0-b: 撞名时源==目标，拷贝会自截断，必须跳过
    if inPlace {
        if abs, err := filepath.Abs(file); err == nil && abs == absTmp {
            continue
        }
    }
    ... // 原有复制逻辑（P0-a 后缺文件跳过不致命）
}
```

要点：

- 撞名时**不 RemoveAll**（目录里可能还有 CopyFiles 的唯一副本）；
- 撞名时**跳过源在 tmpDir 内部的 CopyFiles 项**（`copyFile` 打开源后 `os.Create` 目标会截断同一文件）；这些项的内容本就该等于现有内容，跳过安全；
- `files` 的 `FileOverwrite` 与 `writeVersionMarker` 照常执行——撞名意味着同版本内容重写，幂等无害；
- 撞名时输出一条 Info 告警日志（xlog 目前仅有 Debug/Info/Error 三级，无 Warn）；
- trigger / `UpdateDefaultConfDir` 无需改动：撞名时切换目标等于当前目标，是天然 no-op。

### 3.3 P1：防御纵深

1. **切换前校验**（`UpdateDefaultConfDir`）：建立新符号链接前检查目标目录存在 `.conf-agent-version` 标记，缺失则拒绝切换并返回错误——杜绝任何路径把符号链接切到半成品目录。
2. **半成品清理**（`StoreFile2TmpDir`）：失败返回前 `defer` 清理 tmpDir——但撞名场景（inPlace）**绝不清理**（那会删掉活目录）。
3. **空目录兜底清扫**（`cleanupOldVersions`）：在现有"带标记目录按 VersionKeepCount 清理"之外，增加对**无标记、且为空的** `BaseName_*` 目录的清理（仅当不是当前激活目标）。保守起见只清空空目录，不猜测非空无标记目录的归属。
4. **可观测性**（`reloader.go`）：把 `consecutiveTriggerFailures` 扩展为覆盖 `StoreFile2TmpDir` 失败的连续 reload 失败计数（#19 只统计了 trigger 阶段），汇总日志文案相应调整。

### 3.4 P2（不在本次范围，仅记录）

服务端版本戳改毫秒精度或单调序列，消灭"同秒同戳"等价类。需改 ai-gateway-api 与存量版本数据兼容，单独评估。

## 4. 边界情况

| 场景 | 处理 |
|------|------|
| CopyFiles 源文件缺失 | 记 Info 跳过（P0-a 后），其余文件正常写入，本轮可完成 |
| tmpDir 与激活目录撞名 | 不删除、就地重写、跳过自拷贝项，告警日志 |
| tmpDir 是无关旧目录 | RemoveAll 重建，行为不变 |
| StoreFile2TmpDir 中途失败 | 清理半成品目录（撞名场景除外），下轮重建 |
| UpdateDefaultConfDir 目标无 marker | 拒绝切换，返回错误 |
| 激活目录是普通目录（非符号链接，#19 前的存量形态） | `EvalSymlinks` 返回自身路径，撞名判断退化为路径比较，语义仍正确 |

## 5. 测试计划

### 5.1 单元测试

1. `TestIsFileNotExistError_Wrapped`：`%w` 包装的 ENOENT 判定为 true（修复前为 false，本用例在旧代码上必失败）。
2. `TestStoreFile2TmpDirMissingCopyFileSkips`：CopyFiles 含不存在的文件，Store 成功且其余文件写入。
3. `TestStoreFile2TmpDirActiveDirCollision`：tmpDir == 激活目录时，不删除目录、跳过自拷贝项、文件写入成功、marker 存在。
4. `TestUpdateDefaultConfDirRejectsDirWithoutMarker`：目标无 marker 时拒绝切换。
5. `TestCleanupOldVersionsRemovesEmptyUnmarkedDirs`：无标记空目录被清理，无标记非空目录与带标记目录不受影响。
6. `TestReloaderCountsStoreFailures`：StoreFile2TmpDir 连续失败触发第 10 次汇总 ERROR。

### 5.2 端到端验证

复用 integration-test 三组件环境：

1. 模拟 #20 现场：把激活目录清空，验证**不人工干预**下一轮 reload 自愈、符号链接切到带完整内容的新版本目录；
2. 模拟撞名：构造 tmpDir == 激活目录场景，验证活目录内容不被清空；
3. 验证空版本目录被兜底清理。

## 6. 参考资料

- [rainway-ai-gateway/conf-agent#20](https://github.com/rainway-ai-gateway/conf-agent/issues/20)
- `conf-agent/docs/zh_cn/modifications/2026-09-05-tls-conf-reload-path-fix/`（#19 修复，trigger 失败计数与本文 §3.3-4 直接相关）
- `conf-agent/xfile/file.go`、`conf-agent/conf_reload/file_store/file_store.go`、`conf-agent/conf_reload/reloader.go`
- `ai-gateway-api/model/iversion_control/version_control.go`（版本戳生成）
