# Zel 0.7.5 — Performance and Simplification Plan

Status: implementation and release authorized on 2026-11-16, including commit, tag, push, and CI verification.

Based on [the performance and architecture audit](AUDIT-2026-11-16.md).

## Agreed product decisions

1. Balance performance improvements with code simplification.
2. Individual feature removals are allowed only after explicit approval. The removal of intermediate command previews is approved; other supported features remain unless separately agreed.
3. Preserve Markdown formatting while assistant text streams. Do not switch to plain-text previews as a shortcut.
4. Store newly submitted image previews in separate files instead of retaining their base64 payloads in session history. Existing chats must continue working.
5. **No migration:** do not bulk-convert or rewrite existing chats. Continue reading the old embedded-image format. Use the new format for new image-bearing messages, including messages appended to an existing chat.
6. Rex helpers and local status/title integration are outside this release. Do not modify `~/.zel/zel.toml` or `~/.zel/bin` as part of this plan.
7. In Tracing, show a generated command only when it is complete. Remove intermediate versions, without changing tool execution or cancellation.
8. Remove dead code and duplication, and separate overloaded responsibilities into clear blocks. Avoid extra layers, tiny wrapper packages, and speculative frameworks.
9. Every choice card also offers a custom written answer. Preserve the normal composer draft, preset options, cancellation, and queued questions.

## Release boundaries

Keep existing CLI commands, both supported provider protocols, cancellation, steering, background tasks, trace fidelity, final command visibility, native terminal selection, and append-only crash recovery. No silent deletion of history or image data. No rewrite of the application or UI framework.

Do not bump `VERSION`, change the installed binary, or publish a release while only planning. Each implementation phase needs reviewable changes and the existing `make test` checks before its commit.

## Delivery sequence

### 1. Establish reproducible baselines

- Turn the useful audit reproductions into small, sanitized benchmarks: generated image histories, long tool calls, streaming Markdown, repeated saves, warm history, initial rendering, and resize.
- Fix the existing timeline benchmark setup so initial draft upgrades and fully warmed refresh are measured separately.
- Measure allocated bytes, live heap, and process footprint separately. Include both a fresh session and a long image-heavy session.
- Cover complete command events for both Responses and Chat Completions before changing their preview handling.

**Done when:** the baseline can be repeated without private session files, a live model request, or the user's actual configuration. Keep at most 1–2 test files per package, as required by project instructions.

### 2. Remove intermediate command previews

- Stop assembling/parsing incomplete command JSON for display; publish the complete command once available.
- Continue assembling the authoritative tool-call arguments needed for execution. Do not discard protocol state or execute incomplete arguments.
- Remove `partialCode` and preview-only buffering if they have no remaining callers.
- Remove the unread freeform argument accumulation; keep only the protocol state actually required.
- Avoid unnecessary typed-event-to-JSON-to-typed-event round trips in the Chat adapter where the same change can be made safely.

**Done when:** Tracing contains complete commands with no intermediate replacements; final commands are visible before execution is started; cancellation and steering still work; both provider modes are covered. No quadratic preview work remains. Long argument delivery must preserve UTF-8, escaping, and exact final script contents.

### 3. Make newly submitted images file-backed, without migration

- Define a small versioned attachment reference for new persisted image parts. Reuse the existing session attachment directory rather than introduce a second storage subsystem.
- Keep the captured original used for crops and the request preview conceptually distinct. Reuse files when their contents and lifetime match; do not assume original and preview bytes are identical.
- Store the preview file durably before committing a history reference. Use atomic writes and a verified path confined to the owning session's attachment storage.
- Materialize encoded image content at the provider boundary for an outgoing request. Do not keep a second encoded copy in persistent in-memory history. Bound temporary request/image buffers and avoid rehydrating images omitted by compaction.
- Keep a dual reader: legacy embedded images remain readable without rewriting; new references work in both newly created and previously existing sessions.
- Check image handling in queued inputs and notifications too, so base64 buffers are released after their handoff rather than accidentally retained.
- Keep trace and chat extraction metadata-only: reading display text must not load image bytes.
- Preserve the current missing-original warning behavior. Define a clear recoverable error when a referenced preview is missing; never silently send a different prompt with the image omitted.

**Done when:** new images do not remain embedded in retained raw history, old chats open unchanged, mixed-format chats can be saved/resumed, both provider protocols receive the intended image, and failed/cancelled writes cannot leave committed broken references. Saved attachments must survive closing a session.

**Compatibility boundary:** an older Zel binary is not guaranteed to understand new attachment references. State that explicitly before rollout; if rollback support becomes a requirement, agree a strategy rather than silently add migration. Legacy embedded images may retain their old memory cost: this release does not promise to optimize old files by rewriting them. Portable chat export is not a new feature in this release, but required attachment files must be documented.

### 4. Stop repeatedly processing unchanged history

- Cache immutable per-item type/role, text token estimates, and image count. Update totals when items are appended, replaced, or compacted.
- Avoid regex replacements over encoded image payloads just to estimate text size. Handle new references without reading the corresponding files.
- Continue honoring actual provider-reported usage; keep fallback estimates distinct from reported counts.
- Prepare only new/dirty persistence items and metadata. Skip writes when neither history nor session metadata changed.
- Preserve save boundaries that protect crash recovery and commit-before-tool-execution behavior.
- Cache cheap session summaries instead of repeatedly decoding history to recount messages or find the first user line.

**Done when:** appending a small item does not trigger a scan of historic image payloads; warmed unchanged saves do not prepare the entire history; compaction, resume, title/model changes, and failed-save retries remain correct.

### 5. Keep formatted streaming, with less work per frame

