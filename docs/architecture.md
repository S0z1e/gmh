# Layout

```
cmd/harness/main.go      // CLI: -p print mode, REPL, --resume, -mode suggest/auto
internal/llm/client.go   // OpenAI-compatible client, streaming, tool-call accumulation
internal/llm/image.go    // Image type + wire parts for chat/anthropic/responses
internal/tools/image.go  // LoadImage: workspace jail, type/size caps, dimensions
internal/agent/loop.go   // single loop: LLM -> tool -> observe -> repeat
internal/tools/tools.go  // read/write/edit/bash, workspace-root jail, timeout
internal/tools/sandbox.go  // --sandbox: bwrap invocation + probe
internal/tools/webfetch.go  // webfetch tool: http(s) fetch, HTML to Markdown, 30k-char pages, 15-min cache
internal/tools/html2md.go   // hand-written stdlib HTML to Markdown (tokenizer + rule table, no new dep)
internal/tools/ssrf.go      // webfetch SSRF guard: public-IP check, cross-host redirect refusal, IP pinning
internal/tools/grep.go   // grep tool (ripgrep backend, grep -r fallback, match cap + truncation, match count)
internal/tools/glob.go   // glob tool (rg --files backend, WalkDir fallback, limit + truncation, file count)
internal/session/session.go // JSONL append + resume
internal/session/manage.go // session list, rename, id validation
internal/config/config.go   // JSON config file: providers, models, merge (stdlib only)
internal/skills/skills.go  // .agents/skills/*/SKILL.md discovery, listing only
internal/plugins/plugins.go  // plugin discovery, registry, JS handler calls (moejs)
internal/plugins/host.go  // the `harness` host API: registerTool/log, jailed store
internal/rpc/server.go     // RPC mode: JSONL over stdio for a GUI (see [rpc](rpc.md))
internal/rpc/protocol.go   // RPC request/response/event JSON types
internal/agent/prompts/system.md // lightweight system prompt, embedded
```
