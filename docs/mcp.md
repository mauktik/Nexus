# MCP tools

`nexusd` serves its operations as MCP tools at `/mcp` over Streamable HTTP. Each tool is derived from one operation declaration (`def.Op`), so the tools, the REST API and the CLI share scopes, validation, audit and idempotency. This page covers client setup, the tool semantics and the tool budget.

## Client setup

An MCP client needs three things: the endpoint, a token, and trust in the host's certificate. The first two are configuration; the third is one setup step per client.

### Endpoint and token

- **Endpoint:** `https://<host>:<port>/mcp`, for example `https://nas.lan:7443/mcp`, using the Streamable HTTP transport.
- **Token:** sent on every request as the header `Authorization: Bearer <token>`. Create one on the host:

  ```
  sudo nexusd token create --name agent --scope containers:read --scope packages:read --scope scripts:read --scope scripts:run:deploy-api
  ```

  It is printed once. Scope alone authorizes: there is no approval step, and an agent can do everything its scopes allow. Prefer read scopes plus `scripts:run:<id>` for the scripts the agent may run, and read [security.md](security.md) before granting a mutating scope.
- **Origin:** a request whose `Origin` header is not listed in `mcp.allowedOrigins` is refused with 403, so a web page cannot call `/mcp`. Clients that are not browsers normally send no `Origin` and are not affected.

### Certificate trust: one step per client

In `tls.mode: auto` the host serves a self-signed certificate. Trust that one certificate in the agent's client only, with the client's own option. Never add it to the operating system's trust store: that is a machine-wide change that every program and user on the machine inherits, when only this client needs it.

1. **Copy the certificate** to the client machine and check it against the fingerprint `nexusd` reports. On the host, print it with `sudo cat /var/lib/nexus/tls/cert.pem` and save the output on the client machine as `nas-cert.pem`; the certificate is public, and the private key stays in `key.pem` on the host. Then compare:

   ```
   openssl x509 -in nas-cert.pem -noout -fingerprint -sha256   # on the client machine
   sudo nexusd tls fingerprint                                  # on the host
   ```

   The two fingerprints must be equal.
2. **Use a name the certificate covers.** The generated certificate names the host's `host.name` (the OS hostname by default), `localhost`, every non-loopback address the host had when the certificate was generated, and each entry of `tls.extraSANs`. To connect by a DNS name such as `nas.lan`, list it in `tls.extraSANs` before the first start. The certificate is generated once and kept, because clients trust it; to change its names, delete both files in `/var/lib/nexus/tls/`, restart `nexusd`, and set up every client's trust again.
3. **Configure the trust in the client**, for example:
   - A Node-based client: start it with `NODE_EXTRA_CA_CERTS=/path/to/nas-cert.pem`.
   - curl: `--cacert nas-cert.pem`.
   - A Go client: the certificate in its `tls.Config.RootCAs`.
   - Any other client: its own CA-certificate option, if it has one.

   The certificate is a plain leaf (`IsCA: false`). curl 8.7.1 with `--cacert`, Node 23.11 with `NODE_EXTRA_CA_CERTS` and Go's `RootCAs` each accept it once given it; other TLS stacks, such as rustls, have not been tested.

Check the endpoint, certificate and token together before configuring the agent:

```
curl --cacert nas-cert.pem -H "Authorization: Bearer $TOKEN" https://nas.lan:7443/v1/capabilities
```

Then give the client the endpoint and the header. The shape of the configuration depends on the client; many take something like:

```json
{
  "mcpServers": {
    "nexus": {
      "type": "http",
      "url": "https://nas.lan:7443/mcp",
      "headers": {"Authorization": "Bearer <token>"}
    }
  }
}
```

For a Node-based client, start it with `NODE_EXTRA_CA_CERTS=/path/to/nas-cert.pem` in its environment, and nothing else is needed.

### Fallback: an agent on the host, over loopback

If the client cannot be given its own trust anchor, or rejects the self-signed leaf, run the agent on the Nexus host itself and serve plain HTTP on loopback, in `/etc/nexus/nexusd.yaml`:

```yaml
listen: ["127.0.0.1:7443"]
tls:
  mode: off
```

Then restart, since listen addresses and TLS are fixed at startup: `sudo systemctl restart nexusd`. The endpoint is `http://127.0.0.1:7443/mcp`.

- Use the IP `127.0.0.1` (or `[::1]`), not `localhost`: plain HTTP is accepted only on a loopback IP, and `nexusd validate-config` refuses `localhost`.
- `tls.mode: off` applies to every listen address and is accepted only when every one is a loopback IP, so the host is then reachable from itself only, and `nexus` on other machines cannot reach it.

