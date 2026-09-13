# conf-agent 集成测试

本目录存放 `conf-agent` 的集成测试，采用**进程内集成**方式测试单个模块的完整流程。

## 测试范围

当前覆盖 `file_store` 清理机制与 #20 自愈相关的集成场景：

- 模拟 `ai-gateway-api` InnerAPI 返回配置；
- 模拟 BFE `/reload/{module}` 热加载接口；
- 启动真实 `Reloader` 轮询；
- 验证配置落盘、符号链接切换、版本目录清理是否符合 `VersionKeepCount`；
- 验证激活目录被清空 / CopyFiles 源缺失时 reload 自动恢复（[conf-agent#20](https://github.com/rainway-ai-gateway/conf-agent/issues/20)），空版本目录被兜底清扫。

## 目录结构

```
conf-agent/test/integration/
├── go.mod / go.sum
├── README.md
├── testutil/
│   ├── mock_server.go      # mock ai-gateway-api 与 BFE
│   └── runner.go           # Reloader 启动/停止辅助
└── tests/
    ├── cleanup/
    │   ├── cleanup_test.go # 配置目录清理集成测试
    │   └── design.md       # 测试场景与用例说明
    └── selfheal/
        ├── selfheal_test.go # 激活目录清空自愈 / 缺 CopyFiles 自愈 / 空目录清扫（#20）
        └── design.md        # 测试场景与用例说明
```

## 运行方式

```bash
cd conf-agent/test/integration
go mod tidy
go test -v -count=1 ./tests/...
```

## 设计说明

与 `ai-gateway-api/test` 不同，`conf-agent` 没有数据库和复杂的 HTTP 服务，因此采用**进程内集成**：

- 使用 `httptest.Server` 模拟上游 `ai-gateway-api` 和 BFE；
- 直接构造 `config.ReloaderConfig` 并启动 `conf_reload.Reloader`；
- 通过文件系统状态断言验证行为。

这种方式避免了编译和启动真实二进制，同时覆盖了 `prober → file_store → trigger` 的完整数据流。
