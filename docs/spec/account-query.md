---
type: spec
status: Accepted
updated: 2026-11-21
---

# 群组按账号信息查询

`accountquery.Service.Query(ctx, principal, query)` 返回群组模块拥有的账号记录。backend 负责将当前账号、管理授权范围和 information-query 的请求转换为本模块 Go 类型。此端口供宿主调用。

`Kind` 为 `account`，`Value` 为规范的 issuer/subject 账号键。`Scope` 包含 `School`、`Group`、`Module`。模块必须为 `groups` 或 `*`。`Purpose` 与 `RequestID` 必填，分别记录用途和请求关联标识。

调用方须拥有 `groups.account-query`。Authorizer 使用当前可信身份查询此能力，按学校和群与请求范围求交。无交集返回 `ErrForbidden`。授权或仓储故障返回 `ErrUnavailable`，整页失败。查询使用 5 秒超时并传递取消信号。

`Limit` 为 1 至 100，`After` 为上一页末条记录 ID。数据库先按授权和请求范围筛选，再按 ID 的 C 排序做排他分页。后续页重新读取当前权限和记录。

`Fields` 接受 `business`、`identity`、`document`、`material`、`refund_payee`。空列表或包含 `business` 时返回业务字段。本模块仓储仅投影业务字段，QQ 号码、认证材料、消息正文与管理命令自由文本均不在账号查询结果中。

结果来自成员、黑名单、警告、观察名单、动作和管理审计。动作与审计可查询该账号作为目标或操作者的记录。成员状态 `removed`、已过期的黑名单及已停用的观察记录以 `Current=false` 返回。

群内记录的学校和群来自群配置。平台级黑名单及其审计需要覆盖全部学校与全部群的查询授权。

记录查询 ID 使用数据库生成并持久保存的 UUID。Repository 的写入用 `clock_timestamp()` 刷新 `query_changed_at`，并保留原查询 ID。回滚同时撤销业务更新及时间更新。

backend 通过 `Service.Query` 执行授权、校验与查询。`postgres.Repository.ReadAccount` 是可信仓储端口。日志允许使用 Query 的脱敏字符串表示，禁止序列化完整查询或输出原始数据库错误。

| 场景 | 验证条件 |
| --- | --- |
| IQ-02 | 使用本域能力，按学校和群先过滤后分页，返回业务字段白名单，数据库运行身份按角色隔离，依赖失败时整页失败 |
