# JSON mode

`dashy json [--config FILE]` drives the same controller as the TUI over
newline-delimited JSON: one request per stdin line, one result or event per
stdout line. Diagnostics go to stderr. End of stdin waits for the active turn,
then quits and saves the session.

## Requests

Every request has a client-chosen `id` and a `type`, and gets exactly one
`result`. Controller events follow in emission order.

| type | fields | result `data` |
|---|---|---|
| `input` | `text`, `images` (PNG/JPEG paths), `files` (paths, sent as `@path`) | - |
| `stop` | | - |
| `recall` | | `{text}`: newest queued input, removed from the queue |
| `quit` | | `{closed}`; false while the active turn is cancelled |
| `new` | | - (idle only; a `session` event follows) |
| `resume` | `session` | - (idle only; a `session` event follows) |
| `sessions` | | `{sessions: [{id, title, updated}]}` |
| `rename` | `text` (empty resets) | - |
| `reload` | | - |
| `history` | | `{id, title, context_tokens, records}` (idle only) |
| `models` | | `{models: [{provider, model}], failures: {provider: text}}` |
| `select_model` | `provider`, `model`, optional `effort` (defaults to `medium`) | - |
| `config` | | effective configuration without secrets |
| `info` | | `{session, title, project, db_path, provider, model, effort, configs, messages, requests, tasks, context_tokens, usage: {input_tokens, output_tokens, cached_tokens}}` (available during a turn; `configs` are the existing layers in merge order) |
| `tasks` | | `{tasks: [{id, command, status, started, exit_code}]}` |
| `show_task` | `task` | `{output}`: tail of the task log |
| `stop_task` | `task` | - |
| `status` | | `{busy, cancelling, started, queued, tasks}` |

```json
{"id":"1","type":"input","text":"explain main.go","files":["main.go"]}
{"type":"result","id":"1","ok":true}
{"type":"result","id":"2","ok":false,"error":{"code":"busy","message":"..."}}
```

Error codes: `busy` and `setup_required` are expected states (retry later or
configure dashy); `invalid` is a bad request; `failed` is everything else.

## Events

`{"type":"event","event":KIND,...}`:

- `session`: `session` = `{id, title, context_tokens, records}`; sent at
  startup and after `new`/`resume`.
- `started`: a turn began; `text`, `title`.
- `agent`: `agent` = `{kind, text, id, final, preview, meta}`. Kinds include
  `user`, `delta`, `reasoning_delta`, `assistant`, `reasoning`, `code`,
  `output`, `notice`. Records with the same `id` update one entry: deltas
  append, `final` replaces.
- `done`: `status` (`ok`, `error`, `cancelled`), `text` (final answer),
  `error`, `unsent` (queued texts the model never saw), `title`,
  `context_tokens`.
- `notice`, `error`: one-off messages in `text`. When no provider answers the
  model listing at start, an `error` event carries a setup guide and dashy
  exits with status 1.

`meta` holds `duration_ms`, `input_tokens`, `output_tokens`, `cached_tokens`,
`context_tokens` when known.
