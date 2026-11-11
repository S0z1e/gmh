# Safety (suggest mode) — what it can and cannot promise

Approval is a policy boundary, **not a sandbox**. In `-mode suggest`,
write/edit/bash ask first (stdin in plain mode, a y/n dialog in the
TUI); read, grep, glob and `load_skill` never ask. `-p` print mode
and `--deny-instead-of-prompt` (also `OPENAI_DENY_INSTEAD_OF_PROMPT=1`)
refuse every approval without asking — they only ever deny, never
approve, and the turn continues with the denial as an observation.

What is enforced:
- read/write/edit/grep/glob paths are jailed to the workspace root
  (`..` escapes refused); bash starts in the root.
- bash has a timeout (default 60s, per-call up to 300s,
  `-bash-timeout` / `OPENAI_BASH_TIMEOUT_SECONDS`) and a 64 KiB
  output cap.
- A denylist refuses the obvious catastrophes before they run:
  `rm -rf /` (or `~`), `mkfs` with a device, `dd` to a device,
  writes into `~/.ssh` / `~/.aws` / `~/.config/gcloud` / `~/.kube` /
  `~/.docker`, and anything touching the Docker socket.
- Every approval decision, every bash run (with exit code), and every
  denial is written to `<session>-audit.jsonl` next to the session
  transcript, for later review.
- webfetch refuses private, loopback (except localhost, kept for the
  user's own dev servers), link-local and cloud-metadata addresses
  before any network use, and never follows a redirect to another
  host — it reports the target instead. The connection is then
  pinned to one of the addresses the check already validated
  (transport-layer IP pinning), so a DNS record cannot change
  between the check and the dial; TLS still verifies against the
  hostname. Pinning is skipped when a proxy is in use — the proxy
  dials the target itself. It returns Markdown by default, in
  30,000-character pages (`offset` pages on), and caches a fetched
  page for 15 minutes.
- `--sandbox` (also `HARNESS_SANDBOX=1` or config `sandbox`) runs bash
  calls under bwrap (bubblewrap, Linux only): the filesystem outside
  the workspace is hidden behind a fresh tmpfs root, then only /usr
  and /etc come back (read-only), plus /proc, /dev, a fresh /tmp and
  the workspace itself (read-write, HOME set to it). A sandboxed bash
  cannot write outside the root; the network is NOT blocked, and each
  call gets a fresh /tmp. Setup fails fast when bwrap is missing or
  cannot create a sandbox here (unprivileged user namespaces
  disabled). read/write/edit run in-process and are jailed by the
  harness itself, not by bwrap.

What is NOT promised (residual risk — run on disposable copies or in
a container):
- The denylist is pattern matching over the command text; it can be
  bypassed with variables, quoting, or indirection.
- Without `--sandbox`, bash is only started in the root — once a bash
  call is approved it can `cd` out and write anywhere, including
  paths the write/edit tools refuse. Denying write/edit does not stop
  bash from writing.
- There is no OS sandbox by default; the denylist and jail are harm
  reduction, not isolation. `--sandbox` adds one for bash only.
