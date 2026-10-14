// Package agentpayprovider 为 AgentPay 受保护 HTTP 资源提供 Provider 侧的
// x402 接入入口。
//
// 本包刻意分离“发起付款挑战”和“结算付款”。它加载已审核的付款要求、返回兼容
// 标准的 402 响应，并将付款重试安全地传给下游。B5.2 已封装 Prepare、Settle 与
// B6 履约租约/结果写入；业务结果仍必须由 Provider 的持久化 ResultStore 保存，
// 才能在同订单重试时完整回放。
package agentpayprovider
