# ADR 0004 — pinned, verified dependencies and a single VM lifecycle path

- **Status:** Accepted
- **Date:** 2026-11-15
- **Deciders:** repo owner (mattbucci)

## Context

Two problems surfaced while auditing a host that had been running the sandbox
for months and had rebooted once.

**The supply chain of the sandbox itself was unpinned.** The project exists to
contain supply-chain attacks inside agent VMs, yet it built those VMs and the
host from moving targets: `curl … | bash` installers fetched from `main`
branches (uv, NodeSource, Helm, Trivy, Grype, sentry-cli), a `git clone` of
noVNC's default branch, an unpinned `pip install` list for the agent runtime
(with deepagents installed from GitHub `HEAD`), and binary downloads with no
checksum. Version numbers were scattered across eight scripts, and
`requirements.txt` was not what the image actually installed.

**The reboot path was a second, drifted implementation.** `agent-sandbox-boot.sh`
re-implemented `vm/launch.sh` by hand, and over time the copy diverged:

- restored VMs ran raw Firecracker as root, never under the jailer;
- their egress ACL came from a legacy `rootfs/agents/<type>/allowlist.txt`, not
  the compiled `build/<type>/allowlist.txt` (newer agent types had none);
- the service could start before DNS, and `dig`'s error text was interpolated
  into an nftables rule as if it were an address, so restored VMs silently lost
  their LLM passthrough rule;
- the gateway router and mnemosyne were not restarted;
- the checkout path was hardcoded.

The stop path had its own defects: `stop-all` aborted after the first VM
(`((count++))` under `set -e`), the systemd unit's `ExecStop` destroyed the very
instances the boot service was meant to restore, a stopped VM's jailer chroot
(holding a hard link to its rootfs) was never removed, and Squid's combined ACL
file kept referencing removed VMs.

## Decision

1. **One manifest, verified downloads.** `config/versions.env` is the only place
   a third-party version appears, and every downloaded artifact has a SHA-256
   there. `fetch_verified` (in `lib/versions.sh`) downloads to a temp file and
   only installs on a match. The config compiler inlines the manifest and the
   helper into each `customize.sh`, so install scripts in the rootfs chroot use
   the same pins. No installer is piped into a shell.

2. **Hash-locked Python runtime.** `requirements.txt` holds direct dependencies;
   `requirements.lock` (`sandbox-ctl deps lock`) pins the full tree with hashes
   for the guest's Python and platform. The image build and the in-VM fallback
   both install with `--require-hashes`.

3. **Pin policy: newest release at least 14 days old.** Fresh releases are the
   least vetted, and a sandbox host is a poor place to find that out. The same
   cutoff applies to the Python lock (`--exclude-newer`). Major upgrades that
   need migration or contract re-validation (hermes-agent, mnemosyne 4, the
   Ubuntu base) stay pinned until done deliberately.

4. **One VM lifecycle implementation.** `lib/vm.sh` owns network setup, the
   jailer staging, the VMM start and the noVNC proxy. `launch.sh` and the boot
   service both call it; the boot service's `vm_restore` is those same functions
   applied to a saved `info.json`. The base nftables ruleset likewise has one
   definition (`apply_base_nftables`).

5. **Shutdown preserves, stop destroys.** `stop-all --keep-state` (the unit's
   `ExecStop`) keeps instance disks for the next boot; a plain stop removes the
   instance, its jailer chroot and its Squid ACL. The boot service also restores
   mnemosyne and the gateway, waits for the LLM host to resolve, and falls back
   to the address recorded at launch.

6. **CI.** Go vet/tests with the race detector, the Python tests against the
   lock, ShellCheck, and a rootless shell suite covering the pieces above.

## Consequences

- Updating a dependency is an edit to one file plus `versions verify`; the
  `versions outdated` command shows what upstream has moved to.
- Images cannot be built when an upstream artifact changes under a pinned
  version — that is the point, but it means a re-tagged release needs a
  deliberate checksum update.
- Checksums are recorded for x86_64 only; other architectures must opt in with
  `ALLOW_UNVERIFIED=1`.
- Guest-side pins only take effect when images are rebuilt; host-side pins when
  the corresponding installer is re-run. Running VMs keep the Firecracker binary
  they were started with.
- apt-repository packages (kubectl, terraform, gh, Node.js) are pinned by series
  and trusted via the repository signing key rather than a per-file checksum.
  Capability presets' `pip`/`npm` package lists are still unpinned.
