# Offline checkpoint comparison, 2026-11-15

The original cache-audit fixture drives `Config.Run` through the real chat wire
encoder, reads 100 synthetic files, then stops. It uses the harness system and
tool definitions, a fixed session cache key, a 128000-token context limit,
and synthetic usage. It measures message JSON bytes, not provider tokens or
remote cache hits. Neither tool schemas nor the cache key changes.

| Result size | Previous rewrites / 100 | Checkpoint rewrites / 100 | Previous total message bytes | Checkpoint total | Previous nonshared bytes | Checkpoint nonshared |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 256 B | 0 | 0 | 2612061 | 2612061 | 46076 | 46076 |
| 1024 B | 3 | 0 | 3205486 | 6490461 | 146087 | 122876 |
| 5000 B | 23 | 3 | 2875525 | 17232659 | 545698 | 958207 |
| 16000 B | 97 | 11 | 3160194 | 19584063 | 1630360 | 2984588 |
| 40000 B | 99 | 24 | 4793911 | 19150237 | 3311670 | 5699451 |

The old renderer pruned at each new 24 KiB block after total historical tool
content exceeded 48 KiB. Checkpoints instead retain capped observations until
the context budget, prune enough older results to reclaim headroom, and persist
the replacement. This substantially reduces prefix rewrites, but retains more
input. In the larger cases both total and nonshared bytes increase. Thus this
change cannot be presented as a cost reduction. A real workload must compare
reported prompt/cache usage and the provider's actual prices; the endpoint
control already shows that matching prefixes do not ensure a cache hit.

The committed `TestCheckpointKeepsWireRequestsStableBetweenRewrites` separately
uses measured synthetic request sizes, a mock streaming HTTP endpoint and real
read tools: 101 requests produce 12 checkpoints and 88 append-only transitions
for 100 x 16 KiB outputs. Tests also cover checkpoint reload, raw-record size
limits, hydrated images, transient context hooks, and the final grace request.
