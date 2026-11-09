# Running scripts through Nexus

The `scripts` provider runs scripts that the host's configuration catalogs,
such as a deploy script that pulls a repository and redeploys a container.
A script that works from your terminal is not yet known to work through
Nexus: the environment it runs in is different. This page lists what
changes, how a run ends, the permission checks every run passes, and the
checklist to work through before you rely on a script.

## The catalog

Scripts are defined only in `/etc/nexus/nexusd.yaml`, under
`providers.scripts.catalog`. No API, MCP or CLI call can add, change or
remove one, because that would give its caller arbitrary code execution.

```yaml
providers:
  scripts:
    enabled: true
    catalog:
      - id: deploy-api
        description: Pull main and redeploy the API container
        path: /home/deploy/repo/deploy-api.sh
        runAs: deploy
        workingDirectory: /home/deploy/repo
        timeout: 1h               # Default 1h
        concurrency: 1            # Default 1; one more run is refused with 409
        terminationGrace: 30s     # Default 30s, between SIGTERM and SIGKILL
        env: {PATH: "/home/deploy/.local/bin:/usr/local/bin:/usr/bin:/bin"}
        parameters:
          - {name: branch, type: string, pattern: '^[A-Za-z0-9._/][A-Za-z0-9._/-]*$', arg: "--branch"}
          - {name: apiKey, type: string, pattern: '^[A-Za-z0-9]+$', sensitive: true, env: API_KEY}
```

- `id`, `description`, `path` (absolute), `runAs` and `workingDirectory`
  (absolute) are required.
- A parameter's `type` is `string`, `integer` or `boolean`; `enum` limits it
  to listed values. A `string` parameter needs a `pattern`, so you decide
  about leading dashes and shell characters. A value with `arg` is passed as
  that option followed by the value, one with `env` as that environment
  variable, and one with neither as a positional argument in declared order.
  Values never pass through a shell.
- `sensitive: true` requires `env` and forbids `arg`, because arguments are
  visible to every user in `/proc`. A sensitive value is never written to
  the audit log, the operation record or any log.
- `env` values can be secrets, which is why `nexusd` refuses a configuration
  file that is not owned by root or that a group or others can read. Reads
  of the catalog list environment variables by name only.

Check the file with `sudo nexusd validate-config`, then apply it with
`sudo systemctl reload nexusd`. A reload swaps the whole catalog at once; a
run in progress keeps the definition it started with. An invalid file is
logged and the running catalog stays.

## How a run works

`nexus script invoke deploy-api --param branch=main` (or the `run_script`
MCP tool, or `POST /v1/providers/scripts/scripts/deploy-api/invoke`) checks
the parameters and the script's permissions, then starts the script in its
own systemd unit, `nexus-op-<id>.service`:

- executed directly by its path, with its arguments as an argument vector,
  never through a shell;
- as the `runAs` user, with that user's primary and supplementary groups,
  so membership of a group such as `docker` carries over;
- in `workingDirectory`;
- outside the `nexusd` sandbox, so it keeps running when `nexusd` restarts
  or is upgraded, and when `needrestart` restarts services after an apt
  upgrade.

The run is an operation. `nexus script invoke` waits for it and prints the
outcome; `--follow` streams its output while it runs, and `--no-wait`
returns at once. Read the output later with `nexus operationlog get <id>`.

## How a run ends

| The script | The operation | Details |
| --- | --- | --- |
| Exits with exit code 0 | `Succeeded` | The result holds `exitCode: 0` and the duration |
| Exits with any other code | `Failed`, `script-failed` | The error holds `exitCode` |
| Is killed by a signal | `Failed`, `script-failed` | The error holds `signal` |
| Passes its `timeout` | `Failed`, `script-timeout` | SIGTERM, then SIGKILL after `terminationGrace` |
| Is canceled | `Canceled` | SIGTERM, then SIGKILL after `terminationGrace` |

**The exit code is the only rule.** Nexus does not read the script's
output, so a script that exits 0 after something failed is reported
`Succeeded`. A script that ends with an `echo`, or runs without `set -e`,
can hide a failure this way. Start shell scripts with `set -euo pipefail`
and check that every failure path exits non-zero.

On a cancel or a timeout, SIGTERM goes to the script's whole process group,
and SIGKILL follows after `terminationGrace`. Trap `TERM` if the script must
clean up. Cancel a run with `nexus operation cancel <id>`. On the host,
`systemctl stop` on the unit is refused (`RefuseManualStop=yes`); an
operator who must stop one without the API uses `systemctl kill`:

```
sudo systemctl kill --kill-whom=main --signal=SIGTERM nexus-op-<id>.service
```

## What changes from a terminal

