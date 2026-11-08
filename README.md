# AgentPay Provider SDK for Go

Official Go SDK for protecting an HTTP API or MCP endpoint with AgentPay's public x402 v2 payment profile on BOT Chain.

The SDK returns a `402 Payment Required` challenge, safely binds a signed retry to the original request, and orchestrates AgentPay's `Prepare -> Settle -> Fulfillment` workflow. It never holds a payer private key, broadcasts a transaction itself, or replaces AgentPay's server-side payment validation.

## AgentAuth payment execution

For an AgentAuth protected payment retry, send both `X-AgentAuth-Payment-Execution` and `X-AgentAuth-Claim-Receipt`. The SDK passes them as `agent_auth` metadata to AgentPay's Prepare and Settle endpoints. AgentPay verifies the Receipt and binds the execution to its original order. Requests with neither header keep the existing flow; requests with only one header are rejected. Provider applications should not parse or log the Receipt.

## Install

```bash
go get github.com/BOTChainLabs/agentpay-provider-go@v0.1.0-rc.1
```

## Compatibility

| Item | Supported value |
| --- | --- |
| AgentPay API | `/v1` |
| x402 | v2 |
| Public profile | `exact + permit2 + upfront` |
| Asset | configured BOT Chain USDT |
| Payer | EOA-compatible signer |

Kernel, Session Key and other private profiles are deliberately not part of this SDK.

## Security

Configure the Provider API key only in the Provider's server-side secret store. Do not send it to browsers, payer agents, or application responses. See [SECURITY.md](SECURITY.md) for reporting guidance.

## Development

```bash
go test ./...
go vet ./...
```

## License

Apache-2.0. See [LICENSE](LICENSE).
