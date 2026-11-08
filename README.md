# StuHelper Groups

QQ 群服务候选模块，采用 AGPL-3.0-only。模块提供群配置、入群资格、成员处置、消息留存、举报建议、群事件订阅与娱乐命令。所有管理操作由服务端按学校、群和能力调用 platform `admin.Service.Evaluate`。

[OpenAPI](contracts/groups.openapi.json) 版本为 0.2.0，HTTP 接口统一使用 `/api/v1/groups` 前缀，生成客户端位于 [sdk](sdk)。[装配指南](docs/guide/integration.md) 描述 backend、数据库与 Koishi 接入；[实施状态](docs/development/implementation.md) 记录验证与剩余接口。

```bash
python3 scripts/prepare-candidates.py
pnpm install --frozen-lockfile
pnpm exec lefthook install
bash scripts/check-with-postgres.sh
```

Go 使用 1.27.3，Node 使用 24.22.1，pnpm 使用 10.33.4。本地准备脚本从相邻仓库的固定提交导出 platform、verification 和 qq-binding 到 .tools/candidates，组合见 [candidate-sources.json](candidate-sources.json)。正式依赖版本与工作区锁由负责人统一固定。

测试脚本创建仅监听独立 Unix socket 的 PostgreSQL，结束时删除本次数据库目录。可通过 `STUHELPER_TEST_PG_BIN` 指定 PostgreSQL 程序目录。测试不访问真实 QQ 账号、真实群或生产系统。
