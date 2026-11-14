# zel → Lua: API (финальный черновик)

0.5 — breaking release: bash tool и ZEL.md удалены.

Принято: только Lua (shell — только `sh()`), постоянная main-VM сессии (глобалы живут между вызовами) + явный `state`, всё блокирует кроме
`tasks.spawn`/`agents.spawn`, сабагенты (`agents.*`) — ядро 0.5, свои функции регистрируются в `init.lua` и сами
попадают в системный промпт, полное доверие (без песочницы, trust-промптов и политик). Рантайм: gopher-lua (pure Go,
`SetContext` для таймаута), вся stdlib открыта (`os`, `io` тоже), `package.path` += `~/.zel/lua/?.lua;.zel/lua/?.lua`.

## 1. Tool и модель вызова
Модели доступен ровно один tool `lua`; payload — сырой Lua-код (транспорт и опции — ниже).
- VM живёт между вызовами (0.5.2): глобалы, функции и `require`-кэш остаются. VM сбрасывается на `new`, `resume`, reload
  и после таймаута вызова; следующий вывод один раз начинается с `[lua: fresh VM, <причина>: ...]`. `state.*` переживает и сброс.
- Четыре роли VM, изолированные друг от друга: config-VM (`init.lua`, хуки, тела плагинов), main-VM (вызовы модели),
  task-VM (свежая на каждый `tasks.spawn(fn)`, своя горутина), VM сабагента (своя main-VM; `agents.*` там запрещены → `EDENIED`).
- Вывод = `print()` + `return` (таблицы → компактный JSON), без обрезки (0.5.2; только защитный кап 1 MiB). Фильтровать в Lua:
  `local a = fs.read("bigfile.md"); print(slice(a, 100, 10))`. `sh().out` внутри Lua полный (кап 8 MiB).
- Таймаут вызова фиксирован: 30 s, не настраивается (0.5.2; в 0.5.1 — 10 s, раньше 120 s и `--! timeout=N`). Один дедлайн на весь скрипт и вложенные блокирующие вызовы: `sh{timeout}` и `tasks.wait`/`agents.wait` обрезаются до остатка. Долгая работа — `tasks.spawn`/`agents.spawn` и проверка в следующих вызовах; на дедлайне гибнут бесконечные циклы, а фоновые джобы живут дальше.
- В фоне живут только таски и сабагенты, переживают конец вызова. Незабранный результат приходит
  уведомлением `<notification>t1 finished</notification>` — только короткий текст, детали модель берёт сама (`tasks.result(id)`).
  Агент просыпается только от сообщения юзера или уведомления; несколько подряд приходят одним сообщением.
- Юзер пишет во время вызова → сообщение встаёт в очередь и уходит модели после конца вызова. Вызов не прерывается:
  фоновых тасков тут нет, агент буквально ждёт результат. Esc → отмена (`ECANCELED`), спавненный фон продолжает работать.
- CLI `zel -p` остаётся для людей, в промпте его нет (`zel tasks` удалён в 0.5.2).

### Формат tool-входа (универсальный)
- Канон один: вызов = текст Lua-кода. Опций нет; первая строка `--!` — обычный Lua-комментарий и дедлайн не меняет.
- Транспорт выбирает адаптер провайдера по capability: free-form custom tool (сырой текст, без экранирования) там,
  где есть (Responses), иначе JSON function `{code: string}`. Оба нормализуются в один `Call{code, opts}` до рантайма.
- Новый провайдер = только адаптер транспорта; промпт, хуки (`args = {code}`), логи и сессии от формата не зависят.

### Модель ошибок
- Нормальная неудача — значение: `sh`, `web.*`, `agents.run` дают `ok=false` (+`code`/`status`/`error`).
- Всё остальное (неверные аргументы, нет файла, лимиты, `check=true`, таймаут вызова) — Lua `error()` строкой вида
  `"<fn>: <CODE>: <msg>"`, ловится `pcall`. Коды: `EINVAL ENOENT ENOMATCH EAMBIG ELIMIT EFAIL EDENIED ETIMEOUT ECANCELED`.
  Пример: `local ok, err = pcall(fs.read, "x")` → `false, "fs.read: ENOENT: x"`; `err:find(": ENOENT:")`.
