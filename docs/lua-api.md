# zel → Lua: API (финальный черновик)

0.5 — breaking release: bash tool и ZEL.md удалены.

Принято: только Lua (shell — только `sh()`), свежее окружение на каждый вызов + явный `state`, всё блокирует кроме
`tasks.spawn`/`agents.spawn`, сабагенты (`agents.*`) — ядро 0.5, свои функции регистрируются в `init.lua` и сами
попадают в системный промпт, полное доверие (без песочницы, trust-промптов и политик). Рантайм: gopher-lua (pure Go,
`SetContext` для таймаута), вся stdlib открыта (`os`, `io` тоже), `package.path` += `~/.zel/lua/?.lua;.zel/lua/?.lua`.

## 1. Tool и модель вызова
Модели доступен ровно один tool `lua`; payload — сырой Lua-код (транспорт и опции — ниже).
- Каждый вызов — новый `LState`: глобалы и `require`-кэш не переживают вызов, между вызовами — только `state.*`.
- Вывод = `print()` + `return` (таблицы → компактный JSON), бюджет 8192 B head/tail. `sh().out` внутри Lua полный (кап 8 MiB).
- Таймаут вызова 120s (макс 600, `--! timeout=N`) — один дедлайн на весь скрипт, `sh{timeout}` и `h:wait()` обрезаются до остатка.
- В фоне живут только таски и сабагенты, переживают конец вызова. Незабранный результат приходит сообщением
  `<task-result id=…>` / `<agent-result id=…>`.
- Юзер пишет во время вызова → сообщение встаёт в очередь и уходит модели после конца вызова. Вызов не прерывается:
  фоновых тасков тут нет, агент буквально ждёт результат. Esc → отмена (`ECANCELED`), спавненный фон продолжает работать.
- CLI `zel tasks/view/-p` остаётся для людей, в промпте его нет.

### Формат tool-входа (универсальный)
- Канон один: вызов = текст Lua-кода. Опции — прагма первой строкой, `--! timeout=300` (обычный Lua-комментарий,
  неизвестный ключ → `EINVAL`). Других полей нет.
- Транспорт выбирает адаптер провайдера по capability: free-form custom tool (сырой текст, без экранирования) там,
  где есть (Responses), иначе JSON function `{code: string}`. Оба нормализуются в один `Call{code, opts}` до рантайма.
- Новый провайдер = только адаптер транспорта; промпт, хуки (`args = {code}`), логи и сессии от формата не зависят.

### Модель ошибок
- Нормальная неудача — значение: `sh`, `web.*`, `tasks.run`, `agents.run`, `h:wait` дают `ok=false` (+`code`/`status`/`error`).
- Всё остальное (неверные аргументы, нет файла, лимиты, `check=true`, таймаут вызова) — Lua `error()` строкой вида
  `"<fn>: <CODE>: <msg>"`, ловится `pcall`. Коды: `EINVAL ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED`.
  Пример: `local ok, err = pcall(fs.read, "x")` → `false, "fs.read: ENOENT: x"`; `err:find(": ENOENT:")`.
- Необработанная ошибка → в tool-output: напечатанное до этого + `error: <msg>` + короткий traceback, флаг is_error.