| In a terminal | Through Nexus |
| --- | --- |
| A TTY | **No TTY.** There is no controlling terminal, and stdout and stderr are pipes. `[ -t 1 ]` is false, and tools that check for a terminal turn off colors, progress bars and prompts, or refuse to run. |
| Your `TERM` | **`TERM=dumb`.** Programs that draw on the screen, such as pagers and `tput`, fall back or fail. |
| Your `.bashrc` and `.profile` | **Profiles are not sourced**: no `.bashrc`, `.bash_profile`, `.profile` or `/etc/profile.d`. Aliases, shell functions and anything they add to `PATH`, such as `~/.local/bin`, are absent. |
| Your environment | **Only these variables**: `HOME`, `USER`, `LOGNAME` and `SHELL` from the run-as user's `/etc/passwd` entry, `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin`, `TERM=dumb`, `LANG=C.UTF-8`, `XDG_RUNTIME_DIR` when `/run/user/<uid>` exists, then the catalog entry's `env`, then `env` parameters. Set `env.PATH` for tools elsewhere. |
| An SSH agent | **No `SSH_AUTH_SOCK`.** `git` over SSH needs a key it can use without an agent or a passphrase prompt, under the run-as user's `HOME`, or an HTTPS credential helper. The `docker` CLI reads registry logins from the run-as user's `$HOME/.docker/config.json`. |
| Keyboard input | **stdin is `/dev/null`**: the first read returns end-of-file. `read` gets nothing, and a prompt, such as one from `apt-get` without `-y`, fails or takes its default. |
| Output as it is written | **Output can be block-buffered.** Many programs buffer when stdout is a pipe, so progress can arrive in bursts, and stdout and stderr are ordered by arrival. Use `stdbuf -oL`, `python3 -u` or the program's own line-buffering option when timing matters. |
| `sudo` asks for your password | **`sudo` cannot prompt**: there is no terminal to type into, so `sudo` fails. Catalog the script with the `runAs` it needs, or allow the exact commands with a `NOPASSWD` sudoers rule. |
| Background jobs outlive the shell | **Background processes are stopped when the script ends.** The unit uses `KillMode=control-group`: when the script exits, is canceled or times out, any process it left in the background in its unit (`cmd &`, `nohup`, `disown`, `setsid`) is stopped (A-10). Containers it started are unaffected: they belong to Docker. Start a long-lived host process as a systemd service instead. |

## Integrity checks

Before every run, and again just before the script starts, `nexusd` checks
the script and every directory above it, up to `/`. A run that fails a check
is refused with `script-integrity-failed` (HTTP 422), naming the check, the
path and the exact command that fixes it. Symbolic links are resolved and
the directories they pass through are checked too.

| Check | Refused when | The fix the refusal names |
| --- | --- | --- |
| `script-exists` | The path does not exist | Install the script there, or correct `path` |
| `script-regular-file` | The path is not a regular file | Point `path` at the script file |
| `script-executable` | The run-as user cannot execute it | `chmod u+x <path>` when the run-as user owns it, otherwise `chmod a+x <path>` |
| `script-owner` | It is owned by someone other than root or the run-as user | `chown root <path>` |
| `script-writable` | It is group-writable or world-writable | `chmod g-w <path>`, `chmod o-w <path>` or `chmod g-w,o-w <path>` |
| `directory-owner` | A directory is owned by someone other than root or the run-as user | `chown root <dir>` |
| `directory-world-writable` | A directory is world-writable; the sticky bit does not exempt it, so `/tmp` fails | `chmod o-w <dir>`, or `chmod g-w,o-w <dir>` when its group is shared too |
| `directory-group-writable` | A directory is group-writable and its group is not private | `chmod g-w <dir>` |
| `run-as-user` | The run-as user does not exist | Create the user, or correct `runAs` |

**The private-group rule (A-59).** A group-writable directory passes only
when it belongs to a private group: every member of the group, by the member lists in
`/etc/group` and the primary groups in `/etc/passwd`, is root or the run-as
user. A group missing from `/etc/group` counts as shared. Script files
themselves must never be group-writable. With Ubuntu's default umask `0002`,
`/home/deploy` is `drwxrwxr-x deploy deploy`, and group `deploy` holds only
`deploy`, so the directory passes. A script created in it as `0775` is
refused with `chmod g-w /home/deploy/repo/deploy-api.sh`.

**Checkouts that hold scripts need `umask 022`.** `git` writes files with
the umask of the process that runs it. If a script pulls its own repository
under umask `0002`, every pull that rewrites the script makes it
group-writable again, and the next run is refused until you repeat the
`chmod`. Run `git clone` and `git pull` for such a checkout under `umask 022`,
for example with `umask 022` at the top of the script before it pulls.

## Repository write access is code execution

A script that pulls `main` and deploys it executes whatever `main` contains
when it runs, as the run-as user. Anyone who can push to that branch can
therefore run code on the host as that user (FR-20). Protect the branch,
limit who can push to it, and use the least privileged run-as user that
works. See [security.md](security.md).

## Onboarding checklist

Work through this for each script before you rely on it, or give an agent a
`scripts:run:<id>` token for it.

1. **Read the script.** Remove every prompt. Make every failure exit
   non-zero (`set -euo pipefail`), and make sure it does not end with a
   command that masks a failure.
2. **Check what it needs from your terminal session** against the table
   above: no TTY, `TERM=dumb`, no `.bashrc`, stdin at end-of-file, buffered
   output, a `sudo` that cannot prompt, background processes that are
   stopped. Set `env.PATH` and any other variable it needs in the catalog
   entry.
3. **Check credentials as the run-as user without a login session**: `git`
   access without an SSH agent, and registry logins in
   `~/.docker/config.json`.
4. **Fix permissions** so the integrity checks pass: owner root or the
   run-as user, not group- or world-writable, directories private, and
   `umask 022` for the checkout.
5. **Catalog it** in `/etc/nexus/nexusd.yaml`, then
   `sudo nexusd validate-config` and `sudo systemctl reload nexusd`.
6. **Run it through Nexus**, not from your terminal, and watch it:
   `nexus script invoke deploy-api --param branch=main --follow` (it asks
   for confirmation at a terminal; pass `--yes` elsewhere). Confirm it
   ends `Succeeded` and check its effect independently, for example with
   `nexus container get api`.
7. **Run a failing case through Nexus.** Make it fail on purpose, for
   example with a branch that does not exist, and confirm the operation
   ends `Failed` with a non-zero `exitCode`. If it ends `Succeeded`, the
   script hides failures, and Nexus will report a broken deploy as a
   success.
8. **Only then rely on it**, and grant `scripts:run:<id>` for this one
   script rather than `scripts:run`.
