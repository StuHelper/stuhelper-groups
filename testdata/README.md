# 上游事件测试夹具

qq-binding-owner-bindings.changed.json 从 qq-binding 提交 49df06ddf58a4521ccb07ff61442e0b5ded6fce2 的 outbox.go 和 postgres/outbox.go 生成。字段名取自 Envelope、EventData 的 JSON 标签，常量取自 PendingOutbox。

合成 outbox 行使用 ID 42、测试账号键 dGVzdA:dXNlcg 和 UTC 时间 2026-11-09T00:00:00Z。按照生产赋值规则，ID 转十进制字符串，subject 与 data.owner 相同，data.revision 保持 int64 数值。

夹具不含真实账号或群数据。groups 的测试只读取此 JSON，不导入或检出 qq-binding 私有源码。