- Render stable Markdown blocks once and update the changed tail. Preserve final layout, links, and current streaming presentation.
- Cache positive and negative file-path checks within a bounded session/render revision. Avoid repeated filesystem checks for unchanged code spans; make invalidation explicit.
- Separate state changes, layout invalidation, and screen invalidation. Coalesce redraws and skip full rendering for unchanged frames while preserving immediate cursor/input changes.
- Stop processing offscreen draft upgrades ahead of visible work; keep background upgrades interruptible.
- Avoid reconstructing the entire combined history-row index when only the newest entry changes.
- Separate committed chat records, in-flight previews, and trace snapshots so hidden committed tool/reasoning records do not need an unnecessary permanent UI copy.
- Reset session/theme-sensitive caches at the right boundary. Bound caches by reasonable byte budgets where entry count is insufficient.
- Replace repeated full trace/menu reconstruction with revision-based snapshots or incremental tails. Fetch a small immutable UI settings snapshot rather than clone all settings per input event.
- Retain existing directory-local file completion and debounce; do not add a project-wide index without evidence.

**Done when:** formatted answers still stream; unchanged draws do no full-frame formatting or repeated path checks; scrolling, clickable links, resize, long paste, menus, and native terminal selection remain correct. Measure initial load separately from warmed refresh.

### 6. Simplify the actual architecture, not just file names

Prioritize cohesive responsibilities within the existing packages. Splitting files alone is not a performance improvement or an architecture outcome.

- **Shell execution:** remove the unused alternate Runner/Result/Background implementation after moving useful tests to the actual Command/Reap and runtime task-adoption paths.
- **Task lifecycle and output:** consolidate old/new stop and tail helpers. Use bounded suffix reads, not whole-log reads, for current task output.
- **Model loading:** remove the unused session model cache/listing implementation. Keep the active model catalog path and required request deduplication.
- **Runtime:** separate provider stream handling, history/context accounting, and lifecycle assembly into clear blocks with explicit ownership. Do not add a service/container layer or forward every operation through new interfaces.
- **Settings:** distinguish config loading/application, mutable selection, and the read-only snapshot used by the UI. Share truly equivalent leaf helpers without erasing CLI/session-specific layer semantics.
- **TUI:** separate event reduction, chat/preview state, layout preparation, and terminal drawing. Keep one clear owner for each state/cache; avoid dual callback/event paths where existing controller events suffice.
- **Concurrency:** replace deferred unlock/relock tricks with explicit lock scopes. Reserve state transitions under lock, do slow config/hook/persistence work outside it, then publish only if the session/operation generation still matches.
- **Shutdown:** give asynchronous hooks a bounded, cancellable lifecycle; finish or cancel them before removing unsaved session resources. Do not weaken safety hooks or change blocking/rewriting semantics casually.
- **Leaf cleanup:** remove unused helpers/no-op methods and deduplicate path handling, image markers, and trace-kind mappings when this reduces overall complexity.
- **Documentation:** make the dependency diagram and lifecycle descriptions match the resulting code. Add a small boundary test for useful constraints, not for arbitrary alias-only isolation.

**Done when:** dead implementations are gone, useful tests exercise shipped paths, slow work does not hold the controller lock, and the dependency structure remains acyclic. Approximately 250–350 production lines are cleanup candidates from the audit, not a mandatory deletion quota. Necessary attachment code may offset those deletions; do not sacrifice correctness to achieve a smaller total.

### 7. Verify the combined release

- Run `make test`; preserve coverage for both provider adapters, steering/cancellation, background adoption, output bounds, append-only recovery, and queued-event spill/drain.
- Exercise repeated turns, image submission, session switching, missing files, partial writes, slow hooks, reload during activity, and cancellation while a response is streaming.
- Check old-format, new-format, and mixed-format sessions; verify no existing history was rewritten as a side effect.
- Compare fixed workloads before/after on the same machine: command stream processing, image memory retention, context fallback, no-change saves, streaming frames, initial render/resize, and warmed idle refresh.
- Record peak transient allocation, live heap after collection, and OS footprint separately. Check whether memory settles after switching away from a large session; do not claim every increase is a leak.
- Publish a short results table and explicit limitations. Do not invent target multipliers or tune Go memory limits to hide allocation sources.
- Update architecture/reference docs and release notes, then bump to 0.7.5 only after approval of the finished work. Installation and publication need a separate explicit go-ahead.

**Done when:** approved behavior changes are documented, checks pass, measurements demonstrate improvement on the targeted paths, and no unapproved function/configuration changes were bundled into the release.

## Guardrails against scope creep

- No Rex helper or local hook-configuration changes.
- No historical image migration, automatic bulk conversion, or rewrite of existing session files.
- No plain-text streaming fallback in place of the chosen formatted streaming.
- No removal of the UI queue spool without an equivalent bounded-memory and event-order guarantee. Its synchronous I/O may be improved, but deleting the protection is not simplification.
- No removal of provider normalization, process cleanup, stream limits, history durability, or the small agent host boundary merely to reduce line count.
- No global plugin/DI framework, new UI framework, second helper binary, or general storage-engine rewrite.
- No extra supported-feature removals without a separate decision.

## Implementation checkpoints

1. Review the new attachment reference and failure behavior before implementing phase 3. Confirm the minimum rollback expectation: old chats must work in 0.7.5; support for opening new 0.7.5 image records in older binaries has not been agreed.
2. Review responsibility boundaries before large runtime/TUI changes. Prefer extraction of cohesive code over a broad rewrite.
3. Compare each performance patch with the baseline before stacking unrelated refactors on it.
4. If the measured cost or compatibility risk forces a scope change, ask instead of silently weakening the agreed behavior.

## Approval

The user approved the plan and subsequently authorized full implementation and the 0.7.5 release on 2026-11-16, including the custom choice answer, commits, tag, push, and waiting for CI. Local Rex configuration remains out of scope.
