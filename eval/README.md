# Golden-set evaluation: staged vs unified analysis

Local, on-demand harness comparing the two analysis pipelines against a real
LLM endpoint:

- `staged` — two calls per capture: topic extraction, then matching (the
  default).
- `unified` — one call producing topics together with their mindcache matches
  (experimental, `ANALYSE_MODE=unified`).

It never runs in CI — it requires API credentials and spends tokens.

## Running

```bash
EVAL_BASE_URL=https://api.deepseek.com/v1 \
EVAL_API_KEY=your-key \
EVAL_MODEL=deepseek-v4-flash \
go run ./eval -runs 3
```

Options: `-runs N` (default 3), `-modes staged,unified`, `-testdata DIR`.

The collection for each fixture is seeded directly into the database (`repo.Create`
needs no LLM), so only the analysis calls are measured.

## Fixture format

`eval/testdata/*.json`:

```json
{
  "name": "en-multi-topic",
  "chat": {
    "title": "Chat title",
    "messages": [{ "role": "user", "content": "..." }]
  },
  "collection": ["Existing note brief 1", "Existing note brief 2"],
  "expected": [
    { "topic_contains": "docker", "briefs": ["Existing note brief 1"] }
  ]
}
```

- `chat.content` may be omitted; it is then flattened from `messages` using
  the same convention as the browser extension.
- `expected` labels are deliberately conservative: only unambiguous
  assignments are labelled. A run scores one hit per expected entry whose
  topic was extracted **and** matched to all listed briefs.

## Reading the output

Per run: `RESULT` (ok/FAIL — JSON validity incl. the built-in retry),
`CALLS` (LLM calls made), `TIME`, `MATCH` (hits/labelled).
Per fixture/mode: success rate, average calls, average time, match rate.

## Decision criteria for switching the default

Switch `ANALYSE_MODE` to `unified` by default only when, across the fixtures
and at least one cloud model plus one local ~8B model:

1. unified success rate ≥ staged success rate;
2. unified match rate ≥ staged match rate;
3. unified uses ~half the calls (sanity check of the design goal).

Otherwise keep `staged` as the default; `unified` remains available as an
opt-in. Small local models (≤8B) are expected to prefer `staged` — the
multi-task prompt is harder.

## Notes

- Embeddings (`EMBED_*`) are not exercised here: staged runs against the
  full collection, matching how unified behaves (retrieval needs topic
  briefs, which don't exist before the unified call).
- Temperature is fixed server-side (0.3); `-runs > 1` still shows residual
  variance.
