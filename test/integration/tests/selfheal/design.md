# 自愈与空目录清扫集成测试（conf-agent#20）

## 背景

验证 [conf-agent#20](https://github.com/rainway-ai-gateway/conf-agent/issues/20) 修复：激活目录被清空或 CopyFiles 源缺失时，reload 循环自动恢复，不再永久卡死；半成品空目录被兜底清扫。

对应修复说明：`conf-agent/docs/zh_cn/modifications/2026-09-05-empty-version-dir-stall/`。

## 用例

### TestSelfHeal_EmptiedActiveDirRecovers — 线上事故端到端复现

步骤：

1. 正常下发 v1；运维向激活目录放入 CopyFiles 文件 `static.data`；
2. 版本升级到 v2，`static.data` 被复制进 v2 版本目录；
3. **模拟事故**：清空激活的 v2 目录（配置文件 + CopyFiles 文件 + marker 全删）；
4. 不人工干预，等待下一轮 reload。

断言（修复后行为）：

- 本地版本号读取失败 → 服务端返回全量数据，其版本号恰等于激活目录名（版本撞名）；
- `StoreFile2TmpDir` 就地重建而非 `RemoveAll` 活目录，缺失的 `static.data` 跳过，配置文件与 marker 重写；
- 符号链接保持在重建后的目录，BFE 收到 reload 请求。

修复前行为：撞名时 `RemoveAll` 清空活目录，随即死于 keepFile 缺文件错误，符号链接永久留在空目录上（本用例超时失败）。

### TestSelfHeal_MissingCopyFileFromFirstDeploy — 首次部署即缺 CopyFiles 源

CopyFiles 引用了从未存在的文件，首次下发必须成功（跳过缺失项）。

修复前行为：`%v` 包装的 not-exist 错误无法被 `os.IsNotExist` 识别，"跳过"分支是死代码，每轮 abort，符号链接永远不会建立（本用例超时失败）。

### TestCleanup_SweepsEmptyUnmarkedDirs — 空无标记目录兜底清扫

手动制造一个无 marker 的空目录（半成品）和一个无 marker 的非空目录（用户目录），触发一次成功切换使 `cleanupOldVersions` 运行。

断言：空目录被清扫；非空目录保留。

## 运行方式

```bash
cd conf-agent/test/integration
go test -v -count=1 ./tests/selfheal/
```

## 回归价值验证

用 `git stash` 暂存 #20 修复源码后运行本目录测试，三个用例全部如期失败（前两个为卡死超时，第三个断言失败）；恢复修复后全部通过。
