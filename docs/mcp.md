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
- **Log tools** (`get_operation_logs`, `get_container_logs`, `get_service_logs`) have no `follow`, and passing it is a validation error. Each call returns one page, `{frames, nextOffset, truncated}`, holding at most 64 KiB of output text, counted as it is sent in the result's `structuredContent`: a quote, backslash or newline costs 2 bytes, and `<`, `>`, `&` and any other control character 6 (the MCP SDK escapes `<`, `>` and `&` as `\u003c`, `\u003e` and `\u0026`), so a page of those holds fewer characters. The frames' own members are not counted, nor is the second escaping of the text block, which repeats the result's JSON as a string. While `truncated` is true, call again with `nextOffset`: as `offset` for operation logs (an absolute byte offset), as `since` for container and service logs (the RFC 3339 time of the last line; lines logged at that time may repeat). Every call moves the cursor forward. In an operation log, output longer than the room left in a page is cut there, and the next page continues from the byte after the cut. In a container or service log, a line that does not fit is left whole for the next page. Only a line larger than a whole page, or one logged at the very time passed as `since`, is cut: the page returns its start and paging continues with the next line, so the rest of such a line is not returned. Without `tail` or `since`, `get_container_logs` and `get_service_logs` return the last 200 lines.
- **Untimed frames.** Every log frame carries `t`. A frame whose `t` is the zero time, `0001-01-01T00:00:00Z`, is untimed: the line has no time of its own. OpenRC service log files carry no per-line time, so their frames carry it; it is never a real line time, and an agent should not read it as one. A page that holds an untimed frame has no `nextOffset`, because no time can continue it.
- **`get_service_logs` on OpenRC** reads the files the service declares, which have no per-line time, so it returns one page with no `nextOffset` and refuses `since`. If that page is `truncated`, the newest lines did not all fit: ask again with a smaller `tail`, which returns the newest lines. On systemd the journal gives every line its time, and `get_service_logs` pages with `nextOffset` as `get_container_logs` does. The tool needs `services:logs`, and every call is audited with the service's name, a page that the size ended included.
- **System services.** Four tools serve the `services` provider: `list_services` and `get_service` (`services:read`), `get_service_logs` (`services:logs`) and `control_service` (`services:write`), which starts, stops or restarts a service the host's allow-list names, with an `action` argument; it is long-running. A protected service, such as `nexusd`, `ssh` or `dbus`, or one not on the allow-list, is refused before anything runs ([services.md](services.md#protected-services)). The host reboot, `services.actions.reboot`, is not an MCP tool and appears nowhere in the tool list or its schemas: it is reachable only through REST and `nexus machine restart`, with `services:reboot` and the host's name typed by the operator ([security.md](security.md#services-and-reboot)).
- **`nexusd` itself.** Two read-only tools describe the daemon: `get_nexusd` (version, uptime, configuration path and rollback binary; `nexusd:read`) and `validate_nexusd_config` (checks the configuration file on disk without applying it; `nexusd:read`). Reload, restart, update, rollback and the release upload are not tools and appear nowhere in the tool list or its schemas: they need `nexusd:admin`, which is root-equivalent, and are reachable only through REST and the `nexus` CLI (see [security.md](security.md#nexusdadmin-is-root-equivalent)).
- **Tokens and the configuration file.** The six operations that manage `nexusd`'s own tokens and configuration file, `nexusd.tokens.list`, `nexusd.tokens.add`, `nexusd.tokens.remove`, `nexusd.config.list`, `nexusd.config.get` and `nexusd.actions.update-config`, are deliberately not MCP tools: none declares an MCP binding, so none appears in the tool list, and adding them changed neither the tool count nor the budget below. Each needs `nexusd:admin`, which is root-equivalent; a token add returns a secret that must not pass through a model's context, and a config set rewrites a root-owned file. They are reachable only through REST and the `nexus` CLI (`nexus nexusdtoken get|add|remove`, `nexus nexusdconfig get|update`; see [install.md](install.md#manage-tokens-remotely)). An agent's tokens are made for it by an operator, never by the agent.
- **Untrusted content.** Tools whose output carries host-originated text say so in their description and set `contentTrust: "untrusted-host-data"` in the result. They are `list_containers` and `get_container` (an update status detail can quote a registry's error), `get_container_logs`, `start_container`, `stop_container`, `restart_container` and `remove_container` (a refusal quotes the Docker Engine's text), `update_container`, `check_container_updates`, `get_operation_logs`, `get_package`, and the eight Pi-hole domain and adlist tools, `list_pihole_domains`, `get_pihole_domain`, `add_pihole_domain`, `remove_pihole_domain`, `list_pihole_adlists`, `get_pihole_adlist`, `add_pihole_adlist` and `remove_pihole_adlist` (their results carry the `comment`, `value` and `url` that anyone with Pi-hole's web UI or `pihole` command can have written), every nginx tool except `get_nginx_status` (site names, file data, site text and `nginx -t` output come from the host), the three host tools, `get_host_summary`, `get_host_health` and `get_host_metrics` (hostname, OS and CPU model, mount points, interface, disk, service, command and user names come from the host), and the four services tools, `list_services`, `get_service`, `get_service_logs` and `control_service` (service names, descriptions, unit paths, init system output and log text come from the host). The text stays in its data fields and is never placed in a description. `get_operation`, `list_operations` and `cancel_operation` set the same label when an operation they return belongs to such a tool.

## Tool budget

NFR-8 requires the tool names and descriptions, which a client loads into the model's context, to stay under a documented budget: **under 4,000 tokens** for the catalog providers (docker, apt, scripts, pihole, nexusd, nginx, host, services) and the system tools. With every catalog provider enabled, the tool list is measured at **3,024 tokens** across 72 tools, of which the services provider's four tools add 193, the host provider's three tools add 136, `nexusd`'s two tools add 41, and nginx's 13 tools are included.

Method: tokens are estimated, not counted by a particular tokenizer, as

```
tokens = ceil(total_chars / 4)
```

where `total_chars` is the sum, over every tool in the live `tools/list` of a server with every catalog provider enabled, of the character count of the tool's `name` plus that of its `description`. Input and output schemas are not counted.

`TestToolBudget` in `internal/mcpadapter/budget_test.go` measures this on every test run, through the real MCP SDK client, logs the total, fails at 4,000 or more, and checks that a planted long description fails the same check. Run it with `go test ./internal/mcpadapter -run TestToolBudget -v` to see the current figure. To stay within the budget, shorten the `MCP.Description` in the provider's `spec` package; the long `Description` is the REST and CLI help and is not counted.

Current figure: **72 tools, 12,093 characters, 3,024 tokens**, measured after the services provider was added.

History, newest first:

- **The services provider** added four tools, 771 characters, 193 tokens: within its own ceiling of 200 and the total ceiling of 3,100. Without them the list was 2,831 tokens (68 tools, 11,322 characters). Start, stop and restart share one tool, `control_service`, to stay within that ceiling, and the reboot has none.
- **The host provider** added three tools, 135 tokens, within its own ceiling of 150: without them the list was 2,696 tokens (65 tools). Without the two nexusd tools it was then 2,791 tokens (66 tools).
- **The nexusd provider** added two tools, 41 tokens: 2,655 tokens (63 tools) without them.
- **The nginx provider** added 13 tools. Before them the list was 2,024 tokens (52 tools): the system, docker, apt and scripts tools accounted for 1,028 tokens (24 tools) and the pihole tools for 956 (26 tools), within the pihole provider's own ceiling of 1,200.

The pihole provider adds these 26 tools:

- Status and blocking: `get_pihole_status`, `get_pihole_summary`, `get_pihole_blocking`, `stop_pihole_blocking`, `start_pihole_blocking`.
- Allow and deny entries: `list_pihole_domains`, `get_pihole_domain`, `add_pihole_domain`, `remove_pihole_domain`.
- Blocklists and gravity: `list_pihole_adlists`, `get_pihole_adlist`, `add_pihole_adlist`, `remove_pihole_adlist`, `update_pihole_gravity`.
- DNS service: `restart_pihole_dns`, `flush_pihole_cache`.
- DNS zones: `list_pihole_zones`, `get_pihole_zone`, `add_pihole_zone`, `remove_pihole_zone`.
- Local A records: `list_pihole_records`, `get_pihole_record`, `add_pihole_record`, `update_pihole_record`, `remove_pihole_record`.
- Batch: `apply_pihole_dns_changes`.

The host provider adds these 3 tools, all read-only and under `host:read`:

- `get_host_summary`: the host overview (`host.summary.get`).
- `get_host_health`: reboot required, failed services, OOM kills and the top processes (`host.health.get`).
- `get_host_metrics`: the in-memory metrics history (`host.metrics.get`).

The other seven host operations are REST and CLI only ([host.md](host.md#operations)).

The services provider adds these 4 tools:

- `list_services`: the host's system services and their state (`services.services.list`, `services:read`).
- `get_service`: one service's state, main PID, unit file and log source (`services.services.get`, `services:read`).
- `get_service_logs`: a service's recent log lines (`services.services.logs`, `services:logs`, audited).
- `control_service`: start, stop or restart an allow-listed service (`services.services.control`, `services:write`, long-running).

The per-verb start, stop and restart operations and the reboot are REST and CLI only ([services.md](services.md#naming-and-scopes)).
