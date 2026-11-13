//go:build integration

// Stub-style: SKIP-only (nginx-provider)
//
// Stubs of planning/nginx-provider/test-cases.md. Each test skips until its
// work item lands; the developer of that work item replaces the skip with
// the assertions written in the comment above the test. An un-skipped
// passing test is the GREEN signal (test-strategy.md, RED Stub Style).

package integration

import "testing"

// nginx-provider TC-33: The systemd branch of the installer behaves exactly as before, under dash
// Owner WI 3.2. Requirements: FR-24, NFR-9.
//
//	Setup: Ubuntu harness, using freshHost(t).
//	Steps:
//	  1. sh /harness/release/nexusd/install.sh (/bin/sh is dash on Ubuntu).
//	  2. Record files, modes, owners and hashes under /usr/local/bin,
//	    /etc/systemd/system, /etc/nexus and /var/lib/nexus.
//	  3. Repeat on a fresh host with the base SHA's install.sh (committed as
//	    test/harness/fixtures/base-install.sh from git show
//	    d04583a:deploy/install.sh), run with bash.
//	Expected:
//	  - The two recorded trees are identical, as are the stdout messages apart
//	    from timestamps.
//	  - The existing TC-179, TC-180 and TC-181 (base feature) still pass with sh
//	    instead of bash.
func TestSystemdInstallUnderDashUnchanged(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-33 (WI 3.2)")
}

// nginx-provider TC-47: install.sh on OpenRC installs, never enables, and restarts only on upgrade
// Owner WI 3.4. Requirements: FR-24, CON-12, ASM-17.
//
//	Setup: Fresh Alpine host state, with no /usr/local/bin/nexusd, no
//	/etc/init.d/nexusd and no /etc/nexus.
//	Steps:
//	  1. sh install.sh.
//	  2. Inspect.
//	  3. Edit /etc/nexus/nexusd.yaml (append a comment) and run again.
//	  4. rc-service nexusd start, record the PID, and run sh install.sh again.
//	Expected:
//	  - After step 1:
//	    - /usr/local/bin/nexusd and /etc/init.d/nexusd are root:root 0755;
//	    - /etc/nexus/nexusd.yaml is 0600 with the default content;
//	    - rc-update show lists no nexusd;
//	    - rc-service nexusd status is stopped;
//	    - stdout contains rc-update add nexusd default and rc-service nexusd
//	      start;
//	    - the config's listen address is loopback.
//	  - Step 3 leaves the config hash unchanged.
//	  - Step 4 restarts nexusd: the PID changes and the service is started.
func TestOpenRCInstallFirstAndUpgrade(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-47 (WI 3.4)")
}

// nginx-provider TC-48: uninstall.sh on OpenRC keeps data unless purged and warns about live supervisors
// Owner WI 3.4. Requirements: FR-24.
//
//	Setup: Installed and started. rc-update add nexusd default. One running
//	detached op.
//	Steps:
//	  1. sh uninstall.sh.
//	  2. Reinstall, then sh uninstall.sh --purge.
//	Expected:
//	  - Step 1:
//	    - stdout warns about the running supervisor and names its op ID;
//	    - nexusd is stopped and removed from every runlevel;
//	    - /etc/init.d/nexusd and the binary are gone;
//	    - /etc/nexus, /var/lib/nexus, /var/log/nexus and the audit log remain;
//	    - the supervisor is still alive (it is not killed).
//	  - Step 2 also removes /etc/nexus, /var/lib/nexus, /var/log/nexus and
//	    /etc/conf.d/nexusd.
func TestOpenRCUninstallAndPurge(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-48 (WI 3.4)")
}

// nginx-provider TC-ERR7: install.sh refuses on an unsupported init system and changes nothing
// Owner WI 3.4. Requirements: FR-24, CON-12.
//
//	Setup: unshare -m sh -c 'mount -t tmpfs none /run && sh install.sh', which
//	hides /run/systemd and /run/openrc. Snapshot hashes of /usr/local/bin,
//	/etc/init.d, /etc/systemd/system and /etc/nexus first.
//	Steps:
//	  1. Run it.
//	Expected:
//	  - A non-zero exit, with stderr naming systemd and OpenRC.
//	  - The snapshot is unchanged.
func TestInstallRefusesUnknownInit(t *testing.T) {
	t.Skip("not implemented: nginx-provider TC-ERR7 (WI 3.4)")
}
