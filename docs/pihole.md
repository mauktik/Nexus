# Managing Pi-hole through Nexus

The `pihole` provider manages a Pi-hole that runs on the same host as
`nexusd`. It reads Pi-hole's status, today's summary, the allow and deny
lists and the blocklists (adlists). It can turn blocking off and on, change
those lists, run gravity, and restart DNS or flush its cache. It also
manages local DNS names: A records grouped into zones, kept in one dnsmasq
file that Nexus owns.

This page covers the configuration, which Pi-hole versions are supported,
the DNS file and how to edit it by hand, backups and how to restore them,
and the caveats an operator must know. [security.md](security.md#pi-hole)
explains what each scope can do, and the generated
[CLI reference](cli/nexus.md) and [API reference](api.md) list every flag
and field.

## Configuration

```yaml
providers:
  pihole:
    enabled: true
    dnsFile: /etc/dnsmasq.d/03-custom-dns-names.conf   # The dnsmasq file Nexus owns
```

- `enabled` is `false` by default, like every provider.
- `dnsFile` is the one dnsmasq file Nexus reads and writes. The default is
  `/etc/dnsmasq.d/03-custom-dns-names.conf`. `nexusd validate-config`
  refuses a `dnsFile` that:
  - is not an absolute path;
  - is not directly in `/etc/dnsmasq.d` (a subdirectory or a `..` that
    leads elsewhere is refused);
  - has a name dnsmasq skips: one that starts with `.`, ends with `~`, or
    starts and ends with `#`;
  - is a file Pi-hole manages: `01-pihole.conf`, `02-pihole-dhcp.conf`,
    `04-pihole-static-dhcp.conf`, `05-pihole-custom-cname.conf` or
    `06-rfc6761.conf`.
- The file must exist and be a regular file, not a symbolic link. Nexus
  never creates it. To start from nothing, create an empty one with
  `sudo install -o root -g root -m 0644 /dev/null <path>`.
- Nexus never edits Pi-hole's own files: not `custom.list` (the web
  interface's Local DNS Records), `setupVars.conf` or `pihole-FTL.conf`.

Check the file with `sudo nexusd validate-config`, then apply it with
`sudo systemctl reload nexusd`. A reload takes effect for the next
operation; an operation already queued keeps the `dnsFile` it was given.

### When the provider is unavailable

The provider is checked at startup, at each reload, and when capabilities
are read with a refresh. It reports `unavailable`, with the reason, when:

- `nexusd` is not running under systemd;
- `/usr/local/bin/pihole` or `/usr/bin/pihole-FTL` is missing or not
  executable;
- the Pi-hole major version cannot be detected;
- `dnsFile` does not exist, is a symbolic link or is not a regular file;
- the host runs Pi-hole v6 and `misc.etc_dnsmasq_d` is not `true` in
  `/etc/pihole/pihole.toml`.

An unavailable `pihole` provider answers `capability-unavailable` and leaves
every other provider working. The check only reads; it never runs a Pi-hole
command.

## Supported Pi-hole versions

| | Pi-hole v5 | Pi-hole v6 |
| --- | --- | --- |
| Detection, versions in capabilities | Yes | Yes |
| Status, summary, blocking state | Yes | No: `capability-unavailable`, "v6 backend not implemented" |
| Allow and deny lists, adlists, gravity | Yes | No: `capability-unavailable`, "v6 backend not implemented" |
| Restart DNS, flush the cache | Yes | No: `capability-unavailable`, "v6 backend not implemented" |
| Read zones and records | Yes | Yes, when `misc.etc_dnsmasq_d` is `true` |
| Change zones and records | Yes | No: `capability-unavailable`, "v6 DNS write path not verified (ASM-6)" |

Pi-hole v5 is fully supported; it was built and checked against core
v5.18.4, web v5.21 and FTL v5.25.2. On v6, Nexus detects the version and
reads the DNS file, and nothing else. The v6 configuration test and restart
commands have not been verified, so DNS file writes stay unavailable on v6
until they are.

## Capabilities details

`nexus capability get` shows these details for the `pihole` provider:

| Detail | Meaning |
| --- | --- |
| `backend` | `v5-cli` on Pi-hole v5, `unsupported` on v6 |
| `coreVersion`, `webVersion`, `ftlVersion` | The detected versions, or null |
| `dnsFile` | The configured file |
| `dnsWrites` | `available`, or why zone and record changes cannot run: on v5, when `/etc/dnsmasq.conf` has no `conf-dir` that loads the file's directory; on v6, always |
| `preNexusBackupPath` | Where the pre-Nexus backup is or will be |
| `preNexusBackupExists` | Whether that backup exists |

These details are a snapshot, taken at startup, at the last reload, or at
the last capabilities read with a refresh. They are not re-checked on every
read, so `preNexusBackupExists` and `dnsWrites` can be out of date. Do not
base a decision on them:

- The write that creates the pre-Nexus backup says so at once: its result
  holds `preNexusBackup`, the backup's path. No other write sets it.
- `nexus capability get --refresh` (`GET /v1/capabilities?refresh=true`)
  re-runs the check and shows the current state.
- Every zone and record write checks `conf-dir` again itself before it
  changes anything.

## The DNS file

### Format

Nexus manages three kinds of lines in `dnsFile`:

- **A record:** `address=/<fqdn>/<ipv4>`, such as
  `address=/kiwi.home.local/192.168.7.5`. The name is lowercase labels
  (letters, digits and `-`), at least two of them, with no trailing dot.
  The address is a dotted-quad IPv4 without leading zeros, and never
  `0.0.0.0` or `255.255.255.255`, which mean "blocked" to dnsmasq.
- **Zone marker:** `# nexus:zone <zone>`, such as `# nexus:zone home.local`.
  It declares a zone and starts its section.
- **Unzoned marker:** exactly `# nexus:unzoned`. It starts the section of
  records that belong to no zone.

Every other line is unmanaged: comments, blank lines, `local=`, `server=`,
multi-name lines such as `address=/a.lan/b.lan/10.0.0.1`, `address=/x.lan/`
(the NXDOMAIN form), and records whose name or address is not valid as
described above (uppercase letters, a trailing dot, a line ending in a
carriage return). Nexus keeps unmanaged lines byte for byte, never
interprets them, and reports only how many there are. Nexus itself writes
only record lines and the two markers.

### Zones and membership

A zone exists only where a `# nexus:zone` marker declares it. Nexus never
creates zones on its own from the names in the file: until you declare
them, every record is unzoned.

A record belongs to the declared zone that is the longest suffix of its
name, on label boundaries. A record named exactly like a zone belongs to
it, and nested zones work: with both `home.local` and `lab.home.local`
declared, `nas.lab.home.local` belongs to `lab.home.local`. **Membership
comes from names, never from where a line sits.** A record line moved by
hand into another zone's section still belongs to the zone its name says,
and the next write puts it back in that zone's section.

A new record must fall under a declared zone (otherwise
`dns-name-unzoned`). Existing unzoned records, such as loose names like
`unity.local`, can still be changed and removed.

### What a record answers

Each record is a dnsmasq `address=` line, as Pi-hole's own local DNS file
uses, so it answers for the name **and every name under it**:
`address=/photon.home.local/192.168.7.5` also answers
`x.photon.home.local` with `192.168.7.5`. Where two lines cover a name, the
longer one wins.

Nexus never writes `local=/<zone>/`, `server=` or any other line that
changes how names outside the records resolve. A zone is only a grouping:
names in it that have no record are still forwarded upstream. That keeps
split-horizon domains working: with a zone `xi.example` holding
`router.xi.example`, `www.xi.example` still gets its public answer.

### Layout and the first write

The first Nexus write to a file with no `# nexus:` marker first saves a
copy of the file (see [Backups](#backups)), then reorganizes the whole file
into sections:

1. The unmanaged lines that were outside any section, in their order.
2. One section per declared zone, sorted by name: its marker, its records
   sorted by name, then the unmanaged lines that were in that section.
3. A `# nexus:unzoned` section with the unzoned records sorted by name and
   the unmanaged lines that were in it, only when there are any.

Sections are separated by one blank line. There is no header line.
Rendering a file that is already in this layout, with no change, gives the
same bytes. When the reorganized file holds exactly the same option lines
as before (only order, blank lines and markers changed), DNS answers cannot
change, so nothing is tested or restarted.

For example, these seven lines, in this order:

```
address=/unity.local/192.168.7.1
address=/photon.home.local/192.168.7.5
address=/kiwi.home.local/192.168.7.5
address=/router.xi.example/192.168.7.1
address=/unity.lan/192.168.7.1
address=/photon.app.lan/192.168.7.5
address=/photos.xi.example/192.168.7.7
```

parse as seven unzoned records. After declaring `home.local`, `app.lan` and
`xi.example`, in one batch:

```
nexus piholednsbatch invoke --changes add-zone:home.local --changes add-zone:app.lan --changes add-zone:xi.example
```

the file is:

```
# nexus:zone app.lan
address=/photon.app.lan/192.168.7.5

# nexus:zone home.local
address=/kiwi.home.local/192.168.7.5
address=/photon.home.local/192.168.7.5

# nexus:zone xi.example
address=/photos.xi.example/192.168.7.7
address=/router.xi.example/192.168.7.1

# nexus:unzoned
address=/unity.lan/192.168.7.1
address=/unity.local/192.168.7.1
```

### Editing the file by hand

The file stays yours to edit. Add, change or remove lines anywhere; the
next read and the next write accept them by the rules above. Keep these in
mind:

- **Membership follows names.** Where you put a record line does not
  matter; the next write moves it into the section of its zone.
- **Blank lines at the end of a section are layout.** The next write
  replaces them with the single blank line between sections. Every other
  blank line is kept where it is.
- **Removing a zone keeps its unmanaged lines.** Comments and other
  unmanaged lines inside a removed zone's section move to the end of the
  first block, before the first zone, and are never dropped.
- **A name on more than one line is a duplicate.** Each such record is
  listed with `duplicate: true` and cannot be updated or removed through
  Nexus (`409 conflict`) until you fix it by hand.
- **Your edit takes effect only at a DNS restart.** Nexus does not watch
  the file. Run `sudo pihole restartdns` or `nexus piholeresolver restart`.
  A later Nexus record change also restarts DNS, which applies your edit
  with it.
- **A broken hand edit blocks record changes.** If your edit fails dnsmasq's
  syntax test, every Nexus record change fails `dns-config-test-failed` with
  `preExisting: true` and is rolled back, until you fix the file.
- **Do not edit while a write runs.** A write that finds the file changed
  since it read it fails `dns-file-conflict` and changes nothing; run it
  again. To make sure you change what you looked at, pass the `etag` from a
  zone or record read as `--if-match`.

## How a DNS change runs

Zone, record and batch changes run in their own `nexus-op-<id>` unit, one
at a time behind every other Pi-hole operation. Each one:

1. reads the file and checks `--if-match`, when given;
2. applies the change, and refuses it (`404`, `409`,
   `dns-name-unzoned`) before writing anything when it does not fit the
   file;
3. on the first write, saves the pre-Nexus backup;
4. writes the new file beside the live one, under a dot-prefixed temporary
   name dnsmasq ignores, with the live file's owner, group and mode;
5. checks that nobody changed the live file meanwhile (otherwise
   `dns-file-conflict`, nothing changed);
6. copies the live file to the operation's directory as the rollback copy,
   and renames the new file over it;
7. runs dnsmasq's syntax test, `pihole-FTL --test`;
8. runs `pihole restartdns`, a full restart of FTL;
9. waits up to 15 seconds for FTL to answer on 127.0.0.1, then asks it for
   each changed name and compares the answer with what the file implies.

**Every failure after step 6 rolls back automatically.** If the test, the
restart or the check fails, the rollback copy is put back (and DNS
restarted again when it had been restarted), and the operation fails with
`dns-config-test-failed`, `pihole-command-failed` or
`dns-verification-failed`, each with `rolledBack`. If even the restart
after the rollback fails, the operation fails `dns-rollback-failed`: DNS may
be down, and the problem names the rollback copy to restore by hand.

A change that touches only comment lines (adding a zone, removing an empty
zone) stops after step 6: no test, no restart, no change in any answer. A
change that would leave the file as it is succeeds with `changed: false`
and writes nothing.

**A restart interrupts DNS for every client.** FTL stops answering while it
restarts, typically for about a second, and its cache starts empty, so
the next queries go upstream. A batch restarts DNS once, however many
records it changes.

### Verification needs FTL on 127.0.0.1

The check in step 9 queries FTL at 127.0.0.1:53, which Pi-hole v5's
default listening behavior (`local`) answers. If you restrict Pi-hole's
listening interfaces so that loopback is excluded, the check can
never pass: every record change fails `dns-verification-failed` and is
rolled back. Keep loopback in the listening set.

## Backups

### The pre-Nexus backup

Before its first write to a file without markers, Nexus copies the file
byte for byte to:

```
/var/lib/nexus/pihole/backups/<basename>.pre-nexus
```

`/var/lib/nexus/pihole/backups/03-custom-dns-names.conf.pre-nexus` with the
default `dnsFile`. Beside it, `<basename>.pre-nexus.json` records the
original's `uid`, `gid`, `mode`, `size`, `sha256` and `createdAt`. Both are
mode 0600 in a 0700 directory, outside `/etc/dnsmasq.d`, where dnsmasq
would load them. The backup is read back and compared with the original
before the file is written; if that fails, nothing is written.

Nexus never overwrites or deletes this backup. Listing, restoring or
deleting it through the API is not supported; restore it by hand.

### Restoring the pre-Nexus backup

Restoring puts back the file as it was before Nexus, without markers. Make
sure no Pi-hole DNS operation is queued or running (`nexus operation get`),
then, with the default `dnsFile`:

1. Read the original owner and mode, and check the backup is intact:

   ```
   sudo cat /var/lib/nexus/pihole/backups/03-custom-dns-names.conf.pre-nexus.json
   sudo sha256sum /var/lib/nexus/pihole/backups/03-custom-dns-names.conf.pre-nexus
   ```

   The checksum must equal `sha256` in the JSON.

2. Copy the backup to a dot-prefixed temporary file beside the live file,
   which dnsmasq ignores, and give it the recorded owner and mode. With
   `"uid": 0`, `"gid": 0` and `"mode": "0644"`:

   ```
   sudo cp /var/lib/nexus/pihole/backups/03-custom-dns-names.conf.pre-nexus /etc/dnsmasq.d/.03-custom-dns-names.conf.restore
   sudo chown 0:0 /etc/dnsmasq.d/.03-custom-dns-names.conf.restore
   sudo chmod 0644 /etc/dnsmasq.d/.03-custom-dns-names.conf.restore
   ```

3. Replace the live file in one step, and test the configuration:

   ```
   sudo mv /etc/dnsmasq.d/.03-custom-dns-names.conf.restore /etc/dnsmasq.d/03-custom-dns-names.conf
   sudo pihole-FTL --test
   ```

4. Restart DNS, which applies the file:

   ```
   sudo pihole restartdns
   ```

The backup stays where it is. The next Nexus write reorganizes the file
again, but does not replace the existing backup.

### Rollback copies

Every write also copies the file as it was just before the write to that
operation's directory, `/var/lib/nexus/ops/<operation id>/dnsfile.rollback`
(mode 0600). The automatic rollback uses it, and `dns-rollback-failed`
names it. To undo one write by hand, restore that copy the same way as the
backup above. A rollback copy is removed with its operation record, after
30 days once the operation is no longer among the newest 1,000.

## Blocking

`nexus piholeblocking stop` turns blocking off; DNS keeps answering, but
nothing is blocked. With `--duration-seconds N` (1 to 86400), blocking
comes back on by itself after N seconds; without it, it stays off until
`nexus piholeblocking start`.

The timed re-enable is a transient systemd timer,
`nexus-pihole-reenable.timer`, created before blocking is turned off, so a
failure to create it never leaves blocking off with no way back.
`nexus piholeblocking get` and `nexus pihole get` show when it fires as
`reenableAt`. A new stop replaces a pending timer, and a start cancels it.

- The timer survives the operation's end and restarts of `nexusd`.
- **It does not survive a reboot of the host.** Pi-hole keeps blocking off
  across the reboot, and nothing turns it back on. After a reboot during a
  timed stop, check `nexus piholeblocking get` and run
  `nexus piholeblocking start`. Pi-hole's own `pihole disable 5m` has the
  same limit.

Turning blocking off or on reloads Pi-hole's lists; it does not restart
DNS.

## Allow and deny lists, adlists and gravity

- Allow and deny entries (`piholedomain`) are written straight to
  `gravity.db` in one transaction, with the value as a bound parameter,
  then Pi-hole reloads its lists (`pihole restartdns reload-lists`) and
  the operation waits until FTL has reloaded them; DNS is not restarted. Adding a value that is on the opposite list is refused
  with `409 conflict`, rather than moved silently as `pihole -w` and
  `pihole -b` would.
- Adding an adlist (`piholeadlist add`) goes through `pihole -a adlist
  add`. The new list blocks nothing until the next gravity run downloads
  it, unless you pass `--update-gravity`.
- Removing an adlist (`piholeadlist remove`) is one `gravity.db`
  transaction: the list's gravity rows, then its group links, then the
  list. Pi-hole then reloads its lists (`pihole restartdns reload-lists`),
  and the operation returns once FTL has, so the list's domains are no
  longer blocked when it returns, without a gravity run.
- Nexus does not use `pihole -a adlist del`. Its bare `DELETE FROM adlist`
  is refused by the foreign key from the gravity table for any list a
  gravity run has downloaded, and Pi-hole still reports success, so the
  list would stay.
- After a remove, the `gravityDomains` count in `nexus pihole get` still
  shows the last gravity run's figure; it changes at the next run.
- `nexus piholegravity update` runs `pihole -g`. Its output streams to the
  operation log; it can take many minutes, up to one hour.

### Gravity runs and lost changes

A gravity run, whether Nexus starts it or Pi-hole's weekly cron job does,
builds a new `gravity.db` from a copy of the old one and then replaces it.
A domain or adlist change written to the old database after the copy is
lost. Nexus reduces the risk: before a domain, adlist or gravity operation
changes anything, it waits for a running gravity run
(`/opt/pihole/gravity.sh`) to finish, and fails `pihole-busy` after 30
minutes. It counts a process as a gravity run only when root runs it and
the script's path is its first argument, so another user's process cannot
hold Pi-hole operations back by naming the script. A gravity run that starts after that check, while the change is
being written, can still lose it. If a change was made near a scheduled
gravity run, read the list again to confirm it is there. Reads that land
while gravity replaces the database can answer `dependency-unavailable`;
retry them.

### FTL's list reloads

`pihole restartdns reload-lists` returns as soon as it has signalled FTL;
FTL then re-reads `gravity.db` on its own. If `gravity.db` is written while
FTL re-reads it, the reload fails ("database is locked" in
`/var/log/pihole/FTL.log`), and from then on FTL answers only `pi.hole`,
local DNS records included, until a later reload succeeds. Nexus keeps its
own writes out of that window:

- Before a domain, adlist or gravity operation writes `gravity.db`, Nexus
  reads FTL's log, and waits while FTL is reloading its lists
  ("waiting for FTL to finish reloading its lists"). After 30 seconds it
  fails `pihole-busy` with nothing changed.
- If FTL's last reload failed, or FTL does not answer a test query, Nexus
  first reloads FTL's lists, whoever caused the failure, and only then
  writes.
- After a domain add or remove, or an adlist remove, the operation returns
  once FTL has reloaded its lists and answers again ("FTL reloaded its lists
  in N ms"). This usually adds under a second. A failed reload is retried
  once.
- If FTL still cannot reload its lists, the operation fails
  `pihole-command-failed` for `pihole restartdns reload-lists`. The change
  was written and is not undone; the detail says so. Until a reload
  succeeds FTL may answer only `pi.hole`: run
  `pihole restartdns reload-lists` on the host.
- If FTL is not running, nothing waits: its lists load when it starts.
- Nexus reads FTL's log at `/var/log/pihole/FTL.log`, or at the
  `LOGFILE=` in `/etc/pihole/pihole-FTL.conf`. If FTL is running and the
  log cannot be read, the operation fails `dependency-unavailable` with
  nothing changed.
- Each check sends one query for a name like
  `nexus-probe-0123456789abcdef.invalid` to FTL on 127.0.0.1. Pi-hole
  answers it locally (`.invalid` is one of its local-only domains) and
  shows it in its query log.

A write outside Nexus, such as from Pi-hole's web interface or `pihole -b`,
can still meet a reload, Nexus's or another, and leave FTL in that state.
The next Nexus domain or adlist operation repairs it before it writes; so
does `pihole restartdns reload-lists`.

### Reading gravity.db

nexusd reads `gravity.db` with `CAP_DAC_READ_SEARCH`, so the file's
permission bits do not stop its reads. A read that answers
`dependency-unavailable` means the file is missing, locked, being replaced
by a gravity run, or not a Pi-hole database.

## DNS service

- `nexus piholeresolver restart` runs `pihole restartdns`: a full restart
  of FTL, with a short DNS outage for every client. It succeeds once FTL
  answers on 127.0.0.1 within 15 seconds.
- `nexus piholecache remove` flushes FTL's DNS cache with
  `pihole restartdns reload`, without a restart. It never runs
  `pihole flush`, which deletes Pi-hole's query log rather than the cache.

Pi-hole's FTL service allows 5 starts a minute; a sixth leaves FTL stopped.
Before each DNS restart it starts (`nexus piholeresolver restart`, a record
change, and the restart after a rollback), Nexus runs
`systemctl reset-failed pihole-FTL.service`, so that limit never refuses a
Nexus restart or rollback. Systemd's automatic restarts of a crashing FTL
stay limited. Restarts are serialized on the `pihole` lane. If a restart is
refused anyway, the `pihole-command-failed` problem says so and names the
recovery command, `systemctl reset-failed pihole-FTL.service && pihole
restartdns`.

## Commands

Every command takes `--host` and `--output`, and every mutation waits for
its operation unless given `--no-wait`; [the CLI reference](cli/nexus.md)
lists all flags. Destructive commands (`remove`, and the batch) ask for
confirmation on a terminal; pass `--yes` in scripts.

| Command | Does | Scope |
| --- | --- | --- |
| `nexus pihole get` | Status: versions, FTL and DNS health, blocking, gravity size | `pihole:read` |
| `nexus piholesummary get` | Today's query, blocked and client counts | `pihole:read` |
| `nexus piholeblocking get` | Blocking state and `reenableAt` | `pihole:read` |
| `nexus piholeblocking stop [--duration-seconds N]` | Turn blocking off | `pihole:write` |
| `nexus piholeblocking start` | Turn blocking on | `pihole:write` |
| `nexus piholedomain get [ID] [--filter list=deny] [--filter kind=regex]` | Allow and deny entries | `pihole:read` |
| `nexus piholedomain add --list deny --kind exact --value ads.example.com` | Add an entry | `pihole:write` |
| `nexus piholedomain remove --id ID` or `--list L --kind K --value V` | Remove an entry | `pihole:write` |
| `nexus piholeadlist get [ID]` | Adlists | `pihole:read` |
| `nexus piholeadlist add --url URL [--update-gravity]` | Add an adlist | `pihole:write` |
| `nexus piholeadlist remove --id ID` or `--url URL` | Remove an adlist | `pihole:write` |
| `nexus piholegravity update` | Run gravity | `pihole:write` |
| `nexus piholeresolver restart` | Restart DNS | `pihole:write` |
| `nexus piholecache remove` | Flush the DNS cache | `pihole:write` |
| `nexus piholezone get [ZONE]` | Declared zones, record counts and the file's etag | `pihole:read` |
| `nexus piholezone add --zone home.local` | Declare a zone | `pihole:dns` |
| `nexus piholezone remove ZONE [--cascade]` | Remove a zone; `--cascade` also deletes its records | `pihole:dns` |
| `nexus piholerecord get [FQDN~A] [--filter zone=home.local]` | Records; `--filter zone=unzoned` for the unzoned ones | `pihole:read` |
| `nexus piholerecord add --fqdn nas.home.local --ipv4 192.168.7.9` | Add a record | `pihole:dns` |
| `nexus piholerecord update nas.home.local~A --ipv4 192.168.7.10` | Change a record's address | `pihole:dns` |
| `nexus piholerecord remove nas.home.local~A` | Delete a record | `pihole:dns` |
| `nexus piholednsbatch invoke --changes ...` | Apply several zone and record changes at once | `pihole:dns` |

A record's ID is its name, `~` and its type: `nas.home.local~A`. The zone,
record and batch commands take `--if-match sha256:<hex>`, the `etag` of a
zone or record read, and refuse with `dns-file-conflict` when the file has
changed since.

### Batch grammar

`nexus piholednsbatch invoke` takes 1 to 100 `--changes` items, applied in
order. Each item is one word with no spaces, so it needs no quoting:

| Item | Does |
| --- | --- |
| `add-zone:<zone>` | Declare a zone |
| `remove-zone:<zone>` | Remove a zone that has no records |
| `remove-zone-cascade:<zone>` | Remove a zone and delete its records |
| `add-record:<fqdn>=<ipv4>` | Add a record under a declared zone |
| `update-record:<fqdn>=<ipv4>` | Change a record's address |
| `remove-record:<fqdn>` | Delete a record |

Each item is checked against the file as the items before it left it, by
the same rules as the single commands, so a batch can declare a zone and
add records to it. The batch is all or nothing: if any item fails, the
batch fails `dns-batch-rejected` with that item's `changeIndex` (counting
from 0) and, as `cause`, the problem the single command would have given;
nothing is written. A batch runs one syntax test and restarts
DNS at most once, then checks every changed name.

```
nexus piholednsbatch invoke \
  --changes add-zone:lab.home.local \
  --changes add-record:nas.lab.home.local=192.168.7.20 \
  --changes update-record:kiwi.home.local=192.168.7.6 \
  --changes remove-record:photon.app.lan
```

The same operations are available as MCP tools ([mcp.md](mcp.md)) and REST
routes under `/v1/providers/pihole/` ([api.md](api.md)).
