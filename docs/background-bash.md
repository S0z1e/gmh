# Background bash (long commands don't block the loop)

A bash call that runs longer than the background threshold (default
120s, `-bg-threshold` / `OPENAI_BG_THRESHOLD_SECONDS`; `0` disables
it) keeps running as a job instead of blocking the model: the model
gets a "still running in the background (job bg-1)" observation and
continues with other work. When the model stops calling tools, the
loop waits for the running jobs and feeds each result back as a user
message ("Background job bg-1 finished (exit 0). Output: …"), so the
model can act on it. The result is a user message, not a second tool
message for the same call id — the call already has its tool result,
and a second one with the same id is a 400 from the API. A bash call
that finishes within the threshold never becomes a job. `/jobs` lists
the background jobs (id, status, elapsed, command, exit code once
done). The denylist, timeout, 64 KiB output cap and audit log apply
to background jobs exactly as to synchronous ones. `-p` print mode
never backgrounds (it stays synchronous). If the step cap hits (or
the API call fails) with jobs still running, their results are still
delivered to the session before the error is reported, capped at
32 KiB head+tail. Quitting (`/quit`, `/exit`, RPC `quit`, or orderly
shutdown) stops running and adopted jobs across the current and retained
sessions and saves their results before releasing session writer locks.

Explicit `background:true` jobs run with a 2-hour default max lifetime
(`DefaultBackgroundMaxLifetime`, 7200s) unless a per-call `timeout` (up
to 600s) is passed. Override the lifetime cap via `-bg-max-lifetime`,
`OPENAI_BACKGROUND_MAX_LIFETIME_SECONDS`, or `backgroundMaxLifetimeSeconds`
in config (`0` disables the cap).

Foreground commands follow turn cancellation until handed to the session as
background jobs. Normal turn cleanup or turn cancellation preserves
session-owned jobs, whereas an explicit abort (RPC `abort`) cancels
background jobs belonging to the active session. Each session has a separate
registry, so starting or resuming another session cannot inject the previous
session's results.

After a harness crash or restart, reopening a session validates each persisted
job's OS process identity (`PID`, process group, and OS start token): verifiable
live processes are adopted (with notice that post-crash stdout/stderr pipe
capture closed on crash while pre-crash output stays readable), while exited
processes or reused PIDs are marked `unknown` without signalling or automatic
command replay. Adopted jobs retain their persisted absolute deadline (records
written without one fall back to `Started` + 2 hours): remaining time is
enforced on adoption, deadlines that expired while the harness was down stop the
verified process group immediately on adoption with a recovery note, remaining
process-group members are cleaned up when an adopted leader exits, and wait
errors keep the job stoppable rather than marking it done. On Linux, post-crash
adoption detects process exit via `pidfd` or `/proc` polling and therefore
reports `exit status unavailable` (`ExitCodeUnknown: true`) instead of the
child's exit code (macOS and Windows recover the numeric exit code). Startup
and `/jobs` also surface orphaned running jobs from other unsupervised sessions
in the same workspace, naming the session to resume. `/jobs stop <id>` (and
`/jobs kill <id>`) explicitly stops a running or adopted job and refuses
`unknown` jobs safely.
