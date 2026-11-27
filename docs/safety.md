# Approval and tool safety

In `ask-for-approval` mode, write, edit, Bash, webfetch and plugin calls
ask first. Plain mode reads the answer from stdin, the TUI shows a dialog,
and RPC waits for its client. Read, grep, glob and `load_skill` are
exempt. Print mode (`-p`) refuses any call that would ask the user.

In `approve-for-me` mode, the configured model receives the original task,
the user's answers to questions in the same turn, and the effective tool
arguments in a separate, tool-free request. Bash reviews include the
resolved working directory. The executor checks its identity before launch
and refuses the command if the directory has changed. The reviewer has
45 seconds per call. Failure, timeout or an invalid response refuses the
call. Decisions use the same provider credentials and count toward
metering and turn budgets.

In `full-access` mode, the approval gate is disabled, including for
protected writes and host Git status. Tool validation, path restrictions,
the denylist and a configured Bash sandbox still apply.
`--deny-instead-of-prompt`, `OPENAI_DENY_INSTEAD_OF_PROMPT=1` or the config
field `denyInsteadOfPrompt` refuses any call that still needs approval
without asking the user or reviewer.

What is enforced:
- read/write/edit/grep/glob paths are jailed to the workspace root
  (`..` and symlink escapes refused). File and image operations use
  `os.Root`, which also protects the open/write operation against symlink
  swaps. Search subprocess paths are checked before launch; bash starts
  in the root.
- bash has a timeout (default 120s, per-call up to 600s,
  `-bash-timeout` / `OPENAI_BASH_TIMEOUT_SECONDS`) and a 64 KiB
  output cap.
- A denylist refuses the obvious catastrophes before they run:
  `rm -rf /` (or `~`), `mkfs` with a device, `dd` to a device,
  writes into `~/.ssh` / `~/.aws` / `~/.config/gcloud` / `~/.kube` /
  `~/.docker`, and anything touching the Docker socket.
- Every approval decision records its source in `<session>-audit.jsonl`.
  The same file records Bash runs and tool-policy denials.
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
  The fixed `git_status` lookup in a trusted workspace also runs on the host
  so linked worktrees can read their Git metadata. With the Bash sandbox
  enabled, the approval gate covers this lookup except in `full-access`
  mode. The tool takes no caller-supplied arguments.

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

Cancellation stops a foreground bash process tree. An explicit abort also
cancels background jobs belonging to the active session; normal turn cleanup
leaves them running. Switching sessions isolates job results, approval guards,
read snapshots and queued images. Background completions keep their owner's
audit destination after a switch. Job ownership survives switches within the
process. On session reopen after a crash or restart, persisted jobs are re-adopted once
live OS process identity (`PID`, `PGID`, and start token) is verified (with
transient initial inspection errors keeping the job `running` and stoppable
under guarded stop checks rather than marking it `unknown`). Both exited leaders
and reused leader PIDs with no pending cleanup and no independent surviving
verified group are marked `unknown` without sending signals or replaying
commands. When an exited or PID-reused leader has pending cleanup or surviving
descendants matching an independent persisted group proof, recovery revalidates
those descendants, stops them and marks the job `done` (or keeps it `running`
and stoppable on transient cleanup failure, marking it `unknown` only on
identity mismatch). Output captured before the crash remains readable via its
session-owned path (with the live stdout/stderr pipe having closed at process
exit).

Search subprocesses are not an OS sandbox: a concurrent path replacement
after validation remains a race. Use the sandbox or a disposable workspace
when files may be changed by untrusted processes.
