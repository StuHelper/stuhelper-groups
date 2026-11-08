---
type: guide
status: Accepted
updated: 2026-11-09
---

# Groups 装配指南

## backend

1. 将 groups 作为独立 Go 模块引入，注册 module.yaml 中的依赖和能力。平台必需；QQ 绑定、认证、资料与 AI 能力按操作分别判断。
2. 使用迁移身份执行 postgres.Schema。给 groups 运行身份授予 groups schema 的 USAGE、表的 SELECT/INSERT/UPDATE/DELETE 和序列 USAGE/SELECT，不授予其他模块数据权限或 DDL 权限。
3. 构造 postgres.Repository 与 groups.Service。Auth 注入 platform admin.Service，PlatformIdentity 注入相同的可信 admin.Identity。全平台黑名单额外检查平台管理员身份。
4. 注入 adapters.Facts：Binding 使用 qq-binding.Service.Status，Verification 使用 verification.Service.Facts，Principal 使用仅有 facts.read 权限的服务身份。Details 使用同一 Principal 提供阶段与逐字段披露许可。
5. 用 HTTP 认证中间件提供 account.Principal，挂载 httpapi.Handler。请求体中的角色、学校或主体声明不能替代认证身份。群的学校创建后不能通过配置迁移。
6. 注入 Bot 与可选 Analyzer。Bot 以 Action.ID 幂等执行并查询实际结果，使用绑定事实和配置把内部群、用户标识映射到平台标识。禁止从请求自行决定机器人身份。
7. 启动 Repository.Relay。它 LISTEN groups_outbox，启动时补读，空闲最多每 60 秒补读；按来源和对象保序，每批最多 100。投递失败从 5 秒指数退避至 1 小时，累计 24 小时进入 held。
8. relay 连接失败时由宿主按有上限的退避重启，重启立即补读。取消上下文会停止等待。每个模块只需一个主动 relay；多个 relay 下消费者仍须 inbox 去重。
9. 消费 action.requested 时调用 Execute，消费 action.result 时调用 Fanout。跨模块资格事件按成员所属群调用 Consume，并在宿主持久记录分页进度。事件正文不能直接授予资格。
10. 对每群分页调用 Sweep、Recover、Cleanup。每页最多 100，宿主保留游标并安排下一轮。Sweep 用于资格期限和提醒，Recover 处理执行租约，Cleanup 删除到期消息。

held 事件通过 Repository.Retry 按原 ID 重试。这个方法仅供后台装配使用，调用入口必须先执行平台管理授权和管理审计。发布通知不携带消息正文或身份材料。

动作状态包括 pending、executing、unknown、succeeded、failed、cancelled。只有 Bot 返回实际成功，动作才成为 succeeded。网络失败保留 unknown，后续先 Lookup；无法确定是否执行时继续保留 unknown。

同群待执行、执行中和结果未知的动作合计上限为 1000。单次外部动作最长 15 秒，执行租约 30 秒。宿主关闭时取消调用，租约恢复后继续查询实际结果。

## 外部依赖接口

以下是 groups 消费端的精确 Go 接口，定义与 adapters/dependencies.go 一致。verification/profiles 和 qq-binding 保持各自的数据归属，backend 用桥接适配器转换提供方 DTO 和错误，避免提供方反向导入 groups 形成循环依赖。

```go
package adapters

import (
    "context"
    "github.com/StuHelper/stuhelper-platform/account"
    v "github.com/StuHelper/stuhelper-verification"
)

// CardField separates disclosure permission from source availability. Only
// allowed and available fields may enter a group card; absence is not a violation.
type CardField struct {
    Allowed   bool
    Available bool
    Value     string
}

// GroupDetails describes current stage and approved card fields for one school.
// StageKnown=false never implies an ordinary enrolled student.
type GroupDetails struct {
    StageKnown bool
    Alumni     bool
    Temporary  bool
    Fields     map[string]CardField
}

// Details is a consumer-owned port. Backend bridges provider-owned DTOs without
// making verification or profiles import groups. Principal is a trusted service
// identity with school-scoped facts and per-field disclosure authorization.
type Details interface {
    GroupFacts(ctx context.Context, principal v.Principal, owner, school string) (GroupDetails, error)
}

// BotScope identifies a registered adapter instance. Platform is onebot or
// official; connect is never a bot transport or evidence of a shared QQ identity.
type BotScope struct {
    Platform string
    BotID    string
}

type BotIdentity struct {
    Bot    BotScope
    UserID string
}

// IdentityResolver is the backend bot bridge's consumer-owned port. The provider
// must check the current service identity, bot scope and binding on every call.
// Errors use groups sentinels (errors.Is); unavailable is never a missing binding.
type IdentityResolver interface {
    ResolveOwner(ctx context.Context, principal account.Principal, identity BotIdentity) (string, error)
    ResolveBotIdentity(ctx context.Context, principal account.Principal, owner string, bot BotScope) (BotIdentity, error)
}
```

