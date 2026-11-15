# Agents-A1 cache control, 2026-11-15

Ten sequential streaming chat requests used the configured discovery provider
and Agents-A1 model, the harness system prompt and tool schemas, 250 synthetic
records, a stable per-run `prompt_cache_key`, and `max_tokens: 128`. No tools
were executed. The generated answer was not fed into later requests.

The first five requests were byte-identical. The next five started from that
same request and appended one synthetic assistant tool call and tool result
after each request. Therefore append #1 is also identical to repeat #5;
append #2–#5 are true prefix extensions. Endpoint, model, cache key, system,
tools, and remaining settings stayed fixed across all ten requests.

| Request | Relation | Prompt tokens | Cached tokens | Duration, ms |
| --- | --- | ---: | ---: | ---: |
| repeat 1 | baseline | 8398 | 0 | 2004 |
| repeat 2 | identical | 8398 | 0 | 1575 |
| repeat 3 | identical | 8398 | 0 | 2297 |
| repeat 4 | identical | 8398 | 8320 | 1302 |
| repeat 5 | identical | 8398 | 8320 | 1428 |
| append 1 | identical | 8398 | 8320 | 1155 |
| append 2 | append | 8455 | 8320 | 91547 |
| append 3 | append | 8512 | 0 | 3050 |
| append 4 | append | 8569 | 0 | 1873 |
| append 5 | append | 8626 | 0 | 91789 |

All requests succeeded and explicitly reported their cached-token counter;
none of the zeros is a missing-usage placeholder. The two phases reported
39.63% and 39.10% cached input tokens respectively, with two positive hits
in each group of five. A positive hit reused about 98–99% of its request.
Durations measure the entire request, including output, not time to first token.

This establishes that the endpoint supports cache reads and that stable wire
prefixes alone do not guarantee a hit on this endpoint. The client trace cannot
identify server routing, eviction, cache admission, or provider tokenization;
this sample does not distinguish those explanations. It also does not establish
the hit rate or total cost of a real long tool run. Rewrites from pruning and
turn context remain separate client-side issues to address.

Content-free request traces, including hashes and returned usage, are saved in
[the JSONL evidence](cache-control-2026-11-15.jsonl). Credentials and prompt
contents are not recorded.

To repeat deliberately (makes ten paid provider calls; normal test runs skip it):

```sh
GMH_CACHE_PROBE_CONFIG="$HOME/.minimal-harness/config.json" \
GMH_CACHE_PROBE_OUTPUT=/tmp/gmh-cache-control-new.jsonl \
go test ./internal/agent -run '^TestLiveCacheControl$' -count=1 -v
```

The output path must not already exist. Config selection uses its default
provider and model; this probe requires an explicit `baseUrl`, model and the
chat API. It does not apply CLI endpoint/model environment overrides. It uses
the provider's key and resolved headers without logging them.