## 2. Host API (полный список)
```lua
sh(cmd, o?) -> {ok, code, out, timed_out, ms}  -- bash -c, своя process group; out = stdout+stderr; таймаут → 124
  -- o: timeout (s), cwd, env {K=V}, stdin, check (error EFAIL при code~=0)
-- файлы (пути относительно session.cwd)
fs.read(path, o?) -> string            -- o: from (строка, 1-based), lines; ENOENT
fs.write(path, s, o?) -> nil           -- o: append; родительские папки создаются
fs.edit(path, old, new, o?) -> n       -- o: all; ENOMATCH (0 совпадений), EAMBIG (>1 без all)
fs.list(dir) -> {name, dir, size}[]
fs.glob(pattern) -> string[]           -- "internal/**/*.go"
fs.grep(re, o?) -> {file, line, text}[] -- o: path, glob, fixed, max (default 200); rg, иначе Go regexp
fs.exists(path) -> boolean, "file"|"dir"|nil
fs.tmp(name?) -> string                -- уникальный путь в tmp сессии
-- сеть
web.request{url, method?, headers?, body?, json?, timeout?} -> {ok, status, headers, body, error?}
web.get(url, o?) / web.post(url, o?)   -- сахар; ok = 2xx; сетевая ошибка → ok=false, status=0
json.encode(v, o?) -> string; json.decode(s) -> value   -- o: indent; decode: EINVAL
-- фон: таски
tasks.spawn(cmd, o?) -> Handle         -- o: name, cwd, env, timeout (s, потом kill)
tasks.run(cmd, o?) -> TaskResult       -- spawn + wait; в отличие от sh виден в панели тасков, лог сохраняется
tasks.get(id) -> Handle|nil; tasks.list() -> {id, name, cmd, state}[]  -- get: handle в следующем вызове
-- фон: сабагенты — ядро 0.5, на существующем механизме zel (дочерний zel -p, родитель/глубина, панель, лог)
agents.spawn{prompt, model?, effort?, provider?, cwd?, timeout?} -> Handle
agents.run{...} -> AgentResult         -- spawn + wait
agents.get(id) -> Handle|nil; agents.list() -> {id, model, state, prompt}[]
wait_all(handles, timeout?) -> results[]  -- по индексам; nil у тех, кто не успел
-- Handle
h.id, h.kind                           -- "task" | "agent"
h:status() -> "running"|"done"|"failed"|"stopped"|"timeout"
h:wait(timeout?) -> result | nil, "timeout"  -- таймаут ожидания НЕ убивает фон
h:result() -> result|nil               -- без блокировки
h:tail(n?) -> string                   -- последние n строк лога / последний текст агента
h:stop() -> nil                        -- SIGTERM группе, через 5s SIGKILL
-- TaskResult  = {id, ok, code, out, log, timed_out, ms}   out = хвост 64 KiB, log = путь к полному логу
-- AgentResult = {id, ok, answer, session, error?, ms}
-- состояние между вызовами
state.get(key, scope?) -> value|nil
state.set(key, value, scope?) -> nil   -- value=nil удаляет
state.list(prefix?, scope?) -> string[]  -- ключи, отсортированы
-- прочее
view(path, ...) -> nil                 -- картинки уйдут в следующий запрос
session.{id, cwd, root, parent, depth, model, effort}; session.rename(name)
log.info/warn(msg) -- только Tracing; log.notice(msg) -- уведомление в UI, модели не идёт; sleep(sec)
```

### state
- scope `"session"` (по умолчанию): живёт с сессией, переживает compaction, resume и рестарт zel.
- scope `"project"`: ключ = `session.root` (git root, иначе cwd), общий для всех сессий и сабагентов проекта.
  Сабагент своего session-state не наследует — передавать через prompt или project-scope.
- Значения JSON-совместимые (без функций/циклов). Лимиты: ключ ≤ 256 B, значение ≤ 64 KiB, scope ≤ 1 MiB → иначе `ELIMIT`.
- Хранение в `zel.db`, запись атомарна на ключ, последний писатель выигрывает.

## 3. Plugin API (`init.lua`)
`init.lua` выполняется раз на сессию в долгоживущей config VM: там хуки и тела плагинных функций. Агентский код зовёт их
через мост, аргументы/результаты копируются как JSON-значения. В init.lua доступен весь host API.
```lua
local zel = require("zel")
zel.instructions(text | {file = "AGENTS.md"})   -- статичный текст промпта
zel.prompt(function(ctx) return "..." end)       -- динамический кусок, считается на старте сессии и при Reload
zel.defaults{provider=, model=, effort=, timeout=, output_limit=}  -- CLI-флаги выше
zel.fn{name, summary, params?, returns?, examples?, hidden?, run}  -- функция для агента
zel.on(event, function(ev, ctx) end)             -- хук
```

### Перечитывание: snapshot + Reload
- Слои init.lua читаются раз на старте сессии (и при resume) → снапшот: инструкции, prompt, defaults, fn, хуки.
  Правка файла на живую сессию не влияет.
- Reload (меню Ctrl+K → Reload) — новая config VM, слои заново, промпт пересобирается. Во время хода — применится после
  его конца. Ошибка загрузки → остаётся старый снапшот + `log.notice`.

### zel.fn
```lua
zel.fn{
  name     = "web.search",            -- "ns.name" или "name"; встроенные имена перекрыть нельзя
  summary  = "DuckDuckGo search via ddgs",
  params   = {{"q", "string"}, {"n", "integer?", "max results", default = 5}},  -- позиционные, по порядку
  returns  = "{title,url,body}[]",
  examples = {'web.search("gopher-lua context", 3)'},
  run      = function(q, n) ... end,  -- аргументы уже проверены, default подставлен
}
```
Типы: `string integer number boolean table any`, `T[]`, `{k:T}`, `"a"|"b"`, `?` = необязательный. Host проверяет аргументы
до вызова (`web.search: EINVAL: arg #1 q: expected string, got nil`), результат — нет. `error()` из `run` летит агенту
(без кода — префикс `<name>: EFAIL:`). `hidden = true` — есть, но в промпт не рендерится.

### Генерация промпта
Форма: компактная сигнатура + doc-строки `--` (в 2–3 раза дешевле LuaLS `---@param`, модель читает как Lua):
`name(p: T, p2?: T=default) -> R  -- summary`, затем `--   p: desc` (только если есть) и `--   e.g. …` на пример.
Группы: `## built-in` (фиксированный текст из бинаря), затем `## <слой> (путь)`. Заменяет `bash.md`, пересобирается с промптом.

