# 0.8.0 — tcell interface

## Scope

Replace the Bubble Tea frontend with tcell. Keep the runtime and current user
workflows. Do not build a widget framework or change the provider/tool protocol.
Share only the editor and searchable list, which are used in multiple places.

## Checklist

- [x] Replace terminal lifecycle, event dispatch and drawing; remove the old renderer.
- [x] Implement one Unicode-aware multiline editor for chat, custom choices and forms.
- [x] Preserve streaming chat, cancellation, queued input, recall, tasks and subagents.
- [x] Preserve searchable menus, models/efforts, sessions, rename, info and trace.
- [x] Preserve provider onboarding/settings, live colors, file/prompt completions and images.
- [x] Preserve answered choices, mouse placement, scrolling and copyable text.
- [x] Remove unused dependencies and replace frontend tests with focused unit tests.
- [x] Run make test, exercise an isolated real terminal with a local streaming provider,
      capture evidence and check shutdown/restoration.
- [x] Prepare 0.8.0 for installation and publication. Commit/tag are pushed after
      checks; the release's GitHub Actions run is checked before reporting completion.

## Constraints

Keep all existing repository changes. Never launch test sessions against the user's
live session or overwrite their config. No parallel permanent frontend. Prefer a
small direct implementation to adapters around the old framework. Dependencies
handle terminal escapes and Unicode widths; Zel owns its few concrete UI elements.
