---
type: guide
status: Accepted
updated: 2026-11-21
---

# 工程约定

遵循 StuHelper 工程约定。群配置、成员、动作、消息、举报、订阅、审计和投递数据由 groups 管理。SQL 留在 `postgres`。资格、披露许可与机器人绑定由 backend 通过公开端口提供。

先维护 OpenAPI，再用 `make generate` 更新 SDK。管理操作按当前学校、群和能力授权。机器人动作使用持久任务和执行回执，超时先查询实际结果。撤回内容查看遵循留存期限，禁止记录正文日志或新增业务查看审计。

行为测试使用隔离 PostgreSQL 与模拟机器人。按 [module.yaml](module.yaml) 的 `scenario_tests` 维护场景与测试函数的对应关系。完整检查运行 `bash scripts/check-with-postgres.sh`。
