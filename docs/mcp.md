# MCP tools

`nexusd` serves its operations as MCP tools at `/mcp` over Streamable HTTP. Each tool is derived from one operation declaration (`def.Op`), so the tools, the REST API and the CLI share scopes, validation, audit and idempotency. The client setup example arrives with the rest of the documentation (NFR-9); this page records the tool semantics and the tool budget.

## Tool semantics

- **Names and descriptions.** The name and the concise description come from the operation's `MCP` binding. The adapter appends what an agent needs to use the tool: that it is long-running, how a log page continues, and that its output is untrusted.
- **Annotations.** `readOnlyHint` is true for read-only operations, `destructiveHint` true for destructive ones, and both are sent explicitly as `false` otherwise, because the MCP defaults are `readOnlyHint: false` and `destructiveHint: true`. `idempotentHint` is the operation's `Idempotent` flag. Annotations are hints; scopes are enforced on every call.
- **Schemas.** Every tool has an input schema and an output schema, both objects with no external `$ref`. Results are returned as `structuredContent` and as the same JSON in one text block.
- **Long-running tools** return the operation at once. Poll `get_operation` with its `id` until its status is `Succeeded`, `Failed` or `Canceled`; read its output with `get_operation_logs`; stop a queued operation, or a running one that allows it, with `cancel_operation`.
- **Retries.** Every mutating tool, `cancel_operation` included, accepts an optional `idempotencyKey` argument. It is removed from the arguments before validation and acts as REST's `Idempotency-Key` header: repeating a call with the same key and arguments returns the first call's operation instead of acting again. The header also works; if both are sent they must be equal. `cancel_operation` accepts the key and ignores it, since repeating a cancel changes nothing.
- **`whatIf`** appears exactly on the tools whose operation supports a preview (the apt upgrade, install and remove tools).
- **Log tools** (`get_operation_logs`, `get_container_logs`) have no `follow`, and passing it is a validation error. Each call returns one page, `{frames, nextOffset, truncated}`, holding at most 64 KiB of output text. While `truncated` is true, call again with `nextOffset`: as `offset` for operation logs (an absolute byte offset), as `since` for container logs (the RFC 3339 time of the last line; Docker may repeat lines logged at that time). Without `tail` or `since`, `get_container_logs` returns the last 200 lines.
- **Untrusted content.** Tools whose output carries host-originated text (container and operation logs, package metadata, update results quoting a registry) say so in their description and set `contentTrust: "untrusted-host-data"` in the result. The text stays in its data fields and is never placed in a description. `get_operation`, `list_operations` and `cancel_operation` set the same label when an operation they return belongs to such a tool.

## Tool budget

NFR-8 requires the tool names and descriptions, which a client loads into the model's context, to stay under a documented budget: **under 4,000 tokens** for the initial three providers (docker, apt, scripts) and the system tools.

Method: tokens are estimated, not counted by a particular tokenizer, as

```
tokens = ceil(total_chars / 4)
```

where `total_chars` is the sum, over every tool in the live `tools/list` of a server with every catalog provider enabled, of the character count of the tool's `name` plus that of its `description`. Input and output schemas are not counted.

`TestToolBudget` in `internal/mcpadapter/budget_test.go` measures this on every test run, through the real MCP SDK client, logs the total, fails at 4,000 or more, and checks that a planted long description fails the same check. Run it with `go test ./internal/mcpadapter -run TestToolBudget -v` to see the current figure. To stay within the budget, shorten the `MCP.Description` in the provider's `spec` package; the long `Description` is the REST and CLI help and is not counted.
