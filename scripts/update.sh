#!/bin/sh
# Update zel to the latest release and move an older ~/.zel to the 0.9 layout.
# Usage: sh scripts/update.sh
#    or: curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/zel/main/scripts/update.sh | sh
# Optional: ZEL_HOME=~/.zel, ZEL_VERSION=v0.9.0, ZEL_BINDIR=~/.local/bin,
#           ZEL_SKIP_INSTALL=1 (only move the data), ZEL_CLEAN_ATTACHMENTS=1 (delete old image copies)
set -eu

home="${ZEL_HOME:-$HOME/.zel}"
repo="${ZEL_REPO:-ethanhamilthon/zel}"

say() { printf '%s\n' "$*"; }

# 1. The binary.
if [ -z "${ZEL_SKIP_INSTALL:-}" ]; then
	here=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)
	if [ -n "$here" ] && [ -f "$here/install.sh" ]; then
		sh "$here/install.sh"
	else
		curl -fsSL "https://raw.githubusercontent.com/$repo/main/scripts/install.sh" | sh
	fi
fi

# 2. The data: ~/.zel/config.toml, ~/.zel/.sessions/ and a .gitignore.
mkdir -p "$home"
if [ -f "$home/zel.toml" ]; then
	if [ -e "$home/config.toml" ]; then
		say "kept $home/zel.toml: $home/config.toml already exists, merge them by hand"
	else
		mv "$home/zel.toml" "$home/config.toml"
		say "moved zel.toml to config.toml"
	fi
fi
if [ -d "$home/sessions" ]; then
	if [ -e "$home/.sessions" ]; then
		say "kept $home/sessions: $home/.sessions already exists, merge them by hand"
	else
		mv "$home/sessions" "$home/.sessions"
		say "moved sessions/ to .sessions/"
	fi
fi
# Sockets of sessions that run now stay; stale ones go.
if [ -d "$home/run" ] && ! pgrep -x zel >/dev/null 2>&1; then
	rm -rf "$home/run"
	say "removed the stale run/ folder (sockets are in .sessions/run now)"
fi
# Images are not stored any more.
old=$(find "$home/.sessions" "$home/attachments" -type d -name attachments 2>/dev/null || true)
[ -d "$home/attachments" ] && old="$old
$home/attachments"
old=$(printf '%s\n' "$old" | sed '/^$/d' | sort -u)
if [ -n "$old" ]; then
	if [ -n "${ZEL_CLEAN_ATTACHMENTS:-}" ]; then
		printf '%s\n' "$old" | while IFS= read -r dir; do rm -rf "$dir"; done
		say "deleted old image copies; old chats show a placeholder instead of the images"
	else
		say "old image copies are not used any more (run again with ZEL_CLEAN_ATTACHMENTS=1 to delete them):"
		printf '%s\n' "$old" | sed 's/^/  /'
	fi
fi
for line in .env .sessions/; do
	grep -qxF "$line" "$home/.gitignore" 2>/dev/null || printf '%s\n' "$line" >>"$home/.gitignore"
done

# 3. What changed in the config, for the file that is there.
if [ -f "$home/config.toml" ]; then
	grep -n 'zel\.toml' "$home/config.toml" | sed 's/^/  config.toml:/' >/tmp/zel-update-$$ || true
	if [ -s /tmp/zel-update-$$ ]; then
		say "config.toml still mentions zel.toml (the file is config.toml now):"
		cat /tmp/zel-update-$$
	fi
	rm -f /tmp/zel-update-$$
fi
say "done. A ./zel.toml in a project is not read any more: move it into $home/config.toml."
say "Hooks get config_changed with \"file\" (one path) instead of \"files\"; ZEL_CONFIG is one path."
