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
4. 注入 adapters.Facts：Binding 使用 qq-binding.Service.Status，Verification 使用 verification.Service.Facts，Principal 使用仅有 facts.read 权限的服务身份。Details 必须提供获准取得的阶段与名片字段。
5. 用 HTTP 认证中间件提供 account.Principal，挂载 httpapi.Handler。请求体中的角色、学校或主体声明不能替代认证身份。群的学校创建后不能通过配置迁移。
6. 注入 Bot 与可选 Analyzer。Bot 以 Action.ID 幂等执行并查询实际结果，使用绑定事实和配置把内部群、用户标识映射到平台标识。禁止从请求自行决定机器人身份。
7. 启动 Repository.Relay。它 LISTEN groups_outbox，启动时补读，空闲最多每 60 秒补读；按来源和对象保序，每批最多 100。投递失败从 5 秒指数退避至 1 小时，累计 24 小时进入 held。
8. relay 连接失败时由宿主按有上限的退避重启，重启立即补读。取消上下文会停止等待。每个模块只需一个主动 relay；多个 relay 下消费者仍须 inbox 去重。
9. 消费 action.requested 时调用 Execute，消费 action.result 时调用 Fanout。跨模块资格事件按成员所属群调用 Consume，并在宿主持久记录分页进度。事件正文不能直接授予资格。
10. 对每群分页调用 Sweep、Recover、Cleanup。每页最多 100，宿主保留游标并安排下一轮。Sweep 用于资格期限和提醒，Recover 处理执行租约，Cleanup 删除到期消息。

held 事件通过 Repository.Retry 按原 ID 重试。这个方法仅供后台装配使用，调用入口必须先执行平台管理授权和管理审计。发布通知不携带消息正文或身份材料。

动作状态包括 pending、executing、unknown、succeeded、failed、cancelled。只有 Bot 返回实际成功，动作才成为 succeeded。网络失败保留 unknown，后续先 Lookup；无法确定是否执行时继续保留 unknown。

同群待执行、执行中和结果未知的动作合计上限为 1000。单次外部动作最长 15 秒，执行租约 30 秒。宿主关闭时取消调用，租约恢复后继续查询实际结果。

## 需要补充的公开事实

verification.Facts 返回校园资格布尔值，不能表示校友、临时新生与允许用于群名片的资料。需要 verification/profiles 提供有授权检查的 GroupFacts 接口，返回当前阶段、字段许可及缺失状态。groups 不读取它们的数据库。

Details 缺失时，要求学生资格的群返回 unavailable 或拒绝资格，不能把未知阶段当普通在校生。无需学生资格的群可以只接入 QQ 绑定。学校资格不会因为新年度或毕业由 groups 自行失效。

qq-binding 需要提供机器人身份到平台 owner 的反向解析，以及 owner 到具体 OneBot/官方机器人绑定身份的受授权解析。三项绑定分别检查，不把三项视作同一个 QQ。

事件生产方需要按 EVENT-DELIVERY.md 输出 CloudEvents 1.0，携带稳定 ID、source、subject 和单调 revision。groups 的 Consume 只接受 qq-binding 与 verification 来源，再读取当前事实。

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

GET /v1/groups 按当前管理范围分页；GET/PUT /v1/groups/{group} 读取或配置单群；POST commands 提交动作；GET progress 读取本人申请进度。

GET /v1/groups/{group}/{collection} 读取成员、动作、举报、规则命中、留存消息和订阅。消息读取使用 groups.messages.read，发送和撤回使用各自能力。查看撤回内容不新增审计。

POST batch 接受最多 100 个对象。preview 只显示对象和影响；实际执行返回每个对象的状态，pending 表示已经入队。POST /v1/groups/blacklists 支持单群、多群和全平台范围，必须提供原因。

所有写接口要求 Idempotency-Key。相同键和内容返回原结果，内容不同返回 conflict。分页上限为 100；时间为 UTC RFC3339，时长单位为秒，金额字段不适用。