## 4. Хуки
| event | ev | вернуть |
|---|---|---|
| `session_start` | `{}` | ничего (побочные эффекты: прогреть кэш, проверить окружение) |
| `message` | `{text}` | nil или новую строку текста юзера |
| `before_call` | `{fn, args}` | nil / `{args = новые}` / `{deny = "причина"}` → в агента летит `error("<fn>: EDENIED: …")` |
| `after_call` | `{fn, args, result, err, ms}` | nil / `{result = новый}` |
| `turn_end` | `{answer}` | nil / `{inject = "текст"}` → добавляется user-сообщение, цикл продолжается |
| `bg_done` | `{kind, id, result}` | nil / `{text = "…"}` (переписать `<task-result>`) / `{drop = true}` |

`before/after_call` — на каждый host- и плагинный вызов (`args` = массив позиционных аргументов) и один раз на весь
вызов с `fn = "lua"`, `args = {code}`. `ctx = {session, layer}`. Ошибка в хуке → warning в Tracing, считается nil.

## 5. Слои и слияние
Порядок: `~/.zel/init.lua` → ближайший `.zel/init.lua` вверх от cwd → `--config FILE` (только `.lua`).
- `instructions`/`prompt`: склеиваются по порядку.
- `defaults`: позднее перекрывает; CLI-флаги выше всех.
- `fn`: одинаковое имя — проектная перекрывает глобальную (warning в Tracing); встроенные не перекрываются.
- хуки: выполняются все, глобальные первыми; `args`/`result` передаются по цепочке; первый `deny` останавливает.
- ошибка загрузки слоя: слой пропускается, `log.notice` в UI, сессия стартует.

## 6. Примеры

### 6.1 Глобальный `~/.zel/init.lua` — инструкции + дефолты
```lua
local zel = require("zel")
zel.defaults{model = "sonnet-5.5", effort = "medium"}
zel.instructions [[
Read code first. Work read-only unless the request needs edits. Verify before the final answer.
Reply like an SMS: short, lowercase, user's language, no trailing dot.
Subagents: cheap work → haiku-5.5, real code → sonnet-5.5, review/hard → opus-5.5.
]]
zel.prompt(function() return "env: " .. sh("uname -srm").out .. "today: " .. os.date("%Y-%m-%d") end)
zel.on("session_start", function() sh("mkdir -p ~/.zel/cache") end)
```

### 6.2 Глобальный плагин — документированные `web.search` / `web.fetch`
В ядре их нет (там только сырой `web.request/get/post`): поиск и fetch — плагин в `~/.zel/init.lua`, вот такой.
```lua
local zel = require("zel")
zel.fn{
  name = "web.search", summary = "DuckDuckGo search via ddgs",
  params = {{"q", "string"}, {"n", "integer?", default = 5}},
  returns = "{title,url,body}[]",
  examples = {'for _, h in ipairs(web.search("gopher-lua context", 3)) do print(h.url) end'},
  run = function(q, n)
    local out = fs.tmp("ddgs.json")
    sh(("ddgs text -q %q -m %d -nc -o %s"):format(q, n, out), {check = true, timeout = 30})
    local hits = {}
    for i, h in ipairs(json.decode(fs.read(out))) do hits[i] = {title = h.title, url = h.href, body = h.body} end
    return hits
  end,
}
zel.fn{
  name = "web.fetch", summary = "page as markdown (trafilatura, fallback crwl)",
  params = {{"url", "string"}, {"max", "integer?", "cut to N bytes", default = 20000}},
  returns = "string",
  run = function(url, max)
    local r = sh(("trafilatura -u %q --markdown --no-comments --links"):format(url), {timeout = 40})
    if not r.ok or r.out == "" then r = sh(("crwl crawl %q -o markdown"):format(url), {check = true}) end
    return r.out:sub(1, max)
  end,
}
```
Сгенерированная секция системного промпта (целиком, с встроенной частью):
```text
# Lua
One tool: lua (raw Lua; optional first line --! timeout=N). Fresh env per call; only state.* persists. Output = print() + return (JSON), 8 KB head/tail: filter in Lua.
Errors raise "fn: CODE: msg" (pcall); sh/web/tasks.run/agents.run/h:wait return ok=false instead. Everything blocks.
tasks.spawn/agents.spawn → Handle; unwaited results arrive later as <task-result>/<agent-result>.
## built-in
sh(cmd, o?) -> {ok,code,out,timed_out,ms}  -- o: timeout,cwd,env,stdin,check
fs.read(path, o?) -> string  -- o: from,lines
fs.write(path, s, o?)  -- o: append
fs.edit(path, old, new, o?) -> n  -- exact match; o: all
fs.list(dir) -> {name,dir,size}[]; fs.glob(pat) -> string[]; fs.exists(path) -> bool,kind; fs.tmp(name?) -> path
fs.grep(re, o?) -> {file,line,text}[]  -- o: path,glob,fixed,max
web.get(url, o?) / web.post(url, o?) / web.request{url,method,headers,body,json,timeout} -> {ok,status,headers,body,error}
json.encode(v, o?) / json.decode(s)
tasks.spawn(cmd, o?) -> H; tasks.run(cmd, o?) -> {ok,code,out,log,timed_out}; tasks.get(id); tasks.list()
agents.spawn{prompt,model,effort,timeout} -> H; agents.run{...} -> {ok,answer,session,error}; agents.get(id); agents.list()
H: h.id h:status() h:wait(timeout?) h:result() h:tail(n?) h:stop(); wait_all(hs, timeout?) -> results[]
state.get(k, scope?) / state.set(k, v, scope?) / state.list(prefix?, scope?)  -- scope: "session"|"project"; JSON values
view(path, ...); session.{id,cwd,root,depth}; log.info/notice(msg); sleep(s)
## global (~/.zel/init.lua)
web.search(q: string, n?: integer=5) -> {title,url,body}[]  -- DuckDuckGo search via ddgs
--   e.g. for _, h in ipairs(web.search("gopher-lua context", 3)) do print(h.url) end
web.fetch(url: string, max?: integer=20000) -> string  -- page as markdown (trafilatura, fallback crwl)
--   max: cut to N bytes
```
Порядок ~550 токенов на весь API; каждая плагинная функция — 1–3 строки.

