# Zel roadmap

## Rebranding and repository

- [ ] Replace the existing project at `~/Projects/zel` with this project's files, excluding `.git`, and initialize a fresh Git repository without a remote.
- [ ] Rename Dashy to Zel throughout the binary, Go module/imports, UI, CLI, environment variables, data paths, build scripts, and documentation.
- [ ] Make `make install` install `zel`; use fresh data under `~/.zel` and `ZEL_HOME`, with an isolated development home.
- [ ] Do not provide backward compatibility or migrate old Dashy data. Leave existing user data untouched.

## Settings and onboarding

- [ ] Remove the `setup` command and TOML configuration system. Store application settings and providers in SQLite and manage them through the UI.
- [ ] Show onboarding inside the app when no provider is configured, using the menu's centered visual style:
  - `Welcome to Zel [version]` and `Let's add your first provider to get started`.
  - Four steps: Name, Base URL, API key, API type (`Responses` / `Chat Completions`).
  - Show the current field and step counter (`1/4`), with the standard left-rail input.
  - Left/Right move the input cursor; Tab/Shift+Tab switch steps; Enter advances.
  - Show navigation hints at the bottom of the screen.
  - Validate inputs, then open model and effort selection immediately.
- [ ] Add a Providers menu to add, edit, and delete providers.
- [ ] On model-list fetch failure, offer retry and manual model entry.
- [ ] Remove event visibility toggles such as `bash_output = false`; non-chat events belong in Tracing.
- [ ] Expose retained application settings through the UI, including editor and output limits.

## ZEL.md and project identity

- [ ] Replace `dashy.toml` with Markdown containing YAML front matter:

  ```markdown
  ---
  name: My project
  description: A short project description
  color: terracotta
  hooks:
    app_start: "./start.sh"
  ---
  Project system prompt goes here.
  ```

- [ ] Load `~/.zel/ZEL.md`, then `ZEL.md` files from the filesystem root down to the working directory, then the file passed through `--config <path>`.
- [ ] Combine prompt bodies and hooks across layers; later metadata overrides earlier metadata. Hooks for the same event accumulate rather than replace one another.
- [ ] Provide 20 named, calm accent colors selectable through `color`.
- [ ] Use one accent throughout the application, neutral colors for other UI elements, and red for errors; remove independent semantic/theme accent colors.
- [ ] Show project name and description in Info, and use `zel [version]/<name>` as the main menu header.

## Hooks

- [ ] Define hooks in ZEL.md by event, with these initial events:
  - `app_start`, `app_exit`
  - `session_start`, `session_close`
  - `first_message`, `user_message`
  - `turn_start`, `turn_end`
  - `bash_start`, `bash_end`
  - `model_request_start`, `model_request_end`
  - `error`
- [ ] Fire `first_message` only for the first user message in the entire session, not again on resume.
- [ ] Fire `session_close` when switching sessions and when exiting the application.
- [ ] Pass `ZEL_EVENT` and only relevant event data through environment variables, such as session ID, context usage, and the latest message when applicable. Do not dump the full history or secrets into the environment.
- [ ] Inject nonempty stdout only when the hook exits successfully:
  - Before a model request, wait for hooks and include their output in context.
  - During an active turn, deliver output as steering.
  - After a turn ends, retain output for the next user-triggered turn.
  - Never start a new turn because of hook output.
  - For closing events, record output in Tracing only.
- [ ] Use a 10-second hook timeout. Hook failures must not interrupt normal operation; record errors and stderr in Tracing.
- [ ] Wait for closing hooks within their timeout before exiting.
- [ ] Apply the same output limit and truncation mechanism to Bash and hook output.
  - The configured byte budget is split between the beginning and end, with a truncation marker between them.
  - Example: 4 KiB means the first 2 KiB + truncation marker + last 2 KiB; the marker is additional to the retained output budget.
  - Bash already implements head/tail truncation; reuse it for hooks and verify both paths.

## Tracing

- [ ] Support a leading type filter in search, such as `:user`, `:assistant`, or `:bash`.
- [ ] Apply the remaining search text within that type, e.g. `:user fix tests`.
- [ ] Support every existing Tracing entry type and preserve ordinary search without a prefix.

## Files menu

- [ ] Add Files to the main menu, initially listing the working directory.
- [ ] Include hidden entries, `.` and `..`; list directories first and provide search consistent with other menus.
- [ ] Enter opens a directory in the browser or a file in the configured external editor, defaulting to `nvim`.
- [ ] Reuse the existing terminal/editor handoff and return to the browser after editing.

## Verification

- [ ] Update docs and focused unit tests for the new behavior; keep English UI, docs, and comments.
- [ ] Keep non-test Go source below 10,000 lines and run `make test` before committing implementation changes.

## Remaining implementation decisions

- Confirm whether the original `~/Projects/dashy` directory should be removed after the move.
- Choose the default accent when no `color` is specified (proposed: terracotta).