- Необработанная ошибка → в tool-output: напечатанное до этого + `error: <msg>` + короткий traceback, флаг is_error.

## 2. Host API (полный список)
```lua
sh(cmd, o?) -> {ok, code, out, timed_out, ms}  -- bash -c, своя process group; out = stdout+stderr; таймаут → 124
  -- o: timeout (s), cwd, env {K=V}, stdin, check (error EFAIL при code~=0)
-- файлы (пути относительно session.cwd)
fs.read(path) -> string                -- ENOENT; файлы > 16 MiB → ELIMIT
slice(s, from, count?) -> string       -- строки from..from+count-1 (1-based); from<0 — с конца
fs.write(path, s, o?) -> nil           -- o: append; родительские папки создаются
fs.edit(path, old, new, o?) -> n       -- o: all; ENOMATCH (0 совпадений), EAMBIG (>1 без all)
fs.patch(path, {{old, new}, ...}) -> n -- несколько точных правок атомарно: все или ни одной; {old=, new=, all=} тоже можно
fs.list(dir) -> {name, dir, size}[]
fs.glob(pattern) -> string[]           -- "internal/**/*.go"
fs.grep(re, o?) -> {file, line, text}[] -- o: path, glob, fixed, max (default 200); rg, иначе Go regexp
fs.exists(path) -> boolean, "file"|"dir"|nil
fs.tmp(name?) -> string                -- уникальный путь в tmp сессии
-- сеть
web.request{url, method?, headers?, body?, json?, timeout?} -> {ok, status, headers, body, error?}
web.get(url, o?) / web.post(url, o?)   -- сахар; ok = 2xx; сетевая ошибка → ok=false, status=0
json.encode(v, o?) -> string; json.decode(s) -> value   -- o: indent; decode: EINVAL
-- фон: tasks и agents — два независимых namespace; общего Handle нет, джоб — строка-ID ("t1", "a1")
tasks.spawn(fn, o?) -> id              -- Lua-функция в фоне в своей VM, без лимита 30 s; o: name, timeout (s)
  -- захват копией: upvalue-числа/строки/булевы/таблицы (глубоко)/функции (рекурсивно) копируются в новую VM;
  -- userdata/корутины → EINVAL при spawn. Изменения внутри не видны снаружи, глобалов main-VM нет.
  -- print() таска идёт в его лог (tasks.tail), return — в result.value. Отменяется на quit/new/reload.
-- сабагенты — полноценный дочерний Runtime в этом же процессе (своя сессия, свой контекст), без `zel -p`
agents.spawn{prompt, model?, effort?, provider?, cwd?, timeout?} -> id
agents.run{...} -> AgentResult         -- spawn + wait
-- одинаково в tasks.* и agents.* (ID чужого namespace → ENOENT)
X.list() -> rows                       -- tasks: {id, name, state}; agents: {id, model, state, prompt}
X.get(id) -> row|nil
X.status(id) -> "running"|"done"|"failed"|"stopped"|"timeout"
X.wait(id, timeout?) -> result | nil, "timeout"  -- таймаут ожидания НЕ убивает фон; дедлайн вызова (30 s) → ETIMEOUT
X.wait_all(ids, timeout?) -> results[] -- по индексам; nil у тех, кто не успел; один общий дедлайн
X.result(id) -> result|nil             -- без блокировки
X.tail(id, n?) -> string               -- последние n строк лога (у агента — шаги и финальный ответ)
X.stop(id) -> nil                      -- отмена контекста (task и agent)
-- TaskResult  = {id, ok, value, error?, out, timed_out, ms}   value = return функции, out = хвост 64 KiB напечатанного
-- AgentResult = {id, ok, answer, session, error?, ms}
-- Результат отдаётся ровно один раз: либо wait/run забирает его, либо приходит уведомление <notification>t1 finished</notification>.
-- состояние между вызовами
state.get(key, scope?) -> value|nil
state.set(key, value, scope?) -> nil   -- value=nil удаляет
state.list(prefix?, scope?) -> string[]  -- ключи, отсортированы
-- прочее
view(path, ...) -> nil                 -- картинки уйдут в следующий запрос
session.{id, cwd, root, parent, depth, model, effort}; session.rename(name)
session.models() -> models, failures  -- models: {provider, model, context_window}[] (the model menu's list; context_window 0 when unknown); failures: {provider = error}; listed once per session, failures cached with it; listed again after a provider change
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
zel.defaults{provider=, model=, effort=}  -- CLI-флаги выше
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
| `session_start` | `{}` | nil или строку / `{notify = "текст"}` → уведомление (будит агента) |
| `session_end` | `{reason}` (`quit`/`new`/`resume`) | ничего; на все хуки вместе 5 s |
| `message` | `{text}` | nil или новую строку текста юзера |
| `turn_end` | `{answer}` | nil или строку / `{notify = "текст"}` → уведомление, ход продолжается (не больше 3 раз подряд) |
| `subagent_start` | `{id, prompt, model, effort, provider, cwd, depth}` | nothing; runs in the background when the subagent begins |
| `subagent_end` | `{id, status, ok, answer, error?, session, model, effort, ms}` | nothing; once per subagent: `done`/`failed`/`stopped`/`timeout` |

С 0.5.2 хуков ровно четыре: `before_call`, `after_call` и `bg_done` удалены (`zel.on` с ними — ошибка загрузки слоя).
Всё, что хуки сообщают агенту, приходит в одном формате `<notification>текст</notification>`.
`ctx = {session, layer}`. Ошибка в хуке → warning в Tracing, считается nil.

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
One tool: lua (raw Lua). Each call has a fixed 30 s timeout that cannot be changed: run longer work with tasks.spawn/agents.spawn and check it in later calls.
The VM persists between calls: globals and functions stay until the session changes, init.lua reloads or a call times out; the next output then starts with "[lua: fresh VM ...]". state.* outlives the VM.
Output = print() + return (JSON), never truncated: filter in Lua and print only what you need, e.g. local a = fs.read("bigfile.md"); print(slice(a, 100, 10)).
Errors raise "fn: CODE: msg" (pcall); sh/web/agents.run return ok=false instead. Everything blocks.
You wake up only for user messages and notifications like <notification>t1 finished</notification>; they are short, fetch details yourself (tasks.result(id)).
## built-in
sh(cmd, o?) -> {ok,code,out,timed_out,ms}  -- o: timeout,cwd,env,stdin,check
fs.read(path) -> string; slice(s, from, count?) -> string  -- lines from..from+count-1; from<0 counts from the end
fs.write(path, s, o?)  -- o: append
fs.edit(path, old, new, o?) -> n  -- exact match; o: all
fs.patch(path, {{old, new}, ...}) -> n  -- several exact edits, all or nothing; an edit may set all=true
fs.list(dir) -> {name,dir,size}[]; fs.glob(pat) -> string[]; fs.exists(path) -> bool,kind; fs.tmp(name?) -> path
fs.grep(re, o?) -> {file,line,text}[]  -- o: path,glob,fixed,max
web.get(url, o?) / web.post(url, o?) / web.request{url,method,headers,body,json,timeout} -> {ok,status,headers,body,error}
json.encode(v, o?) / json.decode(s)
tasks.spawn(fn, o?) -> id  -- fn runs in its own VM in the background, no time limit; o: name,timeout
--   fn gets copies of what it captures (locals, tables, functions); changes stay inside and globals of this VM are not there
--   result: {ok,value,error,out,ms,timed_out}; value = fn's return value, out = what it printed
--   stopped when the session changes (new/resume) or init.lua reloads
agents.spawn{prompt,model,effort,provider,cwd,timeout} -> id; agents.run{...} -> {ok,answer,session,error}
tasks.* and agents.* both have: get(id) list() status(id) wait(id, timeout?) wait_all(ids, timeout?) result(id) tail(id, n?) stop(id)
state.get(k, scope?) / state.set(k, v, scope?) / state.list(prefix?, scope?)  -- scope: "session"|"project"; JSON values
view(path, ...); session.{id,cwd,root,parent,depth,model,effort}; session.rename(name); log.info/warn/notice(msg); sleep(s)
session.models() -> models, failures  -- models: {provider,model,context_window}[] (the model menu's list); failures: {provider: error}; listed once per session, failures cached with it; listed again after a provider change
## global (~/.zel/init.lua)
web.search(q: string, n?: integer=5) -> {title,url,body}[]  -- DuckDuckGo search via ddgs
--   e.g. for _, h in ipairs(web.search("gopher-lua context", 3)) do print(h.url) end
web.fetch(url: string, max?: integer=20000) -> string  -- page as markdown (trafilatura, fallback crwl)
--   max: cut to N bytes
```
Порядок ~550 токенов на весь API; каждая плагинная функция — 1–3 строки.