### 6.3 Проект `repos/zel/.zel/init.lua` — инструкции, функция проекта, хуки, state
```lua
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.prompt(function() return "git status:\n" .. sh("git status --short").out end)

zel.fn{
  name = "zel.check", summary = "gofmt + vet + race tests, returns only failures",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  returns = "{ok:boolean, failures:string[]}",
  examples = {'local r = zel.check("./internal/agent/...")'},
  run = function(pkg)
    local fails = {}
    local function add(out, pat)
      for l in out:gmatch("[^\n]+") do if not pat or l:find(pat) then fails[#fails + 1] = l end end
    end
    add(sh("gofmt -l .").out)                                   -- любой вывод = неотформатированный файл
    local v = sh("go vet " .. pkg); if not v.ok then add(v.out, "%.go:") end
    local t = sh("go test -race -count=1 " .. pkg, {timeout = 300}); if not t.ok then add(t.out, "FAIL") end
    state.set("last_check", {ok = #fails == 0, at = os.time()}, "project")
    return {ok = #fails == 0, failures = fails}
  end,
}

zel.on("before_call", function(ev)
  if ev.fn == "sh" and ev.args[1]:match("^go test") and not ev.args[1]:find("-race") then
    ev.args[1] = ev.args[1]:gsub("^go test", "go test -race")
    return {args = ev.args}
  end
end)
zel.on("turn_end", function()
  if sh("git status --porcelain").out ~= "" then
    return {inject = "uncommitted changes: run zel.check() and commit per AGENTS.md"}
  end
end)
```
Добавит в промпт:
```text
## project (repos/zel/.zel/init.lua)
zel.check(pkg?: string="./...") -> {ok:boolean, failures:string[]}  -- gofmt + vet + race tests, returns only failures
--   pkg: go package pattern
--   e.g. local r = zel.check("./internal/agent/...")
```

## 7. Implementation notes (deviations and details of 0.5.0)
- Cancelling: the TUI has no Esc-cancel for a running turn; **Ctrl+D** (stop the active turn) cancels the running
  call (`ECANCELED`). Background tasks and agents keep running, as designed.
- `turn_end` may continue a turn at most 3 times (`inject`), so a hook that always injects cannot loop forever.
- A queued user message is also passed through `message` hooks when it is accepted into the history.
- `agents.spawn` starts a child `zel -p PROMPT` (env `ZEL_DEPTH`+1, parent session via `ZEL_PARENT`, answer file
  `ZEL_REPORT`). Nesting is capped at depth 3. The child is visible in the task panel like any task.
- `ZEL_REPORT` is cleared for everything else, so `sh("zel -p ...")` does not overwrite a parent's report.
- Reload with a broken layer keeps the previous snapshot completely (not layer by layer) and raises a notice;
  the first load at session start skips only the broken layer.
- Tracing kind of a call is `lua` (search `:lua text`). The output-limit and timeout settings in the UI control the lua tool
  (`--! timeout=N` caps at 600).
- `ZEL.md` is neither read nor migrated; the repository itself uses `.zel/init.lua` + `AGENTS.md`.
- Ready-made examples (global config, web plugin, project config) are in `docs/examples/` and covered by tests.