### verification/profiles：GroupFacts

GroupFacts 的 principal 是 backend 注入的可信服务身份，owner 是 platform account.Key 编码，school 是群当前配置的学校。提供方每次检查服务身份是否有效、是否拥有该学校的资格细分读取权限，以及每个名片字段的披露权限。

现有 verification.Service.Facts 使用 facts.read 检查学校范围。GroupFacts 的 backend 桥接应复用这项授权，并通过 profiles/verification 的字段许可检查取得名片字段。原始证件、邮箱、手机号和未许可字段不进入返回值。

学校资格布尔值继续来自 Verification.Facts；GroupDetails 只补充当前阶段和字段许可。StageKnown=false 表示没有可用阶段事实，Alumni 和 Temporary 必须同时为 false，不能把未知阶段解释为普通在校生。

StageKnown=true 时，Alumni 表示该学校的当前有效资格属于校友，Temporary 表示临时新生。两个值都为 false 才表示已确认的其他有效阶段。提供方不得根据年份自行推断毕业或资格失效。

Fields 仅接受 school、grade、department、name、student_id、gender、nickname。每个字段分别返回 Allowed、Available 和 Value；被拒绝、缺失或尚未取得的字段不进入群名片，也不计作违规。

Allowed=false 表示当前服务身份和用途不允许披露；Available=false 表示事实尚未取得。两种情况下提供方必须返回空 Value。Allowed 与 Available 同时为 true 时可以返回已知空值；每个值最多 2048 字节。

gender 的可用值为 male、female、unknown，不从姓名等字段推断。nickname 来自获准取得的自由昵称。groups 还会过滤被拒绝或缺失的字段，拒绝未知字段名、非法性别和矛盾的阶段事实。

Details 为 nil 时 StageKnown 保持 false；返回 StageKnown=false 且 err=nil 是明确的未知事实。提供方故障返回错误，不能返回空的成功结果。要求学生资格的群不会依据未知阶段放行。

### qq-binding：ResolveOwner 与 ResolveBotIdentity

principal 是 backend 注入的可信机器人服务身份。提供方必须每次检查身份、绑定有效状态，以及对 BotScope.Platform 和 BotScope.BotID 的解析权限；这两个方法不对普通浏览器用户直接开放。

ResolveOwner 的 BotIdentity 必须来自经认证的机器人 session。Platform 只能为 onebot 或 official，BotID 标识已登记的机器人实例，UserID 是该实例命名空间内的身份。成功返回唯一、规范编码且当前有效的 owner。

ResolveBotIdentity 按 owner 和指定 BotScope 返回当前绑定身份，返回值的 Bot 必须与请求完全一致，UserID 不得为空。官方机器人身份按实例隔离，不把其他机器人实例的身份或 QQ 互联标识替代为目标身份。

两个解析方法只解析指定绑定项，不要求三项全部完成。入群资格仍由 Binding.Status 分别检查三项绑定。QQ 互联 connect 不能作为机器人平台；三项绑定不代表同一个 QQ。

backend 在每次接收事件与执行动作前重新解析，并检查当前群配置、动作授权与 owner 一致性。解绑后不得沿用旧身份缓存；解析结果不得把 UserID、群号或机器人 ID 当作权限证明。

### 错误语义

桥接适配器将提供方错误映射为以下 groups 错误，调用方使用 errors.Is 判断。未知错误返回 ErrUnavailable；取消与超时保留 context.Canceled、context.DeadlineExceeded。失败不能附带可使用的部分结果。

