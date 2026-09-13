# 修复 CopyFiles 缺文件导致 reload 永久卡死与版本目录撞名删活目录（conf-agent#20）

## 1. 背景

线上收到 [rainway-ai-gateway/conf-agent#20](https://github.com/rainway-ai-gateway/conf-agent/issues/20) 反馈：`bfe/conf/cluster_conf` 符号链接指向的 `cluster_conf_20260904211519` 是空目录，此后 80 余分钟里每轮 reload 都在 `keepFile fail ... cluster_table.data: no such file or directory` 上失败，符号链接永不切换、新配置全部不生效，期间产生了 30+ 个空版本目录无人清理。

**唯一有效的恢复手段是人工干预**（把历史有内容的目录复制回活动目录）。运维风险极大：若此期间重启 BFE，BFE 冷启动会读到空的 `cluster_conf/` 直接无法启动。

## 2. 根因

两个独立缺陷叠加（详见 [design-changes.md](design-changes.md)）：

1. **缺文件容错失效（无法自愈的直接原因）**：`xfile.FileCopyRecursive` 用 `%v` 包装错误切断错误链，而 `IsFileNotExistError` 用不解包的 `os.IsNotExist` 判断——`StoreFile2TmpDir` 里"CopyFiles 源文件缺失则跳过继续"的容错分支是死代码，缺失必然中止本轮。讽刺的是：本地文件缺失时 probe 每轮都能从服务端拿到**全量自愈数据**，却因这一步失败被整个丢弃。
2. **版本目录撞名时 `RemoveAll` 删除活目录（第一个空目录的来源）**：`StoreFile2TmpDir` 开头无条件 `os.RemoveAll(ConfDir + "_" + version)`。版本戳为秒级精度（`ai-gateway-api` 侧 `20060102150405`），且版本号取模块内所有 task 版本的最大值——cluster_conf 有 cluster_table / gslb 两条独立水位线，同秒撞戳（或同机双进程竞态）时，本轮 `tmpDir` 恰好等于当前激活目录，`RemoveAll` 把活目录清空。

连锁症状：

- 半成品目录没有 `.conf-agent-version` 标记 → `cleanupOldVersions` 只清扫带标记的目录 → 空目录无限堆积；
- 卡死发生在 `TriggerBFEReload` **之前**，#19 引入的 trigger 连续失败计数覆盖不到这里，故障静默 80 分钟。

## 3. 修复方案概述

| 优先级 | 修复 | 效果 |
|--------|------|------|
| P0-a | `xfile` 错误包装 `%v` → `%w`，`IsFileNotExistError` 改用 `errors.Is(err, fs.ErrNotExist)` | 缺文件恢复为"跳过继续"，**任何原因的同类卡死都变为 ≤ 一个 reload 周期自愈** |
| P0-b | `StoreFile2TmpDir` 检测 `tmpDir` 与激活目录（`EvalSymlinks(ConfDir)`）撞名时，跳过 `RemoveAll`，跳过源在 `tmpDir` 内部的 CopyFiles 项（防自拷贝截断），就地写入并告警 | 彻底消除"删活目录"这一数据破坏路径 |
| P1 | ① `UpdateDefaultConfDir` 切换前校验目标目录带版本标记；② `StoreFile2TmpDir` 失败时清理半成品目录（撞名场景除外）；③ `cleanupOldVersions` 兜底清扫无标记的空陈旧目录；④ StoreFile2TmpDir 连续失败纳入 #19 的汇总告警计数 | 防御纵深 + 故障可观测 |
| P2（不在本次范围） | 服务端版本戳改毫秒精度或单调序列，消灭"同秒同戳"等价类 | 需改 ai-gateway-api，单独评估 |

## 4. 影响范围

| 文件 | 影响 |
|------|------|
| `xfile/file.go` | 错误包装改 `%w`；`IsFileNotExistError` 改 `errors.Is` |
| `conf_reload/file_store/file_store.go` | `StoreFile2TmpDir` 撞名保护、失败清理；`UpdateDefaultConfDir` 切换前校验；`cleanupOldVersions` 空目录兜底清扫 |
| `conf_reload/reloader.go` | 连续失败计数从仅 trigger 扩展到 StoreFile2TmpDir |
| 测试 | xfile / file_store / reloader 单元测试与集成场景补充 |

## 5. 兼容性

- 正常路径（无撞名、无缺失）行为完全不变；
- `%w` 包装只增加错误链信息，不改变错误消息文本（`%w` 与 `%v` 输出格式一致）；
- 撞名保护只在 `tmpDir == 激活目录` 这一此前必然导致事故的分支生效。

## 6. 部署建议

1. 随 conf-agent 下个版本（v0.0.7 之后）发布，**无需 BFE 侧配合**；
2. 部署后对 conf-agent 的 EROR 日志（尤其 `StoreFile2TmpDir fail` / 连续失败汇总）配置告警——#19/#20 两次故障都是先静默数十分钟才被发现；
3. 保留 #19 的约束：**卡死期间严禁重启 BFE**（冷启动会读空配置目录起不来）。

## 7. 线上恢复手册（存量机器，不升级也可用）

针对 #20 描述的现场（符号链接指向空目录、keepFile 报错循环）：

1. **确认状态**：
   ```bash
   ls -l /home/work/bfe/conf/cluster_conf        # 确认链接目标
   ls /home/work/bfe/conf/cluster_conf/          # 确认为空
   tail -50 <conf-agent 日志>                    # 确认 keepFile fail 循环
   ```
2. **恢复内容**：找到最近一个有内容的版本目录（`ls -lh` 看大小，如 `cluster_conf_20260904210020`），复制到活动目录：
   ```bash
   cp /home/work/bfe/conf/cluster_conf_20260904210020/* /home/work/bfe/conf/cluster_conf_20260904211519/
   ```
   无需重启 conf-agent，下一轮 reload（默认 5s）即可走完全程自愈。
3. **清理空目录**（均无内容、无 `.conf-agent-version`，删除无风险；**保留活动目标目录和最近有内容的目录**）：
   ```bash
   cd /home/work/bfe/conf
   for d in cluster_conf_*; do
     [ "$d" = "$(readlink cluster_conf)" ] && continue
     [ -f "$d/.conf-agent-version" ] && continue
     [ -z "$(ls -A "$d")" ] && rmdir "$d"
   done
   ```
4. **排查触发源**（防止复发）：
   - `ps -ef | grep conf-agent` 确认无双进程；
   - 查 conf-agent 日志 21:15:15~21:15:40 时段，确认 `version=20260904211519` 前后是否有 `reload succ update` 紧跟 `keepFile fail`；
   - 查服务端 `t_config_version` 表，确认 cluster_table 与 gslb 两个 topic 是否存在同戳 `20260904211519`（有则坐实同秒撞名路径）。
5. **红线**：恢复完成前**严禁重启 BFE**。
