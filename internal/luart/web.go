package luart

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const maxWebBody = 8 << 20

var webClient = &http.Client{}

// web.request{url, method?, headers?, body?, json?, timeout?}
func (v *vm) webRequest(L *lua.LState) int {
	t, ok := L.Get(1).(*lua.LTable)
	if !ok {
		raise(L, "web.request", EINVAL, "expected a table {url=...}")
	}
	return v.doRequest(L, "web.request", t)
}

func (v *vm) webGet(L *lua.LState) int  { return v.sugar(L, "web.get", "GET") }
func (v *vm) webPost(L *lua.LState) int { return v.sugar(L, "web.post", "POST") }

func (v *vm) sugar(L *lua.LState, name, method string) int {
	url := L.CheckString(1)
	o := L.NewTable()
	opts(L, 2).ForEach(func(k, val lua.LValue) { o.RawSet(k, val) })
	o.RawSetString("url", lua.LString(url))
	o.RawSetString("method", lua.LString(method))
	return v.doRequest(L, name, o)
}

func (v *vm) doRequest(L *lua.LState, name string, o *lua.LTable) int {
	url, _ := optString(o, "url")
	if url == "" {
		raise(L, name, EINVAL, "url is required")
	}
	method := "GET"
	if m, ok := optString(o, "method"); ok {
		method = strings.ToUpper(m)
	}
	var body io.Reader
	contentType := ""
	if j := o.RawGetString("json"); j != lua.LNil {
		text, err := encodeJSON(j, "")
		if err != nil {
			raise(L, name, EINVAL, "json: %v", err)
		}
		body, contentType = strings.NewReader(text), "application/json"
		if method == "GET" && o.RawGetString("method") == lua.LNil {
			method = "POST"
		}
	} else if s, ok := optString(o, "body"); ok {
		body = strings.NewReader(s)
	}
	ctx := v.ctx
	if n, ok := optNumber(o, "timeout"); ok && n > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(n*float64(time.Second)))
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		raise(L, name, EINVAL, "%v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if h, ok := o.RawGetString("headers").(*lua.LTable); ok {
		h.ForEach(func(k, val lua.LValue) { req.Header.Set(k.String(), val.String()) })
	}
	res := L.NewTable()
	resp, err := webClient.Do(req)
	if err != nil {
		if v.ctx.Err() != nil {
			v.ctxErr(name)
		}
		res.RawSetString("ok", lua.LFalse)
		res.RawSetString("status", lua.LNumber(0))
		res.RawSetString("headers", L.NewTable())
		res.RawSetString("body", lua.LString(""))
		res.RawSetString("error", lua.LString(err.Error()))
		L.Push(res)
		return 1
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, io.LimitReader(resp.Body, maxWebBody))
	headers := L.NewTable()
	for k, vals := range resp.Header {
		headers.RawSetString(strings.ToLower(k), lua.LString(strings.Join(vals, ", ")))
	}
	res.RawSetString("ok", lua.LBool(resp.StatusCode/100 == 2))
	res.RawSetString("status", lua.LNumber(resp.StatusCode))
	res.RawSetString("headers", headers)
	res.RawSetString("body", lua.LString(buf.String()))
	if err != nil {
		if v.ctx.Err() != nil {
			v.ctxErr(name)
		}
		res.RawSetString("ok", lua.LFalse)
		res.RawSetString("error", lua.LString(err.Error()))
	}
	L.Push(res)
	return 1
}
