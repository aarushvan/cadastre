package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, source := range map[string]string{
		"go.mod": "module example.test/repo\n\ngo 1.23\n",
		"main.go": `package demo
import local "example.test/repo/lib"
type Worker[T any] struct{}
func (w *Worker[T]) Work() { leaf() }
func leaf() { cycle() }
func cycle() { leaf() }
func run() { var w Worker[int]; w.Work(); local.Remote() }
func normalizer() {}
`,
		"lib/service.go": "package lib\nfunc Remote() {}\n",
		"web.ts": `export function tsEntry() { tsLeaf(); }
const tsLeaf = () => 1;
class Flow {
  execute() { this.next(); }
  next() { tsEntry(); }
}
`,
		"script.py": `def py_entry():
    py_leaf()

def py_leaf(): return 1

class Runner:
    def run(self):
        self.step()

    def step(self):
        return py_entry()
`,
		"native.cpp":             "int helper() { return 1; }\nstruct Native {\n  int run() { return helper(); }\n};\n",
		"native.h":               "int helper(void);\n",
		"lib.rs":                 "fn target() {}\nstruct Rust {}\nimpl Rust { fn run(&self) { target(); } }\n",
		".gitignore":             "*.gen.go\n!keep.gen.go\nhidden/**/ignored.py\n",
		"skip.gen.go":            "package demo\nfunc Excluded() {}\n",
		"keep.gen.go":            "package demo\nfunc Kept() {}\n",
		"hidden/app/ignored.py":  "def invisible(): pass\n",
		"hidden/.gitignore":      "/local.ts\n",
		"hidden/local.ts":        "function localHidden() {}\n",
		"vendor/no.go":           "package vendor\nfunc Vendor() {}\n",
		"node_modules/module.js": "function Dependency() {}\n",
		"binary.ts":              "function binary() {}\x00",
	} {
		filename := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func indexFixture(t *testing.T) (*Index, string, string) {
	t.Helper()
	dir := fixture(t)
	filename := filepath.Join(dir, defaultIndex)
	if _, err := BuildIndex(dir, filename, false); err != nil {
		t.Fatal(err)
	}
	idx, err := LoadIndex(filename)
	if err != nil {
		t.Fatal(err)
	}
	return idx, dir, filename
}

func TestIndexLanguagesAndIncremental(t *testing.T) {
	idx, dir, filename := indexFixture(t)
	for _, label := range []string{"Worker", "Worker.Work", "tsEntry", "tsLeaf", "Flow.execute", "py_leaf", "Runner.run", "Native.run", "Rust.run", "Kept"} {
		if len(idx.labels[strings.ToLower(label)]) != 1 {
			t.Errorf("expected exactly one symbol named %s", label)
		}
	}
	foundPrototype := false
	for _, id := range idx.lookup("helper") {
		s := idx.symbols[id]
		foundPrototype = foundPrototype || s.Path == "native.h" && s.Kind == "declaration"
	}
	if !foundPrototype {
		t.Error("C header prototype was not indexed")
	}
	for _, label := range []string{"excluded", "invisible", "localhidden", "vendor", "dependency", "binary"} {
		if len(idx.labels[label]) != 0 {
			t.Errorf("indexed ignored symbol %s", label)
		}
	}
	before, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := BuildIndex(dir, filename, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Parsed != 0 || incremental.Reused != idx.Stats().Files || !os.SameFile(before, after) ||
		!before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("incremental no-op rewrote the index: %+v", incremental)
	}
	source := filepath.Join(dir, "web.ts")
	stamp := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	touched, err := BuildIndex(dir, filename, false)
	if err != nil || touched.Parsed != 0 || touched.Reused != incremental.Reused {
		t.Fatalf("hash reuse: %+v %v", touched, err)
	}
	if err := os.WriteFile(source, []byte("function changed() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "script.py")); err != nil {
		t.Fatal(err)
	}
	updated, err := BuildIndex(dir, filename, false)
	if err != nil || updated.Parsed != 1 || updated.Removed != 1 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	reloaded, err := LoadIndex(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.labels["changed"]) != 1 || len(reloaded.labels["tsentry"]) != 0 || len(reloaded.labels["py_leaf"]) != 0 {
		t.Fatal("changed or deleted symbols were not reconciled")
	}
	forced, err := BuildIndex(dir, filename, true)
	if err != nil || forced.Parsed != reloaded.Stats().Files || forced.Reused != 0 {
		t.Fatalf("force: %+v %v", forced, err)
	}
}

func TestHybridLexicalAndGraphRanking(t *testing.T) {
	idx, _, _ := indexFixture(t)
	lexical, err := Search(idx, "normalize", SearchOptions{"lexical", 10})
	if err != nil || len(lexical) != 0 {
		t.Fatalf("lexical: %+v %v", lexical, err)
	}
	hybrid, err := Search(idx, "normalize", SearchOptions{"hybrid", 10})
	if err != nil || len(hybrid) == 0 || hybrid[0].Symbol.Name != "normalizer" ||
		hybrid[0].BM25 != 0 || hybrid[0].Cosine <= 0 {
		t.Fatalf("hybrid did not rank the trigram match first: %+v %v", hybrid, err)
	}
	graph, err := Search(idx, "run", SearchOptions{"graph", 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range graph {
		if hit.Symbol.Qualified == "Remote" {
			if hit.Graph <= 0 || hit.BM25 != 0 {
				t.Fatalf("unexpected neighbor component: %+v", hit)
			}
			return
		}
	}
	t.Fatal("graph ranking omitted a pure graph neighbor")
}

func TestTraceCallersAndCallees(t *testing.T) {
	idx, _, _ := indexFixture(t)
	for _, tc := range []struct {
		start   string
		callees bool
		depth   int
		names   []string
		depths  []int
	}{
		{"leaf", false, 2, []string{"Worker.Work", "cycle", "run"}, []int{1, 1, 2}},
		{"main.go:7:run", true, 3, []string{"Remote", "Worker.Work", "leaf", "cycle"}, []int{1, 1, 2, 3}},
		{"run", true, 0, nil, nil},
		{"tsLeaf", false, 3, []string{"tsEntry", "Flow.next", "Flow.execute"}, []int{1, 2, 3}},
		{"Runner.run", true, 3, []string{"Runner.step", "py_entry", "py_leaf"}, []int{1, 2, 3}},
		{"Native.run", true, 1, []string{"helper"}, []int{1}},
		{"Rust.run", true, 1, []string{"target"}, []int{1}},
	} {
		var got []TraceStep
		var err error
		if tc.callees {
			got, err = TraceCallees(idx, tc.start, tc.depth)
		} else {
			got, err = TraceCallers(idx, tc.start, tc.depth)
		}
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		var depths []int
		for _, step := range got {
			names = append(names, step.Symbol.Qualified)
			depths = append(depths, step.Depth)
		}
		if !reflect.DeepEqual(names, tc.names) || !reflect.DeepEqual(depths, tc.depths) {
			t.Errorf("%s outgoing=%v: got %v %v, want %v %v", tc.start, tc.callees, names, depths, tc.names, tc.depths)
		}
	}
}

func TestCLIInterspersedFlags(t *testing.T) {
	dir := fixture(t)
	filename := filepath.Join(dir, defaultIndex)
	var out, stderr bytes.Buffer
	if err := run([]string{"index", "--json", dir, "--out", filename}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"query", "Worker.Work", "--index", filename, "--limit=1", "--json"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var hits []Hit
	if err := json.Unmarshal(out.Bytes(), &hits); err != nil || len(hits) != 1 || hits[0].Symbol.Qualified != "Worker.Work" {
		t.Fatalf("unexpected output %s (%v)", out.String(), err)
	}
}

func TestMCPOverPipes(t *testing.T) {
	idx, _, _ := indexFixture(t)
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer serverOut.Close()
		defer serverIn.Close()
		done <- serveMCP(serverIn, serverOut, idx)
	}()
	t.Cleanup(func() {
		clientOut.Close()
		clientIn.Close()
	})
	lines := bufio.NewReader(clientIn)
	send := func(request string) {
		t.Helper()
		if _, err := io.WriteString(clientOut, request+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	rpc := func(request string, wantID string, result any) {
		t.Helper()
		send(request)
		frame, err := lines.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(frame, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.JSONRPC != "2.0" || string(reply.ID) != wantID || reply.Error != nil {
			t.Fatalf("unexpected response %s", frame)
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			t.Fatal(err)
		}
	}
	var init struct {
		Version      string                     `json:"protocolVersion"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Server       map[string]string          `json:"serverInfo"`
	}
	rpc(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`"init"`, &init)
	if init.Version != mcpVersion || init.Server["name"] != "cadastre" || init.Capabilities["tools"] == nil {
		t.Fatalf("invalid handshake %+v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var tools struct {
		Tools []mcpTool `json:"tools"`
	}
	rpc(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "1", &tools)
	var names []string
	for _, tool := range tools.Tools {
		if tool.Schema["type"] != "object" {
			t.Errorf("bad tool schema for %s", tool.Name)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"cadastre_callers", "cadastre_search", "cadastre_stats", "cadastre_symbol"}) {
		t.Fatalf("wrong tools: %v", names)
	}
	for _, call := range []struct{ name, args, contains string }{
		{"cadastre_search", `{"query":"normalizer"}`, `"name":"normalizer"`},
		{"cadastre_callers", `{"symbol":"leaf","depth":1}`, `"qualified":"Worker.Work"`},
		{"cadastre_symbol", `{"symbol":"Runner.step"}`, `"path":"script.py"`},
		{"cadastre_stats", `{}`, `"edges":`},
	} {
		var result toolResult
		rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+call.name+`","arguments":`+call.args+`}}`, "2", &result)
		if result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" ||
			!strings.Contains(result.Content[0].Text, call.contains) {
			t.Fatalf("%s: %+v", call.name, result)
		}
	}
	var pong map[string]any
	rpc(`{"jsonrpc":"2.0","id":3,"method":"ping"}`, "3", &pong)
	if err := clientOut.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not exit on EOF")
	}
}
