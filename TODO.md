# TODO 0.3.3

- [x] Bump the version to 0.3.3 and prepare GitHub Actions CI/CD to build for macOS and Linux on amd64 and arm64.
- [x] Remove the extra blank line above the input in both empty chats and chats with history.
- [x] Simplify the main menu header to `zel 0.3.3` on the left and `Ctrl+K` on the right, without `/Menu`.
- [x] Show the menu overflow hint as `+ 5 more items`, with `+` aligned to the left edge of the other menu items.
- [x] Update the main menu labels and descriptions:

  | Label | Description |
  | --- | --- |
  | Resume | View all sessions |
  | New | Or just Ctrl+N |
  | Models | And Efforts, Ctrl+M |
  | Tasks | Background tasks |
  | Tracing | Session Details, Ctrl+T |
  | Rename | Rename this session |
  | Info | Tokens, Session, Ctrl+I |
  | Reload | New Color or Prompt |
  | Help | Keyboard shortcuts |
  | Quit | Exit, Ctrl+Q |
  | Editor | Edit input, Ctrl+E |
  | Providers | Add, edit, remove |
  | Settings | Editor, output, timeouts |
  | Files | Browse files, Ctrl+F |
  | Copy | Agent answer to input, Ctrl+Y |

- [x] Make Copy (and Ctrl+Y) insert the latest assistant message directly into the input.
- [x] Add a Ctrl+A shortcut in Files to create a new file or directory, and show a hint at the bottom: `Ctrl+A: New file or directory`.
- [x] Search menu items by label only, using a strict case-insensitive prefix match instead of fuzzy matching; do not search descriptions. For example, `i` should show only Info, not Tracing.
- [x] Update Tracing:
  - Rename the displayed `assistant` type to `agent`.
  - Give each entry type a distinct color: system dark gray, user accent, agent white, bash gray, notice/task between system and bash, and output darker than system.
  - In the fullscreen entry view, limit the text viewport to 10 lines and support scrolling with arrow keys and the mouse.
  - Arrange the entry header as elapsed time on the left, entry type (system/user/agent/etc.) in the middle, and position/total (e.g. `x/193`) on the right. Remove the second line that only showed the entry type.
  - Support combined type filters such as `:bash+user` (bash or user).
  - Add entry-index filters using `<` and `>`, combinable with type filters. For example, `>123 :bash` shows bash entries starting at entry 123 (inclusive).
  - Render Markdown in system, user, and agent entries.
  - Darken the text color of output entries as well, keeping it darker than system text.
- [x] Replace hardcoded `/home/yen` paths throughout the project with portable home-directory handling: display paths under the user's home as `~` and resolve filesystem paths using the current user's actual home directory, not a literal `~`.
- [x] Audit all selectable lists throughout the application and replace background-color selection highlights with the shared arrow indicator (`<`), matching the menus. The current background-highlighted list is the autocomplete file picker (`internal/tui/render.go`, using `selectBackground`); menus already use arrows. Keep non-selection backgrounds unchanged.
