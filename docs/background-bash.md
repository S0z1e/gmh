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
32 KiB head+tail; quitting with jobs running warns that they are
abandoned (their child processes keep running, orphaned).

Foreground commands follow turn cancellation until handed to the session as
background jobs. Explicit abort cancels the current session's jobs; finishing
a turn normally does not.
