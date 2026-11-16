# TODO — Zel 0.7.5

Approved release plan: [Performance and simplification](docs/PLAN-0.7.5.md).
Audit: [2026-11-16 findings and measurements](docs/AUDIT-2026-11-16.md).

- [x] Approve the 0.7.5 plan before implementation.
- [x] Add sanitized performance baselines and distinguish initial render from warmed refresh.
- [x] Remove intermediate command previews; keep complete commands and execution semantics.
- [x] Store new image previews as file-backed references; read legacy chats without migration.
- [x] Cache context accounting and prepare only changed persistence data.
- [x] Reduce streaming render, path-check, redraw, and live-menu work without changing formatting.
- [x] Remove dead implementations and split overloaded responsibilities without new frameworks.
- [x] Fix slow controller critical sections and bound asynchronous-hook shutdown.
- [x] Verify old/new/mixed histories, cancellation, background tasks, durability, and measured improvements.
- [x] Add custom written answers to choice cards.
- [x] Update documentation, release notes, and version to 0.7.5.
- [ ] Publish tag and verify GitHub CI/release artifacts.

Out of scope: Rex helpers, historical image migration, unapproved feature removals.

## Completed — 0.7.4

- [x] Remove automatic project/global script-folder handling and the example bin folder; use explicit script paths or the normal PATH instead.

- [x] Add a smooth pulsing effect to queued steer messages: fade darker and back while the message is waiting in the queue; stop the animation once it leaves the queue.
- [x] Update the bash tool description to instruct the agent to write large files in chunks using `zel edit`, rather than writing the entire file at once.