| 错误 | GroupFacts | 两个身份解析方法 |
| --- | --- | --- |
| ErrInvalid | owner/school 格式无效 | owner、平台、BotID 或 UserID 格式无效 |
| ErrUnauthenticated | 服务身份缺失或失效 | 服务身份缺失或失效 |
| ErrForbidden | 学校或资格细分读取越权 | 未获准访问该机器人实例或命名空间 |
| ErrNotFound | 完成授权后发现 owner 或学校不存在 | 完成授权后发现当前无绑定或绑定已撤销 |
| ErrConflict | 当前公开事实无法得到一致结果 | 存在歧义绑定，不能唯一解析 |
| ErrUnavailable | 依赖故障、未知错误或响应不符合约定 | 依赖故障或无法确认当前绑定状态 |

没有有效学生资格由 Verification.Facts 返回 CampusEligibility=false；阶段未知由 GroupFacts 返回 StageKnown=false，均不冒充依赖故障。字段级披露被拒绝通过 Allowed=false 表达；整个资格请求越权返回 ErrForbidden。

既有绑定状态与资格摘要接口保持如下签名。Binding.Status 没有调用者参数，仅供 backend 内部经授权的桥接使用，不直接作为公开的任意 owner 查询接口。

```go
type Binding interface {
    Status(context.Context, string) (q.Status, error)
}
type Verification interface {
    Facts(context.Context, v.Principal, string, string) (v.Summary, error)
}
```

上述 q 为 github.com/StuHelper/stuhelper-qq-binding，v 为 github.com/StuHelper/stuhelper-verification。Status 的字符串参数为 owner；Facts 的字符串参数依次为 owner、school。

接口定义不表示提供方已实现新方法。本轮仅更新 groups 消费端定义和桥接行为，verification/profiles、qq-binding 的公开实现及 backend 装配仍由各仓任务完成。

事件生产方按 EVENT-DELIVERY.md 输出 CloudEvents 1.0，携带稳定 ID、source、subject 和单调 revision。groups 的 Consume 接受 qq-binding 与 verification 来源，再读取当前事实。

## Koishi

现有 qq-binding 插件处理私聊绑定码。群服务需要独立处理群事件和命令，并保持原有绑定入口行为。

- 从受信任 session 提取 platform、selfId、guildId、userId、messageId，再由 backend 解析 owner。拒绝伪造的操作者字段和未登记的机器人、群。
- 上报成员申请、加入、消息和撤回事件。后端通过 MemberEvent 确认实际成员状态，通过 Ingest 保存获准留存的消息。未配置留存时不保存正文。
- 消费后端 Action，执行批准、提醒、欢迎、警告、禁言、解禁、踢出、发送、转发、撤回和名片修改。平台不支持的动作返回明确失败。
- 以 Action.ID 保存执行回执。实现 Lookup，区分已成功、已失败、确定未执行和结果未知。超时不得直接重发处罚或消息。
- 掷骰子与自助禁言使用当前请求人身份调用 Command，客户端不能指定其他人。禁言时长交给平台执行，到期由平台自动结束，管理员可提前解禁。
- 将群管理、信息查询、娱乐功能分别注册菜单。信息查询继续调用对应模块。聊天工作台收到 message.changed 后通过受授权接口读取消息，不能广播正文。
- 各群事件接收目标分别展示动作结果。图片使用受授权的媒体标识，媒体代理必须检查当前群权限与留存期限；本模块不生成公开图片 URL。

## 接口使用

OpenAPI 与模块版本为 0.2.0，HTTP 路由统一使用 /api/v1/groups 前缀。SDK 的 server 参数填写服务根地址。

GET /api/v1/groups 按当前管理范围分页；GET/PUT /api/v1/groups/{group} 读取或配置单群；POST commands 提交动作；GET progress 读取本人申请进度。

GET /api/v1/groups/{group}/{collection} 读取成员、动作、举报、规则命中、留存消息和订阅。消息读取使用 groups.messages.read，发送和撤回使用各自能力。查看撤回内容不新增审计。

POST batch 接受最多 100 个对象。preview 只显示对象和影响；实际执行返回每个对象的状态，pending 表示已经入队。POST /api/v1/groups/blacklists 支持单群、多群和全平台范围，必须提供原因。

所有写接口要求 Idempotency-Key。相同键和内容返回原结果，内容不同返回 conflict。分页上限为 100；时间为 UTC RFC3339，时长单位为秒，金额字段不适用。
