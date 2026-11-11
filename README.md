# minimal-harness

A minimal coding agent harness in Go. The core (CLI, agent
loop, tools, sessions, config, RPC) is stdlib-only; the TUI binary
adds Bubble Tea v2 + lipgloss v2. Eight tools: read, write, edit,
bash, grep, glob, `load_skill`, `webfetch`.

It stays small on purpose: a lightweight system prompt, no
sub-agents, no built-in permission popups. It also has suggest mode
(approval before write/edit/bash), background bash jobs, JavaScript
plugins, plan mode via a plugin, and an RPC mode (JSONL over stdio)
for driving it from a GUI — see [rpc](docs/rpc.md).

Use it interactively, run one task headless with `-p`, or drive it
from a GUI over `--rpc`. See [docs/index.md](docs/index.md) for the
full documentation.

## Getting started

You need Go and an API key in the environment — the key is read only
from env vars, never from a file.

```bash
go build -o harness ./cmd/harness
```

Copy the example config into place (see
[configuration](docs/configuration.md) for the full format). It names
your providers and models; the keys stay in env vars:

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
(the default), write/edit/bash ask first; read, grep, glob and
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
| [plugins](docs/plugins.md) | JavaScript plugins: tools, commands, hooks, plan mode |
| [architecture](docs/architecture.md) | Source layout |
| [rpc](docs/rpc.md) | `--rpc`: JSONL over stdio for a GUI |

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

`go build ./...` also builds the TUI binary (`harness-tui`), which
`harness` re-execs when the TUI is selected, so `-p` and the plain
CLI never import Bubble Tea.
