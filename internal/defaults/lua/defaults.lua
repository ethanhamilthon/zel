-- Zel defaults module.
-- Installed in ~/.config/zel/defaults/defaults.lua and loaded via require("defaults").
-- Helpers are local; the returned table maps names to { description = ..., fn = ... } entries.

local unpack = unpack or table.unpack

local function raise(fn, code, msg)
  error(fn .. ": " .. code .. ": " .. tostring(msg), 0)
end

local function text_arg(fn, v)
  if type(v) ~= "string" then
    raise(fn, "EINVAL", "expected string, got " .. type(v))
  end
  return v
end

local function opts_arg(fn, v)
  if v == nil then
    return {}
  end
  if type(v) ~= "table" then
    raise(fn, "EINVAL", "expected table, got " .. type(v))
  end
  return v
end

local function bool_opt(fn, v)
  if v == nil then
    return false
  end
  if type(v) ~= "boolean" then
    raise(fn, "EINVAL", "expected boolean")
  end
  return v
end

local function int_arg(fn, v)
  if type(v) ~= "number" or v ~= math.floor(v) or math.abs(v) >= 9007199254740992 then
    raise(fn, "EINVAL", "expected integer")
  end
  return v
end

local function sq(s)
  return "'" .. s:gsub("'", "'\\''") .. "'"
end

local function first_line(s)
  s = s:match("^%s*(.-)%s*$") or ""
  local line, rest = s:match("^([^\n]*)\n(.*)$")
  local more = rest ~= nil
  line = line or s
  if #line > 200 then
    line = line:sub(1, 200)
    more = true
  end
  if more then
    line = line .. "..."
  end
  return line
end

local function exit_code_of(ok, _, code)
  if ok == true then
    return 0
  elseif type(ok) == "number" then
    return ok
  elseif type(code) == "number" then
    return code
  end
  return 1
end

local function popen_read(cmd)
  local p, err = io.popen(cmd, "r")
  if not p then
    return nil, 1, tostring(err or "popen failed")
  end
  local out = p:read("*a") or ""
  local code = exit_code_of(p:close())
  return out, code
end

local function exec_ok(cmd)
  local code = exit_code_of(os.execute(cmd))
  return code == 0, code
end

local function set_status(text)
  if not os.getenv("ZEL_SESSION_ID") then
    return
  end
  os.execute("zel status " .. sq(text) .. " >/dev/null 2>&1")
end

-- Path helpers -------------------------------------------------------------

local start_cwd
local cwd

local function expand_tilde(p)
  if p == "~" or p:sub(1, 2) == "~/" then
    local home = os.getenv("HOME")
    if home and home ~= "" then
      return home .. p:sub(2)
    end
  end
  return p
end

