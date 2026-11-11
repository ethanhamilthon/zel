# Zel roadmap

## Rebranding and repository

- [x] Replace the existing project at `~/Projects/zel` with this project's files, excluding `.git`, and initialize a fresh Git repository without a remote.
- [x] Rename Dashy to Zel throughout the binary, Go module/imports, UI, CLI, environment variables, data paths, build scripts, and documentation.
- [x] Make `make install` install `zel`; use fresh data under `~/.zel` and `ZEL_HOME`, with an isolated development home.
- [x] Do not provide backward compatibility or migrate old Dashy data. Leave existing user data untouched.

## Settings and onboarding

- [x] Remove the `setup` command and TOML configuration system. Store application settings and providers in SQLite and manage them through the UI.
- [x] Show onboarding inside the app when no provider is configured, using the menu's centered visual style:
  - `Welcome to Zel [version]` and `Let's add your first provider to get started`.
  - Four steps: Name, Base URL, API key, API type (`Responses` / `Chat Completions`).
  - Show the current field and step counter (`1/4`), with the standard left-rail input.
  - Left/Right move the input cursor; Tab/Shift+Tab switch steps; Enter advances.
  - Show navigation hints at the bottom of the screen.
  - Validate inputs, then open model and effort selection immediately.
- [x] Add a Providers menu to add, edit, and delete providers.
- [x] On model-list fetch failure, offer retry and manual model entry.
- [x] Remove event visibility toggles such as `bash_output = false`; non-chat events belong in Tracing.
- [x] Expose retained application settings through the UI, including editor and output limits.

## ZEL.md and project identity

- [x] Replace `dashy.toml` with Markdown containing YAML front matter:

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

- [x] Load `~/.zel/ZEL.md`, then `ZEL.md` files from the filesystem root down to the working directory, then the file passed through `--config <path>`.
- [x] Combine prompt bodies and hooks across layers; later metadata overrides earlier metadata. Hooks for the same event accumulate rather than replace one another.
- [x] Provide 20 named, calm accent colors selectable through `color`.
- [x] Use one accent throughout the application, neutral colors for other UI elements, and red for errors; remove independent semantic/theme accent colors.
- [x] Show project name and description in Info, and use `zel [version]/<name>` as the main menu header.

## Hooks

- [x] Define hooks in ZEL.md by event, with these initial events:
  - `app_start`, `app_exit`
  - `session_start`, `session_close`
  - `first_message`, `user_message`
  - `turn_start`, `turn_end`
  - `bash_start`, `bash_end`
  - `model_request_start`, `model_request_end`
  - `error`
- [x] Fire `first_message` only for the first user message in the entire session, not again on resume.
- [x] Fire `session_close` when switching sessions and when exiting the application.
- [x] Pass `ZEL_EVENT` and only relevant event data through environment variables, such as session ID, context usage, and the latest message when applicable. Do not dump the full history or secrets into the environment.
- [x] Inject nonempty stdout only when the hook exits successfully:
  - Before a model request, wait for hooks and include their output in context.
  - During an active turn, deliver output as steering.
  - After a turn ends, retain output for the next user-triggered turn.
  - Never start a new turn because of hook output.
  - For closing events, record output in Tracing only.
- [x] Use a 10-second hook timeout. Hook failures must not interrupt normal operation; record errors and stderr in Tracing.
- [x] Wait for closing hooks within their timeout before exiting.
- [x] Apply the same output limit and truncation mechanism to Bash and hook output.
  - The configured byte budget is split between the beginning and end, with a truncation marker between them.
  - Example: 4 KiB means the first 2 KiB + truncation marker + last 2 KiB; the marker is additional to the retained output budget.
  - Bash already implements head/tail truncation; reuse it for hooks and verify both paths.

## Tracing

- [x] Support a leading type filter in search, such as `:user`, `:assistant`, or `:bash`.
- [x] Apply the remaining search text within that type, e.g. `:user fix tests`.
- [x] Support every existing Tracing entry type and preserve ordinary search without a prefix.

## Files menu

- [x] Add Files to the main menu, initially listing the working directory.
- [x] Include hidden entries, `.` and `..`; list directories first and provide search consistent with other menus.
- [x] Enter opens a directory in the browser or a file in the configured external editor, defaulting to `nvim`.
- [x] Reuse the existing terminal/editor handoff and return to the browser after editing.

## Verification

- [x] Update docs and focused unit tests for the new behavior; keep English UI, docs, and comments.
- [x] Run `make test` before committing implementation changes.

## Resolved decisions

- The original `~/Projects/dashy` source directory is retained as a safety copy; active development is in `~/Projects/zel` with fresh Git history and no remote.
- The default accent is `terracotta`.
- The previous 10,000-line source cap has been removed from project rules and CI.
