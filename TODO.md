# TODO — Zel 0.7.5

Approved release plan: [Performance and simplification](docs/PLAN-0.7.5.md).
Audit: [2026-11-16 findings and measurements](docs/AUDIT-2026-11-16.md).

- [x] Approve the 0.7.5 plan before implementation.
- [ ] Add sanitized performance baselines and distinguish initial render from warmed refresh.
- [ ] Remove intermediate command previews; keep complete commands and execution semantics.
- [ ] Store new image previews as file-backed references; read legacy chats without migration.
- [ ] Cache context accounting and prepare only changed persistence data.
- [ ] Reduce streaming render, path-check, redraw, and live-menu work without changing formatting.
- [ ] Remove dead implementations and split overloaded responsibilities without new frameworks.
- [ ] Fix slow controller critical sections and bound asynchronous-hook shutdown.
- [ ] Verify old/new/mixed histories, cancellation, background tasks, durability, and measured improvements.
- [ ] Update documentation and release notes; version bump and publication only after approval.

Out of scope: Rex helpers, historical image migration, unapproved feature removals.

## Completed — 0.7.4

- [x] Remove automatic project/global script-folder handling and the example bin folder; use explicit script paths or the normal PATH instead.

- [x] Add a smooth pulsing effect to queued steer messages: fade darker and back while the message is waiting in the queue; stop the animation once it leaves the queue.
- [x] Update the bash tool description to instruct the agent to write large files in chunks using `zel edit`, rather than writing the entire file at once.
