# 工程约定

遵循 StuHelper Next 工程约定。只修改 groups 仓库，跨模块通过公开接口读取当前事实。OpenAPI 先于实现，SQL 留在 postgres。机器人动作通过持久任务执行，超时先查询实际结果。撤回内容查看不得新增业务查看审计或记录正文日志。测试只使用隔离 PostgreSQL 和模拟机器人。
