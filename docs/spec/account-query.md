---
type: spec
status: Accepted
updated: 2026-11-09
---

# groups 按账号信息查询

accountquery 包提供模块自己的 Go DTO 和授权查询服务，backend 负责转换账号、admin.Scope 与 information-query 类型。HTTP 路径、OpenAPI 和 SDK 保持现状。

```go
func (s accountquery.Service) Query(
    ctx context.Context, principal accountquery.Principal,
    q accountquery.Query,
) ([]accountquery.Record, error)
```

Kind 必须为 account，Value 为规范 issuer/subject 账号键。Scope 包含 School、Group、Module，分别对应学校、群和模块。Purpose 与 RequestID 必填，分别用于用途和请求追踪。

Limit 为 1–100，After 是上一页最后一条记录 ID。数据库先按当前授权和请求范围过滤，再按 ID 的 C 排序取排他分页。游标不会固定历史快照，后续页重新读取当前权限和记录。

Fields 使用 business、identity、document、material、refund_payee 类别。空列表返回业务字段与敏感字段掩码；显式列表限制业务字段并申请所列敏感类别的展开。只有模块实际拥有的字段会返回。

能力为 `groups.account-query`，已登记 module.yaml。Authorizer.Scopes 必须从当前可信身份和授权读取本能力的范围，不接受客户端角色，也不使用其他域或管理写入能力替代。

授权范围与请求范围求交，空交集返回 ErrForbidden。授权、Repository 或敏感查看审计不可用返回 ErrUnavailable，且不返回部分结果。调用有五秒上限并传递取消信号。

Query 的 String 和 GoString 隐去所有查询参数。错误不包含查询值或数据库原始错误。宿主不能将 Query 序列化后写入日志。敏感审计只保存内部记录 ID、字段类别、用途与授权依据。

返回成员资格和入群认证状态、截止时间、豁免、延期、警告、黑名单、关注与管理动作及审计结果。管理记录包含目标账号受到的处置及该账号作为操作者的记录。

学校与群来自 config，记录先与本域授权范围求交再分页。不会返回 QQ 号码、认证材料、消息内容或管理命令中的自由文本。成员记录保留 removed 状态，Current=false。

装配使用 accountquery.Service{Repo: repository, Auth: authorizer, Audit: auditor}。postgres.Repository 实现 ReadAccount；这是可信内部存储端口，外部入口必须调用 Service.Query。

| 场景 | 验证条件 |
| --- | --- |
| IQ-02 | 本域授权、学校和群范围先过滤、稳定排他分页、运行身份隔离、敏感能力独立、审计失败和依赖不可用 |

隔离 PostgreSQL 测试包含建表初始化幂等性、各类 Repository 更新的时间刷新，以及事务回滚对时间和业务数据的保护。数据库身份只获模块 SELECT 权限，写入应被拒绝。accountquery 单元测试检查拒绝、字段掩码、依赖失败和查询日志脱敏。

query_changed_at 与 query_id直接定义在 CREATE TABLE 的最终结构中。Repository 的写入 SQL 显式设置 query_changed_at，使用 clock_timestamp() 记录每次写入时间，同一事务内的连续更新也会刷新时间。

群记录的查询 ID 使用数据库生成并持久保存的 UUID，不编码账号键。建表初始化和记录更新保留查询 ID。