local function clean_path(p)
  local abs = p:sub(1, 1) == "/"
  local parts = {}
  for seg in p:gmatch("[^/]+") do
    if seg == ".." then
      if #parts > 0 and parts[#parts] ~= ".." then
        table.remove(parts)
      elseif not abs then
        parts[#parts + 1] = ".."
      end
    elseif seg ~= "." and seg ~= "" then
      parts[#parts + 1] = seg
    end
  end
  if abs then
    return "/" .. table.concat(parts, "/")
  end
  if #parts == 0 then
    return "."
  end
  return table.concat(parts, "/")
end

local function current_cwd()
  if cwd then
    return cwd
  end
  if not start_cwd then
    local out, code = popen_read("pwd")
    if out and code == 0 then
      out = out:gsub("[\r\n]+$", "")
      if out ~= "" then
        start_cwd = clean_path(out)
      end
    end
    if not start_cwd then
      start_cwd = clean_path(os.getenv("PWD") or ".")
    end
  end
  cwd = start_cwd
  return cwd
end

local function resolve(p)
  p = expand_tilde(p)
  if p:sub(1, 1) == "/" then
    return clean_path(p)
  end
  local base = current_cwd()
  if base == "/" then
    return clean_path("/" .. p)
  end
  return clean_path(base .. "/" .. p)
end

local function split_segments(p)
  local segs = {}
  for s in p:gmatch("[^/]+") do
    segs[#segs + 1] = s
  end
  return segs
end

local function rel_path(base, target)
  base = clean_path(base)
  target = clean_path(target)
  if base == target then
    return "."
  end
  local bs = split_segments(base)
  local ts = split_segments(target)
  local i = 1
  while i <= #bs and i <= #ts and bs[i] == ts[i] do
    i = i + 1
  end
  local out = {}
  for _ = i, #bs do
    out[#out + 1] = ".."
  end
  for j = i, #ts do
    out[#out + 1] = ts[j]
  end
  if #out == 0 then
    return "."
  end
  return table.concat(out, "/")
end

local function path_dir(p)
  if p == "/" then
    return "/"
  end
  local parent = p:match("^(.*)/[^/]+$")
  if not parent then
    return "."
  end
  if parent == "" then
    return "/"
  end
  return parent
end

local function path_base(p)
  if p == "/" then
    return "/"
  end
  return p:match("([^/]+)$") or p
end

local function raise_io_err(fn, err)
  local s = tostring(err or "unknown error")
  local low = s:lower()
  if low:find("no such file", 1, true) or low:find("not exist", 1, true) then
    raise(fn, "ENOENT", s)
  elseif low:find("permission denied", 1, true) then
    raise(fn, "EDENIED", s)
  elseif low:find("is a directory", 1, true) then
    raise(fn, "EINVAL", s)
  else
    raise(fn, "EFAIL", s)
  end
end

local exit_to_code = {
  [1] = "EFAIL",
  [2] = "EINVAL",
  [3] = "ENOENT",
  [4] = "ENOMATCH",
  [5] = "EAMBIG",
  [6] = "EDENIED",
  [7] = "ELIMIT",
  [124] = "ETIMEOUT",
  [130] = "ECANCELED",
}

local known_codes = {
  EINVAL = true, ENOENT = true, ENOMATCH = true, EAMBIG = true,
  ELIMIT = true, EFAIL = true, EDENIED = true, ETIMEOUT = true, ECANCELED = true,
}

local function raise_cli_err(fn, code, out)
  local msg = (out or ""):gsub("^%s+", ""):gsub("%s+$", "")
  local c, rest = msg:match("^[^:]+:%s*([A-Z]+):%s*(.+)$")
  if c and known_codes[c] then
    raise(fn, c, rest)
  end
  c, rest = msg:match("^([A-Z]+):%s*(.+)$")
  if c and known_codes[c] then
    raise(fn, c, rest)
  end
  local mapped = exit_to_code[code] or "EFAIL"
  if msg == "" then
    msg = "command exited with status " .. tostring(code)
  end
  raise(fn, mapped, msg)
end

-- JSON decoder -------------------------------------------------------------

local function utf8_char(cp)
  if cp < 0x80 then
    return string.char(cp)
  elseif cp < 0x800 then
    return string.char(0xC0 + math.floor(cp / 64), 0x80 + (cp % 64))
  elseif cp < 0x10000 then
    return string.char(
      0xE0 + math.floor(cp / 4096),
      0x80 + (math.floor(cp / 64) % 64),
      0x80 + (cp % 64)
    )
  else
    return string.char(
      0xF0 + math.floor(cp / 262144),
      0x80 + (math.floor(cp / 4096) % 64),
      0x80 + (math.floor(cp / 64) % 64),
      0x80 + (cp % 64)
    )
  end
end

local json_escapes = {
  ['"'] = '"',
  ['\\'] = '\\',
  ['/'] = '/',
  ['b'] = '\b',
  ['f'] = '\f',
  ['n'] = '\n',
  ['r'] = '\r',
  ['t'] = '\t',
}

local function json_decode(str)
  local pos = 1
  local len = #str

  local function skip_ws()
    local _, e = str:find("^[ \t\r\n]+", pos)
    if e then
      pos = e + 1
    end
  end

  local parse_value

  local function parse_string()
    pos = pos + 1
    local buf = {}
    while pos <= len do
      local c = str:sub(pos, pos)
      if c == '"' then
        pos = pos + 1
        return table.concat(buf)
      elseif c == '\\' then
        pos = pos + 1
        if pos > len then
          return nil, "unterminated escape"
        end
        local esc = str:sub(pos, pos)
        local rep = json_escapes[esc]
        if rep then
          buf[#buf + 1] = rep
          pos = pos + 1
        elseif esc == 'u' then
          local hex = str:sub(pos + 1, pos + 4)
          if not hex:match("^[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]$") then
            return nil, "invalid unicode escape"
          end
          local cp = tonumber(hex, 16)
          pos = pos + 5
          if cp >= 0xD800 and cp <= 0xDBFF and str:sub(pos, pos + 1) == "\\u" then
            local hex2 = str:sub(pos + 2, pos + 5)
            local cp2 = tonumber(hex2, 16)
            if cp2 and cp2 >= 0xDC00 and cp2 <= 0xDFFF then
              cp = 0x10000 + (cp - 0xD800) * 1024 + (cp2 - 0xDC00)
              pos = pos + 6
            end
          end
          buf[#buf + 1] = utf8_char(cp)
        else
          return nil, "invalid escape \\" .. esc
        end
      else
        local next_special = str:find('["\\]', pos)
        if not next_special then
          return nil, "unterminated string"
        end
        buf[#buf + 1] = str:sub(pos, next_special - 1)
        pos = next_special
      end
    end
    return nil, "unterminated string"
  end

  local function parse_number()
    local s, e = str:find("^%-?%d+%.?%d*[eE]?[+-]?%d*", pos)
    if not s then
      return nil, "invalid number"
    end
    local n = tonumber(str:sub(s, e))
    if not n then
      return nil, "invalid number"
    end
    pos = e + 1
    return n
  end

  local function parse_array()
    pos = pos + 1
    local arr = {}
    skip_ws()
    if str:sub(pos, pos) == ']' then
      pos = pos + 1
      return arr
    end
    local idx = 1
    while true do
      skip_ws()
      local val, err = parse_value()
      if err then
        return nil, err
      end
      arr[idx] = val
      idx = idx + 1
      skip_ws()
      local c = str:sub(pos, pos)
      if c == ']' then
        pos = pos + 1
        return arr
      elseif c == ',' then
        pos = pos + 1
      else
        return nil, "expected ',' or ']'"
      end
    end
  end

  local function parse_object()
    pos = pos + 1
    local obj = {}
    skip_ws()
    if str:sub(pos, pos) == '}' then
      pos = pos + 1
      return obj
    end
    while true do
      skip_ws()
      if str:sub(pos, pos) ~= '"' then
        return nil, "expected object key"
      end
      local key, kerr = parse_string()
      if kerr then
        return nil, kerr
      end
      skip_ws()
      if str:sub(pos, pos) ~= ':' then
        return nil, "expected ':'"
      end
      pos = pos + 1
      skip_ws()
      local val, verr = parse_value()
      if verr then
        return nil, verr
      end
      obj[key] = val
      skip_ws()
      local c = str:sub(pos, pos)
      if c == '}' then
        pos = pos + 1
        return obj
      elseif c == ',' then
        pos = pos + 1
      else
        return nil, "expected ',' or '}'"
      end
    end
  end

  parse_value = function()
    skip_ws()
    if pos > len then
      return nil, "unexpected end of JSON"
    end
    local c = str:sub(pos, pos)
    if c == '"' then
      return parse_string()
    elseif c == '{' then
      return parse_object()
    elseif c == '[' then
      return parse_array()
    elseif c == 't' and str:sub(pos, pos + 3) == "true" then
      pos = pos + 4
      return true
    elseif c == 'f' and str:sub(pos, pos + 4) == "false" then
      pos = pos + 5
      return false
    elseif c == 'n' and str:sub(pos, pos + 3) == "null" then
      pos = pos + 4
      return nil
    else
      return parse_number()
    end
  end

  local val, err = parse_value()
  if err then
    return nil, err
  end
  skip_ws()
  if pos <= len then
    return nil, "trailing characters in JSON"
  end
  return val
end
-- Glob matcher -------------------------------------------------------------

local function seg_match(pat, s)
  local plen, slen = #pat, #s

  local function match_class(pi, ch)
    if pi > plen then
      return false, pi
    end
    local neg = false
    if pat:sub(pi, pi) == "!" or pat:sub(pi, pi) == "^" then
      neg = true
      pi = pi + 1
    end
    local matched = false
    local first = true
    local j = pi
    while j <= plen do
      local c = pat:sub(j, j)
      if c == "]" and not first then
        local ok = matched
        if neg then ok = not ok end
        return ok, j + 1
      end
      first = false
      if j + 2 <= plen and pat:sub(j + 1, j + 1) == "-" and pat:sub(j + 2, j + 2) ~= "]" then
        local hi = pat:sub(j + 2, j + 2)
        if ch >= c and ch <= hi then
          matched = true
        end
        j = j + 3
      else
        if ch == c then
          matched = true
        end
        j = j + 1
      end
    end
    return nil, pi
  end

  local function step(pi, si)
    while pi <= plen do
      local c = pat:sub(pi, pi)
      if c == "*" then
        while pi <= plen and pat:sub(pi, pi) == "*" do
          pi = pi + 1
        end
        if pi > plen then
          return true
        end
        for k = si, slen do
          if step(pi, k) then
            return true
          end
        end
        return false
      elseif c == "?" then
        if si > slen then
          return false
        end
        pi = pi + 1
        si = si + 1
      elseif c == "\\" then
        if si > slen then
          return false
        end
        if pi == plen then
          if s:sub(si, si) ~= "\\" then
            return false
          end
          pi = pi + 1
          si = si + 1
        else
          local lit = pat:sub(pi + 1, pi + 1)
          if s:sub(si, si) ~= lit then
            return false
          end
          pi = pi + 2
          si = si + 1
        end
      elseif c == "[" then
        if si > slen then
          return false
        end
        local ok, next_pi = match_class(pi + 1, s:sub(si, si))
        if ok == nil then
          if s:sub(si, si) ~= "[" then
            return false
          end
          pi = pi + 1
          si = si + 1
        elseif not ok then
          return false
        else
          pi = next_pi
          si = si + 1
        end
      else
        if si > slen or s:sub(si, si) ~= c then
          return false
        end
        pi = pi + 1
        si = si + 1
      end
    end
    return si > slen
  end

  return step(1, 1)
end

local function glob_match(pattern, path)
  local pat_segs = split_segments(pattern)
  local path_segs = split_segments(path)
  local function step(pi, si)
    while pi <= #pat_segs do
      local seg = pat_segs[pi]
      if seg == "**" then
        if pi == #pat_segs then
          return si <= #path_segs or #path_segs == 0
        end
        for k = si, #path_segs + 1 do
          if step(pi + 1, k) then
            return true
          end
        end
        return false
      else
        if si > #path_segs or not seg_match(seg, path_segs[si]) then
          return false
        end
        pi = pi + 1
        si = si + 1
      end
    end
    return si > #path_segs
  end
  return step(1, 1)
end

local has_rg_cached
local function has_rg()
  if has_rg_cached == nil then
    has_rg_cached = exec_ok("command -v rg >/dev/null 2>&1")
  end
  return has_rg_cached
end

local function path_kind(p)
  local out, code = popen_read("if test -d " .. sq(p) .. "; then echo d; elif test -e " .. sq(p) .. "; then echo f; fi")
  if not out or code ~= 0 then
    return nil
  end
  local ch = out:sub(1, 1)
  if ch == "d" then
    return "dir"
  elseif ch == "f" then
    return "file"
  end
  return nil
end

-- Module entries -----------------------------------------------------------

local entry_mt = {
  __call = function(self, ...)
    return self.fn(...)
  end,
}

local function entry(description, fn)
  return setmetatable({ description = description, fn = fn }, entry_mt)
end

local MAX_READ_BYTES = 16 * 1024 * 1024

local image_exts = {
  [".png"] = true,
  [".jpg"] = true,
  [".jpeg"] = true,
  [".gif"] = true,
  [".webp"] = true,
}

local function ext_lower(p)
  local base = path_base(p)
  local ext = base:match("(%.[^./]+)$")
  return ext and ext:lower() or ""
end

local sh_entry = entry(
  "`sh(cmd, {timeout, cwd, env, stdin, check}?) -> {ok, code, out, timed_out, ms}` - bash -c in its own process group; out = stdout+stderr; check=true raises on failure",
  function(cmd, opts)
    cmd = text_arg("sh", cmd)
    local o = opts_arg("sh", opts)
    local timeout = o.timeout
    if timeout ~= nil then
      if type(timeout) ~= "number" or timeout <= 0 then
        raise("sh", "EINVAL", "timeout must be a positive number of seconds")
      end
    end
    local dir = current_cwd()
    if o.cwd ~= nil then
      dir = resolve(text_arg("sh", o.cwd))
    end
    local env_flags = {}
    if o.env ~= nil then
      if type(o.env) ~= "table" then
        raise("sh", "EINVAL", "env must be a table")
      end
      for k, v in pairs(o.env) do
        if type(k) ~= "string" or k == "" or k:find("=", 1, true) then
          raise("sh", "EINVAL", "env keys must be nonempty strings without '='")
        end
        if type(v) ~= "string" then
          raise("sh", "EINVAL", "env values must be strings")
        end
        env_flags[#env_flags + 1] = "--env " .. sq(k .. "=" .. v)
      end
    end
    local stdin_data = o.stdin
    if stdin_data ~= nil then
      stdin_data = text_arg("sh", stdin_data)
    end
    local check = bool_opt("sh", o.check)

    set_status("$ " .. first_line(cmd))

    local parts = { "zel exec" }
    if timeout ~= nil then
      parts[#parts + 1] = "--timeout " .. tostring(timeout)
    end
    parts[#parts + 1] = "--cwd " .. sq(dir)
    for _, ef in ipairs(env_flags) do
      parts[#parts + 1] = ef
    end
    parts[#parts + 1] = "-- bash -c " .. sq(cmd)

    local tmp
    if stdin_data ~= nil then
      tmp = os.tmpname()
      local tf, terr = io.open(tmp, "wb")
      if not tf then
        os.remove(tmp)
        raise("sh", "EFAIL", tostring(terr))
      end
      local wok, werr = tf:write(stdin_data)
      local cok, cerr = tf:close()
      if not wok or not cok then
        os.remove(tmp)
        raise("sh", "EFAIL", tostring(werr or cerr))
      end
      parts[#parts + 1] = "< " .. sq(tmp)
    end

    local out, code, perr = popen_read(table.concat(parts, " "))
    if tmp then
      os.remove(tmp)
    end
    if not out then
      raise("sh", "EFAIL", perr)
    end
    local parsed, jerr = json_decode(out)
    if type(parsed) ~= "table" then
      if code ~= 0 then
        raise_cli_err("sh", code, out)
      end
      raise("sh", "EFAIL", "invalid JSON from zel exec: " .. tostring(jerr))
    end
    local rcode = parsed.code or 0
    local rok = parsed.ok
    if rok == nil then
      rok = (rcode == 0)
    end
    local rout = parsed.out or ""
    if check and rcode ~= 0 then
      raise("sh", "EFAIL", "exit status " .. tostring(rcode) .. ": " .. first_line(rout))
    end
    return {
      ok = rok,
      code = rcode,
      out = rout,
      timed_out = parsed.timed_out == true,
      ms = parsed.ms or 0,
    }
  end
)

local read_entry = entry(
  "`read(path) -> text` - read file (max 16 MiB); images (png/jpg/jpeg/gif/webp) attach to your next request and return nil",
  function(path)
    path = text_arg("read", path)
    set_status("read " .. path)
    local p = resolve(path)
    local f, err = io.open(p, "rb")
    if not f then
      raise_io_err("read", err)
    end
    if image_exts[ext_lower(p)] then
      local probe, rerr = f:read(1)
      if rerr then
        f:close()
        raise_io_err("read", rerr)
      end
      local sz, serr = f:seek("end")
      f:close()
      if sz and sz > MAX_READ_BYTES then
        raise("read", "ELIMIT", p .. " exceeds 16 MiB")
      end
      if not sz and serr then
        raise_io_err("read", serr)
      end
      local out, code, perr = popen_read("zel steer --file " .. sq(p) .. " 2>&1")
      if not out then
        raise("read", "EFAIL", perr)
      end
      if code ~= 0 then
        raise_cli_err("read", code, out)
      end
      return nil
    end
    local data, rerr = f:read(MAX_READ_BYTES + 1)
    f:close()
    if rerr then
      raise_io_err("read", rerr)
    end
    data = data or ""
    if #data > MAX_READ_BYTES then
      raise("read", "ELIMIT", p .. " exceeds 16 MiB")
    end
    return data
  end
)

local slice_entry = entry(
  "`slice(s, from, count?) -> text` - lines from..from+count-1; negative from counts from the end",
  function(s, from, count)
    s = text_arg("slice", s)
    from = int_arg("slice", from)
    if from == 0 then
      raise("slice", "EINVAL", "lines are 1-based")
    end
    local lines = {}
    local pos = 1
    local len = #s
    while pos <= len do
      local nl = s:find("\n", pos, true)
      if nl then
        lines[#lines + 1] = s:sub(pos, nl)
        pos = nl + 1
      else
        lines[#lines + 1] = s:sub(pos)
        break
      end
    end
    local nlines = #lines
    local start
    if from > 0 then
      start = from
    else
      start = math.max(1, nlines + from + 1)
    end
    if start > nlines then
      return ""
    end
    local last = nlines
    if count ~= nil then
      local n = int_arg("slice", count)
      if n < 0 then
        raise("slice", "EINVAL", "count must not be negative")
      end
      last = math.min(nlines, start + n - 1)
    end
    if last < start then
      return ""
    end
    return table.concat(lines, "", start, last)
  end
)

local write_entry = entry(
  "`write(path, s, {append}?)` - write file (creates parent folders). Write large files in small chunks: a short `write`, then successive `edit` calls; never write or replace a large file at once",
  function(path, s, opts)
    path = text_arg("write", path)
    s = text_arg("write", s)
    local o = opts_arg("write", opts)
    local append = bool_opt("write", o.append)
    set_status("write " .. path)
    local p = resolve(path)
    local parent = path_dir(p)
    if parent ~= "." and parent ~= "/" then
      local ok = exec_ok("mkdir -p " .. sq(parent))
      if not ok then
        raise("write", "EFAIL", "could not create parent directory " .. parent)
      end
    end
    local f, err = io.open(p, append and "ab" or "wb")
    if not f then
      raise_io_err("write", err)
    end
    local wok, werr = f:write(s)
    local cok, cerr = f:close()
    if not wok then
      raise_io_err("write", werr)
    end
    if not cok then
      raise_io_err("write", cerr)
    end
  end
)

local function literal_count(s, sub)
  local n = 0
  local pos = 1
  local sublen = #sub
  while true do
    local i = s:find(sub, pos, true)
    if not i then
      break
    end
    n = n + 1
    pos = i + sublen
  end
  return n
end

local function literal_replace_all(s, old, new)
  local out = {}
  local pos = 1
  local oldlen = #old
  while true do
    local i = s:find(old, pos, true)
    if not i then
      out[#out + 1] = s:sub(pos)
      break
    end
    out[#out + 1] = s:sub(pos, i - 1)
    out[#out + 1] = new
    pos = i + oldlen
  end
  return table.concat(out)
end

local edit_entry = entry(
  "`edit(path, old, new, {all}?) -> n` or `edit(path, {{old, new, all?}, ...}) -> n` - exact-text replacement (all or nothing). Change existing files with `edit`; never rewrite a whole file with `write`",
  function(path, old_or_list, new_or_nil, opts)
    path = text_arg("edit", path)
    local blocks = {}
    if type(old_or_list) == "table" then
      local len = #old_or_list
      if len == 0 then
        raise("edit", "EINVAL", "expected a nonempty list of edits")
      end
      for k in pairs(old_or_list) do
        if type(k) ~= "number" or k < 1 or k > len or k ~= math.floor(k) then
          raise("edit", "EINVAL", "expected a nonempty list of edits")
        end
      end
      for i = 1, len do
        local row = old_or_list[i]
        if type(row) ~= "table" then
          raise("edit", "EINVAL", "expected table, got " .. type(row))
        end
        blocks[#blocks + 1] = {
          old = text_arg("edit", row[1]),
          new = text_arg("edit", row[2]),
          all = bool_opt("edit", row[3]),
        }
      end
    else
      local old = text_arg("edit", old_or_list)
      local new = text_arg("edit", new_or_nil)
      local o = opts_arg("edit", opts)
      blocks[1] = { old = old, new = new, all = bool_opt("edit", o.all) }
    end
    for _, b in ipairs(blocks) do
      if b.old == "" then
        raise("edit", "EINVAL", "old text is empty")
      end
    end

    set_status("edit " .. path)
    local p = resolve(path)
    local f, err = io.open(p, "rb")
    if not f then
      raise_io_err("edit", err)
    end
    local data, rerr = f:read(MAX_READ_BYTES + 1)
    f:close()
    if rerr then
      raise_io_err("edit", rerr)
    end
    data = data or ""
    if #data > MAX_READ_BYTES then
      raise("edit", "ELIMIT", p .. " exceeds 16 MiB")
    end

    local content = data
    local total = 0
    for i, b in ipairs(blocks) do
      local n = literal_count(content, b.old)
      if n == 0 then
        raise("edit", "ENOMATCH", "block " .. i .. ": old text not found")
      elseif n > 1 and not b.all then
        raise("edit", "EAMBIG", "block " .. i .. ": old text found " .. n .. " times; make it unique or pass all = true")
      end
      if b.all then
        content = literal_replace_all(content, b.old, b.new)
        total = total + n
      else
        local s_idx, e_idx = content:find(b.old, 1, true)
        content = content:sub(1, s_idx - 1) .. b.new .. content:sub(e_idx + 1)
        total = total + 1
      end
    end

    local wf, wopen_err = io.open(p, "wb")
    if not wf then
      raise_io_err("edit", wopen_err)
    end
    local wok, werr = wf:write(content)
    local cok, cerr = wf:close()
    if not wok then
      raise_io_err("edit", werr)
    end
    if not cok then
      raise_io_err("edit", cerr)
    end
    return total
  end
)
local list_entry = entry(
  "`list(dir?) -> {name, dir, size}[]` - list entries in a directory",
  function(dir)
    local target = current_cwd()
    if dir ~= nil then
      target = resolve(text_arg("list", dir))
    end
    local kind = path_kind(target)
    if not kind then
      raise("list", "ENOENT", target .. ": no such file or directory")
    elseif kind ~= "dir" then
      raise("list", "EINVAL", target .. ": not a directory")
    end
    local script = table.concat({
      "cd " .. sq(target) .. " || exit 1",
      "if stat -f '%z' . >/dev/null 2>&1; then sc() { stat -f '%z' -- \"$1\" 2>/dev/null || echo 0; }; else sc() { stat -c '%s' -- \"$1\" 2>/dev/null || echo 0; }; fi",
      "for f in * .*; do",
      "  [ \"$f\" = \".\" ] || [ \"$f\" = \"..\" ] && continue",
      "  [ -e \"$f\" ] || [ -L \"$f\" ] || continue",
      "  if [ -d \"$f\" ]; then",
      "    printf 'd\\t0\\t%s\\0' \"$f\"",
      "  else",
      "    printf 'f\\t%s\\t%s\\0' \"$(sc \"$f\")\" \"$f\"",
      "  fi",
      "done",
    }, "\n")
    local out, code, perr = popen_read(script)
    if not out then
      raise("list", "EFAIL", perr)
    end
    if code ~= 0 then
      raise("list", "EFAIL", "could not list " .. target)
    end
    local entries = {}
    for rec in out:gmatch("([^%z]+)%z") do
      local k, sz, name = rec:match("^([df])\t(%d+)\t(.*)$")
      if k then
        entries[#entries + 1] = {
          name = name,
          dir = (k == "d"),
          size = tonumber(sz) or 0,
        }
      end
    end
    table.sort(entries, function(a, b) return a.name < b.name end)
    return entries
  end
)

local glob_entry = entry(
  "`glob(pattern) -> paths` - find files matching pattern (`**` supported, respects .gitignore when rg is available)",
  function(pattern)
    pattern = text_arg("glob", pattern)
    pattern = expand_tilde(pattern)
    local absolute = pattern:sub(1, 1) == "/"
    local widx = pattern:find("[*?%[]")
    local bidx = pattern:find("\\", 1, true)
    if bidx and (not widx or bidx < widx) then
      widx = bidx
    end
    if not widx then
      local p = resolve(pattern)
      if path_kind(p) ~= nil then
        return { absolute and p or rel_path(current_cwd(), p) }
      end
      return {}
    end
    local prefix = pattern:sub(1, widx - 1)
    local last_slash = prefix:match("^.*()/")
    local dir_part, subpat
    if last_slash then
      dir_part = pattern:sub(1, last_slash)
      subpat = pattern:sub(last_slash + 1)
    else
      dir_part = ""
      subpat = pattern
    end
    local root
    if dir_part ~= "" then
      root = resolve(dir_part)
    else
      root = current_cwd()
    end
    if path_kind(root) ~= "dir" then
      return {}
    end

    local cmd
    if has_rg() then
      cmd = "cd " .. sq(root) .. " && rg --files --hidden --glob '!.git'"
    else
      cmd = "cd " .. sq(root) .. " && find . -name .git -prune -o -type f -print"
    end
    local out, code, perr = popen_read(cmd)
    if not out then
      raise("glob", "EFAIL", perr)
    end
    if code ~= 0 and out == "" then
      return {}
    end

    local cwd_now = current_cwd()
    local results = {}
    for line in out:gmatch("[^\r\n]+") do
      local rel = line:gsub("^%./", "")
      if rel ~= "" and rel ~= "." and glob_match(subpat, rel) then
        local full
        if root == "/" then
          full = "/" .. rel
        else
          full = root .. "/" .. rel
        end
        if absolute then
          results[#results + 1] = clean_path(full)
        else
          results[#results + 1] = rel_path(cwd_now, full)
        end
      end
    end
    table.sort(results)
    return results
  end
)

local grep_entry = entry(
  "`grep(re, {path, glob, fixed, max}?) -> {file, line, text}[]` - search files (max 200 by default, respects .gitignore when rg is available)",
  function(re, opts)
    re = text_arg("grep", re)
    local o = opts_arg("grep", opts)
    local fixed = bool_opt("grep", o.fixed)
    local limit = 200
    if o.max ~= nil then
      limit = int_arg("grep", o.max)
    end
    if limit < 0 then
      raise("grep", "EINVAL", "max must not be negative")
    end
    local glob_pat
    if o.glob ~= nil then
      glob_pat = text_arg("grep", o.glob)
    end
    local cwd_now = current_cwd()
    local target = cwd_now
    if o.path ~= nil then
      target = resolve(text_arg("grep", o.path))
    end

    set_status("grep " .. re)

    local kind = path_kind(target)
    if not kind then
      raise("grep", "ENOENT", target .. ": no such file or directory")
    end
    if limit == 0 then
      return {}
    end

    local function display_file(p)
      local full
      if p:sub(1, 1) == "/" then
        full = clean_path(p)
      else
        full = resolve(p)
      end
      return rel_path(cwd_now, full)
    end

    local rows = {}
    if has_rg() then
      local args = { "cd " .. sq(cwd_now) .. " && rg --json --hidden --glob '!.git'" }
      if fixed then
        args[#args + 1] = "-F"
      end
      if glob_pat then
        args[#args + 1] = "-g " .. sq(glob_pat)
      end
      local search_arg = rel_path(cwd_now, target)
      if search_arg:sub(1, 2) == ".." then
        search_arg = target
      end
      args[#args + 1] = "-- " .. sq(re) .. " " .. sq(search_arg) .. " 2>&1"
      local out, code, perr = popen_read(table.concat(args, " "))
      if not out then
        raise("grep", "EFAIL", perr)
      end
      if code > 1 then
        local err_lines = {}
        for line in out:gmatch("[^\r\n]+") do
          if line:sub(1, 1) ~= "{" then
            err_lines[#err_lines + 1] = line
          end
        end
        local msg = #err_lines > 0 and table.concat(err_lines, "\n") or out
        raise("grep", "EINVAL", msg)
      end
      for line in out:gmatch("[^\r\n]+") do
        if #rows >= limit then
          break
        end
        local obj = json_decode(line)
        if type(obj) == "table" and obj.type == "match" and type(obj.data) == "table" then
          local data = obj.data
          local ptext = type(data.path) == "table" and data.path.text or nil
          local lnum = data.line_number
          local ltext = type(data.lines) == "table" and data.lines.text or nil
          if ptext and lnum and ltext then
            ltext = ltext:gsub("\r?\n$", "")
            rows[#rows + 1] = {
              file = display_file(ptext),
              line = lnum,
              text = ltext,
            }
          end
        end
      end
      return rows
    end

    -- Fallback: find + grep -rnE / -rnF
    local flag = fixed and "-nFI" or "-nEI"
    local function run_grep_lines(cmd)
      local out, code, perr = popen_read(cmd)
      if not out then
        raise("grep", "EFAIL", perr)
      end
      if code > 1 then
        raise("grep", "EINVAL", out:gsub("%s+$", ""))
      end
      for line in out:gmatch("[^\r\n]+") do
        if #rows >= limit then
          break
        end
        local file, lnum, text = line:match("^(.-):(%d+):(.*)$")
        if file and lnum then
          rows[#rows + 1] = {
            file = display_file(file),
            line = tonumber(lnum),
            text = text,
          }
        end
      end
    end

    if glob_pat then
      local files = {}
      if kind == "file" then
        files[1] = target
      else
        local fout, fcode, fperr = popen_read("cd " .. sq(target) .. " && find . -name .git -prune -o -type f -print")
        if not fout then
          raise("grep", "EFAIL", fperr)
        end
        if fcode == 0 then
          for line in fout:gmatch("[^\r\n]+") do
            local rel = line:gsub("^%./", "")
            if rel ~= "" and rel ~= "." then
              files[#files + 1] = (target == "/" and "/" or target .. "/") .. rel
            end
          end
        end
      end
      local basename_only = not glob_pat:find("/", 1, true) and glob_pat ~= "**"
      local matched = {}
      for _, fpath in ipairs(files) do
        local candidate
        if basename_only or kind == "file" then
          candidate = path_base(fpath)
        else
          candidate = rel_path(target, fpath)
        end
        if glob_match(glob_pat, candidate) then
          matched[#matched + 1] = fpath
        end
      end
      table.sort(matched)
      local i = 1
      while i <= #matched and #rows < limit do
        local batch = {}
        local j = math.min(#matched, i + 49)
        for k = i, j do
          batch[#batch + 1] = sq(matched[k])
        end
        run_grep_lines("cd " .. sq(cwd_now) .. " && grep " .. flag .. " -H -e " .. sq(re) .. " -- " .. table.concat(batch, " ") .. " 2>&1")
        i = j + 1
      end
    else
      local rflag = kind == "dir" and (flag .. " -r --exclude-dir=.git") or flag
      local search_arg = rel_path(cwd_now, target)
      if search_arg:sub(1, 2) == ".." then
        search_arg = target
      end
      run_grep_lines("cd " .. sq(cwd_now) .. " && grep " .. rflag .. " -H -e " .. sq(re) .. " -- " .. sq(search_arg) .. " 2>&1")
    end
    return rows
  end
)

local exists_entry = entry(
  "`exists(path) -> bool, \"file\"|\"dir\"|nil` - check if path exists and its kind",
  function(path)
    path = text_arg("exists", path)
    local k = path_kind(resolve(path))
    if not k then
      return false, nil
    end
    return true, k
  end
)

local cd_entry = entry(
  "`cd(dir?)` - change working folder for file/shell calls; no argument resets to the start folder",
  function(dir)
    if dir == nil then
      current_cwd()
      cwd = start_cwd
      return
    end
    dir = text_arg("cd", dir)
    local p = resolve(dir)
    local k = path_kind(p)
    if not k then
      raise("cd", "ENOENT", p .. ": no such file or directory")
    elseif k ~= "dir" then
      raise("cd", "EINVAL", p .. ": not a directory")
    end
    cwd = p
  end
)

local choice_entry = entry(
  "`choice(question, {options...}) -> id` - show a picker in the UI (non-blocking; answer arrives in `<notification>`). Prefer it to asking in your final answer",
  function(question, options)
    question = text_arg("choice", question)
    if type(options) ~= "table" then
      raise("choice", "EINVAL", "expected table, got " .. type(options))
    end
    local n = #options
    if n == 0 then
      raise("choice", "EINVAL", "expected at least one option")
    end
    for k in pairs(options) do
      if type(k) ~= "number" or k < 1 or k > n or k ~= math.floor(k) then
        raise("choice", "EINVAL", "expected a list")
      end
    end
    local args = { "zel choice" }
    local need_sep = question:sub(1, 1) == "-"
    if not need_sep then
      for i = 1, n do
        if type(options[i]) == "string" and options[i]:sub(1, 1) == "-" then
          need_sep = true
          break
        end
      end
    end
    if need_sep then
      args[#args + 1] = "--"
    end
    args[#args + 1] = sq(question)
    for i = 1, n do
      args[#args + 1] = sq(text_arg("choice", options[i]))
    end
    local out, code, perr = popen_read(table.concat(args, " ") .. " 2>&1")
    if not out then
      raise("choice", "EFAIL", perr)
    end
    if code ~= 0 then
      raise_cli_err("choice", code, out)
    end
    return (out:gsub("[\r\n]+$", ""))
  end
)

local docs_entry = entry(
  "`docs(topic?) -> text` - Zel reference (`zel docs`); use it when the user asks about Zel, its config, hooks and so on",
  function(topic)
    local cmd = "zel docs"
    if topic ~= nil then
      topic = text_arg("docs", topic)
      cmd = cmd .. " " .. sq(topic)
    end
    local out, code, perr = popen_read(cmd .. " 2>&1")
    if not out then
      raise("docs", "EFAIL", perr)
    end
    if code ~= 0 then
      raise_cli_err("docs", code, out)
    end
    return (out:gsub("\n$", ""))
  end
)

local models_entry = entry(
  "`models() -> list, failures` - list available models of configured providers",
  function()
    local out, code, perr = popen_read("zel models --json 2>&1")
    if not out then
      raise("models", "EFAIL", perr)
    end
    if code ~= 0 then
      raise_cli_err("models", code, out)
    end
    local parsed, jerr = json_decode(out)
    if type(parsed) ~= "table" then
      raise("models", "EFAIL", "invalid JSON from zel models: " .. tostring(jerr))
    end
    local raw_list = parsed.models or parsed
    local failures = parsed.failures or {}
    local list = {}
    for _, m in ipairs(raw_list) do
      if type(m) == "table" then
        list[#list + 1] = {
          provider = m.provider or m.Provider,
          model = m.model or m.Model,
          context_window = m.context_window or m.ContextWindow,
        }
      end
    end
    return list, failures
  end
)

local session_entry = entry(
  "`session() -> table` - current session info (id, name, cwd, root, mode, modes, depth, model, effort, provider)",
  function()
    local out, code, perr = popen_read("zel session --json 2>&1")
    if not out then
      raise("session", "EFAIL", perr)
    end
    if code ~= 0 then
      raise_cli_err("session", code, out)
    end
    local parsed, jerr = json_decode(out)
    if type(parsed) ~= "table" then
      raise("session", "EFAIL", "invalid JSON from zel session: " .. tostring(jerr))
    end
    return parsed
  end
)

local rename_entry = entry(
  "`rename(name)` - rename the current session",
  function(name)
    name = text_arg("rename", name)
    local cmd = "zel rename "
    if name:sub(1, 1) == "-" then
      cmd = cmd .. "-- "
    end
    local out, code, perr = popen_read(cmd .. sq(name) .. " 2>&1")
    if not out then
      raise("rename", "EFAIL", perr)
    end
    if code ~= 0 then
      raise_cli_err("rename", code, out)
    end
  end
)

return {
  sh = sh_entry,
  read = read_entry,
  write = write_entry,
  edit = edit_entry,
  slice = slice_entry,
  list = list_entry,
  glob = glob_entry,
  grep = grep_entry,
  exists = exists_entry,
  cd = cd_entry,
  choice = choice_entry,
  docs = docs_entry,
  models = models_entry,
  session = session_entry,
  rename = rename_entry,
}
