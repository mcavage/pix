// Shared helpers: env, JSON/HTTP convenience, and the MCP scaffolding (stdio
// dispatcher) used by the stdio MCP servers (e.g. slack).

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type jsonObj = map[string]any

func envRaw(k string) string { return os.Getenv(k) }

// postFormClient is the HTTP client used for form-encoded API calls (e.g. Slack).
// A 30-second timeout prevents indefinite hangs when the upstream accepts the
// TCP connection but never completes the response (H-2).
var postFormClient = &http.Client{Timeout: 30 * time.Second}

func httpPostForm(u, bearer string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := postFormClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return b, res.StatusCode, nil
}

func parseJSONObj(b []byte) (jsonObj, error) {
	var m jsonObj
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func getStr(m jsonObj, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func getStrOr(m jsonObj, key, def string) string {
	if s := getStr(m, key); s != "" {
		return s
	}
	return def
}

func getMap(m jsonObj, key string) jsonObj {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func clampInt(v any, def, lo, hi int) int {
	n := def
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case string:
		if p, err := strconv.Atoi(x); err == nil {
			n = p
		}
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return n
}

func itoaClamp(v any, def, lo, hi int) string { return strconv.Itoa(clampInt(v, def, lo, hi)) }

// --- MCP -------------------------------------------------------------------

type mcpTool struct {
	Name        string
	Description string
	Properties  jsonObj
	Required    []string
}

func (t mcpTool) schema() jsonObj {
	req := t.Required
	if req == nil {
		req = []string{}
	}
	props := t.Properties
	if props == nil {
		props = jsonObj{}
	}
	return jsonObj{"name": t.Name, "description": t.Description,
		"inputSchema": jsonObj{"type": "object", "properties": props, "required": req}}
}

func mcpErr(id any, code int, msg string) jsonObj {
	return jsonObj{"jsonrpc": "2.0", "id": id, "error": jsonObj{"code": code, "message": msg}}
}

func stripMeta(m jsonObj) jsonObj {
	out := jsonObj{}
	for k, v := range m {
		if k != "_meta" && v != nil {
			out[k] = v
		}
	}
	return out
}

// mcpDispatcher returns a handler: given a JSON-RPC message it returns the reply
// and whether there is one (notifications produce none).
func mcpDispatcher(serverName string, tools []mcpTool, handlers map[string]func(jsonObj) (any, error)) func(jsonObj) (jsonObj, bool) {
	return func(msg jsonObj) (jsonObj, bool) {
		method, _ := msg["method"].(string)
		id := msg["id"]
		switch method {
		case "notifications/initialized":
			return nil, false
		case "initialize":
			return jsonObj{"jsonrpc": "2.0", "id": id, "result": jsonObj{
				"protocolVersion": "2025-06-18", "capabilities": jsonObj{"tools": jsonObj{}},
				"serverInfo": jsonObj{"name": serverName, "version": "0.0.1"}}}, true
		case "tools/list":
			ts := []jsonObj{}
			for _, t := range tools {
				ts = append(ts, t.schema())
			}
			return jsonObj{"jsonrpc": "2.0", "id": id, "result": jsonObj{"tools": ts}}, true
		case "tools/call":
			params, _ := msg["params"].(map[string]any)
			name, _ := params["name"].(string)
			fn := handlers[name]
			if fn == nil {
				return mcpErr(id, -32601, "unknown tool: "+name), true
			}
			args, _ := params["arguments"].(map[string]any)
			if args == nil {
				args = jsonObj{}
			}
			res, err := fn(stripMeta(args))
			if err != nil {
				return mcpErr(id, -32603, err.Error()), true
			}
			text, _ := json.MarshalIndent(res, "", "  ")
			return jsonObj{"jsonrpc": "2.0", "id": id, "result": jsonObj{
				"content": []jsonObj{{"type": "text", "text": string(text)}}}}, true
		default:
			if id == nil {
				return nil, false
			}
			return mcpErr(id, -32601, "method not found"), true
		}
	}
}

// mcpStdio runs a dispatcher over stdio. The MCP stdio transport is
// newline-delimited JSON (one JSON-RPC message per line, no embedded newlines) —
// that's what the Docker sandboxes gateway speaks. We also tolerate Content-Length
// (LSP) framing on input and reply in whichever framing the peer used, so the same
// binary works behind the gateway or a Content-Length client.
// maxMCPFrameBytes bounds a single MCP frame so a hostile or buggy peer cannot
// force an unbounded allocation via Content-Length. 32 MiB dwarfs any real
// tool result.
const maxMCPFrameBytes = 32 << 20

func mcpStdio(handle func(jsonObj) (jsonObj, bool)) {
	reader := bufio.NewReader(os.Stdin)
	useContentLength := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			continue
		}

		var body []byte
		if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
			useContentLength = true
			length := 0
			fmt.Sscanf(strings.TrimSpace(trimmed[len("content-length:"):]), "%d", &length)
			// consume the rest of the headers up to the blank line
			for {
				h, herr := reader.ReadString('\n')
				if herr != nil {
					return
				}
				if strings.TrimRight(h, "\r\n") == "" {
					break
				}
			}
			if length <= 0 {
				continue
			}
			// Cap the frame before allocating: an arbitrary Content-Length from
			// the peer would otherwise let a single header trigger an unbounded
			// host allocation (memory exhaustion). 32 MiB is far above any real
			// MCP message.
			if length > maxMCPFrameBytes {
				return
			}
			body = make([]byte, length)
			if _, rerr := io.ReadFull(reader, body); rerr != nil {
				return
			}
		} else {
			body = []byte(trimmed) // newline-delimited JSON
		}

		var msg jsonObj
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		reply, ok := handle(msg)
		if !ok {
			continue
		}
		out, _ := json.Marshal(reply)
		if useContentLength {
			fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n", len(out))
			os.Stdout.Write(out)
		} else {
			os.Stdout.Write(append(out, '\n'))
		}
	}
}
