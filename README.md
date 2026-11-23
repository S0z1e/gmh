# minimal-harness

A minimal coding agent harness in Go. The core (CLI, agent
loop, tools, sessions, config, RPC) is stdlib-only; the TUI binary
adds Bubble Tea v2 + lipgloss v2. Eight tools: read, write, edit,
bash, grep, glob, `load_skill`, `webfetch`.

It stays small on purpose: a lightweight system prompt, no
sub-agents, no built-in permission popups. It also has suggest mode
(approval before write/edit/bash, webfetch and plugin tools), background bash jobs, JavaScript
plugins, plan mode via a plugin, and an RPC mode (JSONL over stdio)
for driving it from a GUI — see [rpc](docs/rpc.md).

Use it interactively, run one task headless with `-p`, or drive it
from a GUI over `--rpc`. See [docs/index.md](docs/index.md) for the
full documentation.

## Getting started

You need Go and an API key. Set the key in an environment variable or
in your provider's `apiKey` config field.

```bash
go build -o harness ./cmd/harness
```

Copy the example config into place (see
[configuration](docs/configuration.md) for the full format). It names
your providers and models; this example reads the key from an env var:

```bash
cp config.example.json ~/.minimal-harness/config.json
```

`config.example.json`:

```json
{
  "defaultProvider": "example",
  "defaultModel": "example-model",
  "providers": {
    "example": {
      "baseUrl": "https://api.example.com/v1",
      "api": "anthropic-messages",
      "apiKey": "$EXAMPLE_API_KEY",
      "models": [
        {"id": "example-model", "reasoning": true, "contextWindow": 256000}
      ]
    }
  }
}
```

Then run it in the project you want it to work in:

```bash
./harness -root /path/to/project

# One task, headless, then exit:
./harness -p -root /path/to/project "look at this project and add a test"

# Resume a session:
./harness --resume <session-id> -root /path/to/project
```

Flags and env vars override the config file (flag > env > model >
project file > global file > default). `OPENAI_BASE_URL`,
`OPENAI_MODEL` and `OPENAI_API_MODE` still work as overrides;
`-provider`, `-model`, `-api` and `-config` pick the provider, model,
API shape, or config file for one run. Three API shapes: `-api chat`,
`-api responses`, `-api anthropic`.

Usage is shown per turn and per session (prompt, cached, completion,
total tokens), always from the API's returned usage — never estimated
locally. Cost is estimated only when you set rates
(`OPENAI_PRICE_*_PER_MTOK`); every estimate is labelled with its
source and marked as unverified against the provider's actual prices.
Without rates, no estimate is shown.

## Safety

Approval is a policy boundary, **not a sandbox**. In `-mode suggest`
(the default), write/edit/bash, webfetch and plugin tools ask first; read, grep, glob and
`load_skill` never ask. `-mode auto` never prompts — use it only on a
disposable copy. `-p` print mode refuses every approval without
asking.

Paths are jailed to the workspace root, bash has a timeout and a
64 KiB output cap, and a denylist refuses the obvious catastrophes
before they run. But the denylist is pattern matching and can be
bypassed, and an approved bash call can `cd` out and write anywhere.
`--sandbox` runs bash under bwrap for real isolation of bash only.
Full details, including what is *not* promised, are in
[docs/safety.md](docs/safety.md).

## Documentation

| Doc | What it covers |
| --- | --- |
| [configuration](docs/configuration.md) | Providers, models, APIs, effort, cost rates, merge rules |
| [usage](docs/usage.md) | Modes, skills, sessions, compaction, commands, the TUI |
| [safety](docs/safety.md) | What suggest mode enforces, and what it does not promise |
| [background-bash](docs/background-bash.md) | Long bash calls as background jobs |
| [images](docs/images.md) | Sending images to a model that accepts them |
| [file attachments](docs/attachments.md) | Attach logs and other UTF-8 text files to a turn |
| [plugins](docs/plugins.md) | JavaScript plugins: tools, commands, hooks, plan mode |
| [architecture](docs/architecture.md) | Source layout |
| [rpc](docs/rpc.md) | `--rpc`: JSONL over stdio for a GUI |

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

`go build ./...` checks that every package compiles. To create the two
executables that the launcher expects, build both into the same directory:

```bash
go build -o bin/harness ./cmd/harness
go build -o bin/harness-tui ./cmd/harness-tui
```

On Windows use `harness.exe` and `harness-tui.exe`. The TUI is a separate
process, so `-p` and the plain CLI never import Bubble Tea.

### Release archives

```bash
go run ./cmd/harness-package -version v1.0.0 -out dist
# Or build just the current target, for example:
go run ./cmd/harness-package -targets darwin/arm64 -version dev -out dist
```

Linux/macOS archives are `.tar.gz`; Windows archives are `.zip`. Each contains
`harness`, `harness-tui` (with `.exe` on Windows), and `manifest.json` with
binary SHA-256 hashes. Extract both executables into the same directory and
keep these names. Published releases also include `SHA256SUMS` for the archives.
Existing archive paths are never overwritten.

CI runs the full race and subprocess suite on Linux/macOS, including real
bubblewrap checks on Linux. Windows runs native provider, session, configuration
and packaging checks. Bash tools on Windows require Bash (for example Git for
Windows) on PATH; Unix shell fixture tests and bubblewrap are outside that
Windows support boundary. All five release targets are compiled; native jobs
extract their own archive and exercise the production TUI sibling launch path.

### Fixed scenario baseline

```bash
go run ./cmd/harness-eval > /tmp/harness-scenario-baseline.json
```

Run this from a repository checkout with Go, Bash and Git available. Ten fixed
scenarios cover small changes, cross-file refactoring, fail/edit/rerun, nested
instructions, long output, repeated compaction, immutable images, failed
background builds, cancellation/recovery, and document injection. Their existing
file, command and transcript assertions are the success oracles. Reports record
the harness commit, dirty state, scripted fixture model, per-scenario result and
p50/p95 scenario duration; a missing or skipped selected test fails the run.

The default run uses scripted providers and leaves provider token and cost
metrics empty. To run the live suite separately with your own provider, use
an explicit configuration and a pinned model:

```bash
go run ./cmd/harness-eval -live-config /path/to/config.json -model MODEL_ID \
  -max-cost-usd 1 -max-tokens 50000 -timeout 20m > /tmp/harness-live-results.json
```

Live evaluation requires complete metering rates and Chat Completions or
Responses. It uses temporary workspaces, checks file contents and scope, and
limits shell commands to the fixture's checks and scripts. It stops dispatch
when metering is incomplete or the suite budget is exhausted. The last
in-flight request can exceed the budget because usage arrives with its
response. CI runs the scripted suite.