### 6.3 Проект `repos/zel/.zel/init.lua` — инструкции, функция проекта, хуки, state
```lua
-- Copy to ./.zel/init.lua of a Go project: instructions, a project function, hooks, state.
local zel = require("zel")
zel.instructions{file = "AGENTS.md"}
zel.prompt(function() return "git status:\n" .. sh("git status --short").out end)

-- A Lua call lives 30 s, so the slow checks run as one background task.
zel.fn{
  name = "zel.check", summary = "start gofmt + vet + race tests in the background, returns the task id",
  params = {{"pkg", "string?", "go package pattern", default = "./..."}},
  returns = "string (task id: read it with tasks.wait(id, 20) or wait for its notification)",
  examples = {'local id = zel.check("./internal/agent/...")'},
  run = function(pkg)
    pkg = pkg or "./..."
    local id = tasks.spawn(function()
      local r = sh('test -z "$(gofmt -l .)" && go vet ' .. pkg .. ' && go test -race -count=1 ' .. pkg)
      print(r.out)
      return r.ok
    end, {name = "check", timeout = 600})
    state.set("last_check", {id = id, at = os.time()}, "project")
    return id
  end,
}

zel.on("turn_end", function()
  if sh("git status --porcelain").out ~= "" then
    return "uncommitted changes: run zel.check() and commit per AGENTS.md"
  end
end)
```
Добавит в промпт:
```text
## project (repos/zel/.zel/init.lua)
zel.check(pkg?: string="./...") -> string (task id: read it with tasks.wait(id, 20) or wait for its notification)  -- start gofmt + vet + race tests in the background, returns the task id
--   pkg: go package pattern
--   e.g. local id = zel.check("./internal/agent/...")
```

