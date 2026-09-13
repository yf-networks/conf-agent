# 版本目录撞名与 reload 自愈设计

## 1. 背景

[conf-agent#20](https://github.com/rainway-ai-gateway/conf-agent/issues/20) 线上事故：某时刻 `cluster_conf` 的激活版本目录变为空目录，此后 80 余分钟每轮 reload 都在 CopyFiles 缺文件上失败，符号链接永不切换、新配置全部不生效，空版本目录堆积 30+ 个，且唯一恢复手段是人工复制文件。

事故由两个独立缺陷叠加而成，本文档描述对应的防御设计：

1. **版本目录撞名**：本轮版本目录名与当前激活目录名相同时，`RemoveAll` 会把活目录清空；
2. **错误链断裂**：CopyFiles 源文件缺失本可跳过，却因错误包装与判断方式不匹配，"跳过"分支成为死代码，把每轮都能拿到的全量自愈数据丢弃。

## 2. 版本号的构成与撞名路径

### 2.1 版本号语义

- 服务端（ai-gateway-api）版本戳为**秒级**时间戳（`20060102150405`），语义是"内容不变返回旧版本、内容变化才打新戳，戳 = 首次观测到变化的秒"；
- conf-agent 侧：每个 task 独立读取本地文件版本（本地缺失视为 `00000000000000`），请求服务端；**模块版本号 = 本轮所有 task 文件版本的最大值**（`reloader.go`）；
- 版本目录名 = `ConfDir + "_" + 版本号`，符号链接 `ConfDir` 指向上一次成功切换的版本目录。

### 2.2 撞名的两条现实路径

撞名 = 本轮 `tmpDir` 路径恰等于 `EvalSymlinks(ConfDir)`（当前激活目录）：

1. **跨 topic 同秒戳**：一个模块有多个 task（如 cluster_conf 的 cluster_table / gslb），各自独立水位线。task A 的变更先被拉到并切换（目录 `_<戳1>`）；task B 由任意 poller 在同一秒打出相同戳 → 下一轮本 agent 拉到 task B@戳1 → 模块版本 = max = 戳1 == 激活目录名。
2. **同机双进程竞态**：两个 conf-agent 进程（部署残留/手工误启）各按间隔轮询；服务端对旧版本请求持续返回同一版本，"慢一步"的进程拿到的就是对方刚切上的同名版本。

### 2.3 撞名的后果（修复前）

`StoreFile2TmpDir` 开头无条件 `RemoveAll(tmpDir)` → 活目录被清空 → CopyFiles 从空目录拷贝失败 → 死于缺陷二 → 符号链接永久留在空目录上。即使只修缺陷二（缺文件跳过），CopyFiles 内容（如 `tls_rule_conf.data`、`client_ca/`，不在 InnerAPI 中、无法从服务端找回）也已随活目录丢失，新版本目录不完整、BFE reload 照样失败。

## 3. 撞名保护协议（P0-b）

`StoreFile2TmpDir` 开头解析 `EvalSymlinks(ConfDir)`，与 `tmpDir` 比较：

| 判定 | 行为 |
|------|------|
| `tmpDir` ≠ 激活目录 | 原有逻辑：`RemoveAll` + `MkdirAll` 重建 |
| `tmpDir` == 激活目录（撞名） | **不删除**：输出 Info 告警；跳过源解析到 `tmpDir` 内部的 CopyFiles 项（拷贝到自身会截断文件）；`files` 照常覆写、`marker` 照常重写——同版本内容重写幂等无害 |

配套约束：

- trigger 与 `UpdateDefaultConfDir` 无需感知：撞名时切换目标等于当前目标，是天然 no-op；
- 撞名场景**绝不**在失败路径清理 tmpDir（半成品清理必须排除激活目录）；
- 该协议同样覆盖"本地版本号读取失败 → 服务端返回全量数据 → 版本恰等于激活目录"这一自愈路径——#20 事故现场即由此自动恢复。

## 4. 错误链契约（P0-a）

`xfile` 的约定：**凡是要被调用方做错误类型判断的返回错误，必须用 `%w` 包装，调用方用 `errors.Is` 判断**。

背景：Go 中 `fmt.Errorf("...: %v", err)` 会彻底切断错误链（`errors.Is` 也无法穿透），而 `os.IsNotExist` 不解包任意包装——两者组合使 `IsFileNotExistError` 对包装后的 not-exist 错误永远返回 false。

| 组件 | 修复前 | 修复后 |
|------|--------|--------|
| `FileCopyRecursive` 错误包装 | `%v`（链断裂） | `%w`（消息文本不变） |
| `IsFileNotExistError` | `os.IsNotExist(err)` | `errors.Is(err, fs.ErrNotExist)` |
| CopyFiles 源缺失 | 中止本轮（死代码分支） | 记 Info 跳过，本轮继续 |

`errors.Is(err, fs.ErrNotExist)` 既能穿透 `%w` 链，也能识别裸 `*PathError` / `syscall.ENOENT`，覆盖面是 `os.IsNotExist` 的超集。

## 5. 自愈闭环

修复后，#20 类卡死（激活目录被清空 / CopyFiles 源缺失）在一个 reload 周期内自动恢复：

1. `loadLocalVersion` 读不到本地文件 → 版本视为 `00000000000000`（裸错误判断，无包装问题）；
2. 服务端返回**全量配置**；
3. 撞名保护使 Store 不删活目录（或直接重建空目录）；CopyFiles 缺失项跳过；
4. `FileOverwrite` 写入全量配置、`writeVersionMarker` 重写标记；
5. trigger BFE reload 成功后切换符号链接（撞名时为同目标 no-op）。

## 6. 防御纵深（P1）

| 措施 | 位置 | 说明 |
|------|------|------|
| 切换前校验 | `UpdateDefaultConfDir` | 目标目录必须带 `.conf-agent-version` 标记，否则拒绝切换，杜绝符号链接指向半成品目录 |
| 半成品清理 | `StoreFile2TmpDir` 失败路径 | defer 清理本轮 tmpDir（撞名场景除外） |
| 空目录兜底清扫 | `cleanupOldVersions` | 无标记且为空的 `BaseName_*` 目录随每次切换清理（非空无标记目录保留，防误删用户目录） |
| 失败计数 | `reloader.go` | 连续失败计数覆盖 Store 与 Trigger 两个阶段，每 10 次输出汇总 ERROR |

## 7. 边界情况

| 场景 | 处理 |
|------|------|
| CopyFiles 源文件缺失 | Info 跳过，本轮继续 |
| CopyFiles 源是空目录 | 正常复制（`copyDir` 先建目录） |
| tmpDir 与激活目录撞名 | 就地重建，跳过自拷贝项，告警日志 |
| 撞名后 Store 失败 | 不清理激活目录，下轮重试 |
| 激活目录是普通目录（非符号链接） | `EvalSymlinks` 返回自身路径，撞名判断退化为路径比较，语义仍正确 |
| 无标记非空目录 | 清理时保留 |

## 8. 相关文档

- [配置加载与热加载流程](../配置加载与热加载流程.md)
- [配置目录版本管理与清理](../配置目录版本管理与清理.md)
- [tls_conf 版本目录下发与自包含性设计](../tls_conf版本目录下发与自包含性设计.md)（#19，CopyFiles 目录保留语义）
- `conf-agent/docs/zh_cn/modifications/2026-09-05-empty-version-dir-stall/`（本次修复变更记录与线上恢复手册）
