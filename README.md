---
type: index
status: Accepted
updated: 2026-11-21
---

# StuHelper Groups

groups 管理群配置、申请和成员资格、群规则、消息留存、举报、订阅及机器人动作。管理操作通过 platform 校验学校、群和操作能力。QQ 绑定、资格和名片字段由各来源模块提供当前事实。

[OpenAPI](contracts/groups.openapi.json) 版本为 0.4.0，HTTP 接口使用 `/api/v1/groups` 前缀。Go 和 TypeScript SDK 位于 [sdk](sdk)。群内事实的后台账号查询使用独立 Go 服务。

- [装配指南](docs/guide/integration.md)涵盖依赖、数据库、事件、Koishi 和 HTTP 接口。
- [账号查询规格](docs/spec/account-query.md)列出可查字段、权限范围和分页规则。
- [实施状态](docs/development/implementation.md)记录当前实现、测试范围和待验证事项。

```bash
make setup
bash scripts/check-with-postgres.sh
```

`make setup` 安装锁定的依赖和 Lefthook。工具链版本见 [.go-version](.go-version)、[.node-version](.node-version) 和 [package.json](package.json)，正式 Go 构建设置 `GOWORK=off`。

检查脚本启动仅监听独立 Unix socket 的 PostgreSQL 18，完成后清理本次数据库目录。使用 `STUHELPER_TEST_PG_BIN` 指定 PostgreSQL 程序目录。测试使用合成数据和模拟机器人。