## 7. Implementation notes (deviations and details of 0.5.0)
- Cancelling: the TUI has no Esc-cancel for a running turn; **Ctrl+D** (stop the active turn) cancels the running
  call (`ECANCELED`). Background tasks and agents keep running, as designed.
- `turn_end` may continue a turn at most 3 times (returned text → notification), so a hook that always answers cannot loop forever.
- A queued user message is also passed through `message` hooks when it is accepted into the history.
- `agents.spawn` runs a child agent inside the application (its own session; no `zel -p` process, no report file, no
  socket). Subagents cannot start subagents (0.5.2; `agents.*` raises `EDENIED` there, `tasks.*` works). The child is visible in the task panel like any task, and closing the parent
  cancels it. `sh()` still gets `ZEL_SESSION_ID` and `ZEL_DEPTH`.
- Compatibility (0.5.1): handles and the global `wait_all` are gone (use `tasks.wait(id)` / `agents.wait_all(ids)`);
  `--! timeout=N`, the timeout setting and `zel.defaults{timeout=}` are removed; `zel view` and `zel tasks run|stop`
  are removed (use `view()` and `tasks.*`).
- Compatibility (0.5.2): `tasks.spawn(cmd)`, `tasks.run` and the `zel tasks` CLI are removed (wrap shell work in
  `tasks.spawn(function() return sh(cmd) end)`); `fs.read` lost `{from, lines}` (use `slice`); the output-limit
  setting and `zel.defaults{output_limit=}` are removed; hooks are `session_start`, `session_end`, `message`, `turn_end`.
- 0.5.3: `session.models()` is new. A turn that fails or is stopped (Ctrl+D) closes in the chat with its wall-clock time, steps and
  the error text or `stopped by user`; that closing message is kept in the saved history but never sent to the model.
- Reload with a broken layer keeps the previous snapshot completely (not layer by layer) and raises a notice;
  the first load at session start skips only the broken layer.
- Tracing kind of a call is `lua` (search `:lua text`). The lua output is not truncated (1 MiB safety cap) and the 30 s call timeout is fixed (0.5.2).
- `ZEL.md` is neither read nor migrated; the repository itself uses `.zel/init.lua` + `AGENTS.md`.
- Ready-made examples (global config, web plugin, project config) are in `docs/examples/` and covered by tests.