## Tool semantics

- **Names and descriptions.** The name and the concise description come from the operation's `MCP` binding. The adapter appends what an agent needs to use the tool: that it is long-running, how a log page continues, and that its output is untrusted.
- **Annotations.** `readOnlyHint` is true for read-only operations, `destructiveHint` true for destructive ones, and both are sent explicitly as `false` otherwise, because the MCP defaults are `readOnlyHint: false` and `destructiveHint: true`. `idempotentHint` is the operation's `Idempotent` flag. Annotations are hints; scopes are enforced on every call.
- **Schemas.** Every tool has an input schema and an output schema, both objects with no external `$ref`. Results are returned as `structuredContent` and as the same JSON in one text block.
- **Long-running tools** return the operation at once. Poll `get_operation` with its `id` until its status is `Succeeded`, `Failed` or `Canceled`; read its output with `get_operation_logs`; stop a queued operation, or a running one that allows it, with `cancel_operation`.
- **Retries.** Every mutating tool, `cancel_operation` included, accepts an optional `idempotencyKey` argument. It is removed from the arguments before validation and acts as REST's `Idempotency-Key` header: repeating a call with the same key and arguments returns the first call's operation instead of acting again. The header also works; if both are sent they must be equal. `cancel_operation` accepts the key and ignores it, since repeating a cancel changes nothing.
- **`whatIf`** appears exactly on the tools whose operation supports a preview (the apt upgrade, install and remove tools, and `add_nginx_site` and `update_nginx_site`).
- **Log tools** (`get_operation_logs`, `get_container_logs`) have no `follow`, and passing it is a validation error. Each call returns one page, `{frames, nextOffset, truncated}`, holding at most 64 KiB of output text, counted as it is sent in the result's `structuredContent`: a quote, backslash or newline costs 2 bytes, and `<`, `>`, `&` and any other control character 6 (the MCP SDK escapes `<`, `>` and `&` as `\u003c`, `\u003e` and `\u0026`), so a page of those holds fewer characters. The frames' own members are not counted, nor is the second escaping of the text block, which repeats the result's JSON as a string. While `truncated` is true, call again with `nextOffset`: as `offset` for operation logs (an absolute byte offset), as `since` for container logs (the RFC 3339 time of the last line; Docker may repeat lines logged at that time). Every call moves the cursor forward. In an operation log, output longer than the room left in a page is cut there, and the next page continues from the byte after the cut. In a container log, a line that does not fit is left whole for the next page. Only a line larger than a whole page, or one logged at the very time passed as `since`, is cut: the page returns its start and paging continues with the next line, so the rest of such a line is not returned. Without `tail` or `since`, `get_container_logs` returns the last 200 lines.
- **Untrusted content.** Tools whose output carries host-originated text say so in their description and set `contentTrust: "untrusted-host-data"` in the result. They are `list_containers` and `get_container` (an update status detail can quote a registry's error), `get_container_logs`, `start_container`, `stop_container`, `restart_container` and `remove_container` (a refusal quotes the Docker Engine's text), `update_container`, `check_container_updates`, `get_operation_logs`, `get_package`, and every nginx tool except `get_nginx_status` (site names, file data, site text and `nginx -t` output come from the host). The text stays in its data fields and is never placed in a description. `get_operation`, `list_operations` and `cancel_operation` set the same label when an operation they return belongs to such a tool.

## Tool budget

NFR-8 requires the tool names and descriptions, which a client loads into the model's context, to stay under a documented budget: **under 4,000 tokens** for the initial three providers (docker, apt, scripts) and the system tools. The nginx provider's 13 tools are held to the same budget: the measurement below covers every catalog provider, nginx included, and stood at 1,700 tokens for 37 tools on 2026-11-13.

Method: tokens are estimated, not counted by a particular tokenizer, as

```
tokens = ceil(total_chars / 4)
```

where `total_chars` is the sum, over every tool in the live `tools/list` of a server with every catalog provider enabled, of the character count of the tool's `name` plus that of its `description`. Input and output schemas are not counted.

`TestToolBudget` in `internal/mcpadapter/budget_test.go` measures this on every test run, through the real MCP SDK client, logs the total, fails at 4,000 or more, and checks that a planted long description fails the same check. Run it with `go test ./internal/mcpadapter -run TestToolBudget -v` to see the current figure. To stay within the budget, shorten the `MCP.Description` in the provider's `spec` package; the long `Description` is the REST and CLI help and is not counted.
