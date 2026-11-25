package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const mcpVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"inputSchema"`
}

func toolSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

var symbolInput = map[string]any{
	"type": "string", "minLength": 1, "description": "A name, qualified name, or symbol ID",
}

var mcpTools = []mcpTool{
	{"cadastre_search", "Search the indexed repository for code symbols.", toolSchema(map[string]any{
		"query": map[string]any{"type": "string", "minLength": 1},
		"mode":  map[string]any{"type": "string", "enum": []string{"hybrid", "lexical", "graph"}, "default": "hybrid"},
		"limit": map[string]any{"type": "integer", "minimum": 1, "default": 10},
	}, "query")},
	{"cadastre_callers", "Trace incoming or outgoing calls breadth first.", toolSchema(map[string]any{
		"symbol":  symbolInput,
		"depth":   map[string]any{"type": "integer", "minimum": 0, "default": 3},
		"callees": map[string]any{"type": "boolean", "default": false},
	}, "symbol")},
	{"cadastre_symbol", "Find exact symbols and return locations and source previews.", toolSchema(map[string]any{
		"symbol": symbolInput,
	}, "symbol")},
	{"cadastre_stats", "Show repository, file, symbol, and edge counts.", toolSchema(map[string]any{})},
}

type mcpServer struct {
	idx         *Index
	initialized bool
}

func serveMCP(in io.Reader, out io.Writer, idx *Index) error {
	server := mcpServer{idx: idx}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var response any
		switch {
		case !json.Valid(line):
			response = failure(nil, -32700, "Parse error")
		case line[0] == '[':
			var batch []json.RawMessage
			if err := json.Unmarshal(line, &batch); err != nil {
				return err
			}
			if len(batch) == 0 {
				response = failure(nil, -32600, "Invalid Request")
				break
			}
			var replies []*rpcReply
			for _, msg := range batch {
				if reply := server.handle(msg); reply != nil {
					replies = append(replies, reply)
				}
			}
			if len(replies) > 0 {
				response = replies
			}
		default:
			if reply := server.handle(line); reply != nil {
				response = reply
			}
		}
		if response != nil {
			if err := enc.Encode(response); err != nil {
				return fmt.Errorf("write MCP response: %w", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP message: %w", err)
	}
	return nil
}

func failure(id json.RawMessage, code int, msg string) *rpcReply {
	return &rpcReply{JSONRPC: "2.0", ID: id, Error: &rpcError{code, msg}}
}

func (s *mcpServer) handle(raw json.RawMessage) *rpcReply {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		return failure(nil, -32600, "Invalid Request")
	}
	if len(req.ID) == 0 {
		// The server has no work to perform for initialization and cancellation notifications.
		return nil
	}
	switch req.ID[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
	default:
		return failure(nil, -32600, "Invalid Request ID")
	}
	if len(req.Params) > 0 && req.Params[0] != '{' {
		return failure(req.ID, -32602, "Parameters must be an object")
	}
	var result any
	switch req.Method {
	case "initialize":
		if s.initialized {
			return failure(req.ID, -32600, "Session already initialized")
		}
		s.initialized = true
		result = map[string]any{
			"protocolVersion": mcpVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "cadastre", "version": version},
		}
	case "ping":
		result = map[string]any{}
	case "tools/list", "tools/call":
		if !s.initialized {
			return failure(req.ID, -32002, "Initialize the session first")
		}
		if req.Method == "tools/list" {
			var params struct {
				Cursor string `json:"cursor"`
			}
			if len(req.Params) != 0 && json.Unmarshal(req.Params, &params) != nil || params.Cursor != "" {
				return failure(req.ID, -32602, "Invalid tools cursor or parameters")
			}
			result = map[string]any{"tools": mcpTools}
		} else {
			var call struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			// MCP clients can attach metadata to a tool call.
			if err := json.Unmarshal(req.Params, &call); err != nil || call.Name == "" {
				return failure(req.ID, -32602, "Tool name is required")
			}
			value, err := s.callTool(call.Name, call.Arguments)
			if errors.Is(err, unknownTool) {
				return failure(req.ID, -32602, err.Error())
			}
			if err != nil {
				result = toolResult{Content: []toolContent{{"text", err.Error()}}, IsError: true}
			} else {
				data, err := json.Marshal(value)
				if err != nil {
					return failure(req.ID, -32603, "Could not encode tool result")
				}
				result = toolResult{Content: []toolContent{{"text", string(data)}}}
			}
		}
	default:
		return failure(req.ID, -32601, "Method not found")
	}
	return &rpcReply{JSONRPC: "2.0", ID: req.ID, Result: result}
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func decodeParams(raw json.RawMessage, args any) error {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] != '{' {
		return errors.New("arguments must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(args)
}

var unknownTool = errors.New("unknown tool")

func (s *mcpServer) callTool(name string, raw json.RawMessage) (any, error) {
	switch name {
	case "cadastre_search":
		args := struct {
			Query string `json:"query"`
			Mode  string `json:"mode"`
			Limit int    `json:"limit"`
		}{Mode: "hybrid", Limit: 10}
		if err := decodeParams(raw, &args); err != nil {
			return nil, err
		}
		return Search(s.idx, args.Query, SearchOptions{args.Mode, args.Limit})
	case "cadastre_callers":
		args := struct {
			Symbol  string `json:"symbol"`
			Depth   int    `json:"depth"`
			Callees bool   `json:"callees"`
		}{Depth: 3}
		if err := decodeParams(raw, &args); err != nil {
			return nil, err
		}
		return traceGraph(s.idx, args.Symbol, args.Depth, args.Callees)
	case "cadastre_symbol":
		var args struct {
			Symbol string `json:"symbol"`
		}
		if err := decodeParams(raw, &args); err != nil {
			return nil, err
		}
		ids := s.idx.lookup(args.Symbol)
		if len(ids) == 0 {
			return nil, fmt.Errorf("symbol %q not found", args.Symbol)
		}
		result := make([]Symbol, 0, len(ids))
		for _, id := range ids {
			result = append(result, s.idx.symbols[id].Symbol)
		}
		return result, nil
	case "cadastre_stats":
		if err := decodeParams(raw, &struct{}{}); err != nil {
			return nil, err
		}
		return s.idx.Stats(), nil
	default:
		return nil, fmt.Errorf("%w %q", unknownTool, name)
	}
}
