package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var languages = map[string]string{
	".go": "go", ".ts": "typescript", ".tsx": "typescript",
	".js": "javascript", ".mjs": "javascript", ".py": "python", ".rs": "rust",
	".c": "c", ".cpp": "cpp", ".h": "header",
}

var skippedDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, "vendor": true,
}

type ignoreRule struct {
	base              string
	pattern           *regexp.Regexp
	directory, negate bool
}

func readIgnore(dir string, inherited []ignoreRule) ([]ignoreRule, error) {
	file := filepath.Join(dir, ".gitignore")
	info, err := os.Lstat(file)
	if os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() {
		return inherited, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	rules := append([]ignoreRule(nil), inherited...)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
			line = strings.TrimSuffix(line, " ")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		directory := strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")
		if line == "" {
			continue
		}
		prefix := "^"
		if !anchored && !strings.Contains(line, "/") {
			prefix = "(?:^|/)"
		}
		re, err := regexp.Compile(prefix + globRegex(line) + "$")
		if err == nil {
			rules = append(rules, ignoreRule{dir, re, directory, negate})
		}
	}
	return rules, nil
}

func globRegex(pattern string) string {
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			start := i
			for i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
			}
			if i > start && (start == 0 || pattern[start-1] == '/') {
				switch {
				case i+1 == len(pattern):
					out.WriteString(".*")
					continue
				case pattern[i+1] == '/':
					out.WriteString("(?:.*/)?")
					i++
					continue
				}
			}
			out.WriteString("[^/]*")
		case '?':
			out.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end > 0 {
				class := pattern[i+1 : i+1+end]
				if strings.HasPrefix(class, "!") {
					class = "^/" + class[1:]
				}
				out.WriteString("[" + class + "]")
				i += end + 1
			} else {
				out.WriteString("\\[")
			}
		case '\\':
			if i+1 < len(pattern) {
				i++
			}
			out.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		default:
			out.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	return out.String()
}

func ignored(rules []ignoreRule, filename string, directory bool) bool {
	skip := false
	for _, rule := range rules {
		if rule.directory && !directory {
			continue
		}
		rel, err := filepath.Rel(rule.base, filename)
		if err == nil && rule.pattern.MatchString(filepath.ToSlash(rel)) {
			skip = !rule.negate
		}
	}
	return skip
}

type callRef struct {
	Name      string `json:"name"`
	Qualifier string `json:"qualifier,omitempty"`
	Import    string `json:"import,omitempty"`
}

func extract(rel, language string, src []byte) (string, []symbol) {
	if language == "go" {
		return extractGo(rel, src)
	}
	return "", extractLexical(rel, language, src)
}

func extractGo(rel string, src []byte) (string, []symbol) {
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if file == nil || file.Name == nil {
		return "", nil
	}
	imports := make(map[string]string)
	for _, imp := range file.Imports {
		target, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(target)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		if alias != "_" && alias != "." {
			imports[alias] = target
		}
	}
	var result []symbol
	add := func(name, qualified, kind string, node ast.Node, doc *ast.CommentGroup, calls []callRef) {
		start, end := fset.PositionFor(node.Pos(), false), fset.PositionFor(node.End(), false)
		if start.Line == 0 || start.Offset > end.Offset || end.Offset > len(src) {
			return
		}
		body := src[start.Offset:end.Offset]
		result = append(result, makeSymbol(rel, "go", name, qualified, kind, start.Line, end.Line, body, doc.Text(), calls))
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name, kind := d.Name.Name, "function"
			qualified := name
			if d.Recv != nil && len(d.Recv.List) != 0 {
				kind = "method"
				if receiver := goTypeName(d.Recv.List[0].Type); receiver != "" {
					qualified = receiver + "." + name
				}
			}
			var calls []callRef
			if d.Body != nil {
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if ref := goCall(call.Fun, imports); ref.Name != "" {
							calls = append(calls, ref)
						}
					}
					return true
				})
			}
			add(name, qualified, kind, d, d.Doc, calls)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if t, ok := spec.(*ast.TypeSpec); ok {
					doc := t.Doc
					if doc == nil {
						doc = d.Doc
					}
					add(t.Name.Name, t.Name.Name, "type", t, doc, nil)
					if it, ok := t.Type.(*ast.InterfaceType); ok {
						for _, field := range it.Methods.List {
							for _, method := range field.Names {
								add(method.Name, t.Name.Name+"."+method.Name, "method", field, field.Doc, nil)
							}
						}
					}
				}
			}
		}
	}
	return file.Name.Name, result
}

func goTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return goTypeName(t.X)
	case *ast.ParenExpr:
		return goTypeName(t.X)
	case *ast.IndexExpr:
		return goTypeName(t.X)
	case *ast.IndexListExpr:
		return goTypeName(t.X)
	}
	return ""
}

func goLocalType(expr ast.Expr) string {
	if id, ok := expr.(*ast.Ident); ok && id.Obj != nil {
		switch d := id.Obj.Decl.(type) {
		case *ast.Field:
			return goTypeName(d.Type)
		case *ast.ValueSpec:
			if d.Type != nil {
				return goTypeName(d.Type)
			}
			for i, name := range d.Names {
				if name.Name == id.Name && i < len(d.Values) {
					return goInitializerType(d.Values[i])
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range d.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == id.Name && i < len(d.Rhs) {
					return goInitializerType(d.Rhs[i])
				}
			}
		case *ast.TypeSpec:
			return id.Name
		}
	}
	return goInitializerType(expr)
}

func goInitializerType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		return goTypeName(e.Type)
	case *ast.UnaryExpr:
		if lit, ok := e.X.(*ast.CompositeLit); ok {
			return goTypeName(lit.Type)
		}
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "new" && id.Obj == nil && len(e.Args) == 1 {
			return goTypeName(e.Args[0])
		}
	}
	return ""
}

func goCall(expr ast.Expr, imports map[string]string) callRef {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return goCall(e.X, imports)
	case *ast.IndexExpr:
		if id, ok := e.X.(*ast.Ident); ok && id.Obj != nil && id.Obj.Kind == ast.Var {
			return callRef{}
		}
		return goCall(e.X, imports)
	case *ast.IndexListExpr:
		return goCall(e.X, imports)
	case *ast.Ident:
		if e.Obj == nil || e.Obj.Kind == ast.Fun {
			return callRef{Name: e.Name}
		}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok && id.Obj == nil && imports[id.Name] != "" {
			return callRef{Name: e.Sel.Name, Import: imports[id.Name]}
		}
		if receiver := goLocalType(e.X); receiver != "" {
			return callRef{Name: e.Sel.Name, Qualifier: receiver}
		}
	}
	return callRef{}
}

const ident = `[A-Za-z_$][\w$]*`

type declPattern struct {
	re          *regexp.Regexp
	kind, shape string
}

func declaration(pattern, kind, shape string) declPattern {
	return declPattern{regexp.MustCompile(pattern), kind, shape}
}

var jsDecls = []declPattern{
	declaration(`\bfunction(?:[ \t]*\*[ \t]*|[ \t]+)(`+ident+`)(?:[ \t]*<[^<>\n]*>)?[ \t]*\(`, "function", "brace"),
	declaration(`\b(?:const|let|var)[ \t]+(`+ident+`)(?:[ \t]*:[^=\n;]+)?[ \t]*=[ \t]*(?:async[ \t]+)?(?:\(|`+ident+`[ \t]*=>)`, "function", "arrow"),
	declaration(`\b(?:class|interface|enum)[ \t]+(`+ident+`)[^;{\n]*\{`, "type", "scope"),
	declaration(`\btype[ \t]+(`+ident+`)(?:[ \t]*<[^<>\n]*>)?[ \t]*=`, "type", "line"),
	declaration(`(?m)(?:^[ \t]*|[;{}][ \t]*)(?:(?:public|private|protected|static|async|readonly|override|get|set)[ \t]+)*\*?(`+ident+`)(?:[ \t]*<[^<>\n]*>)?[ \t]*\(`, "method", "member"),
}

var pyDecls = []declPattern{
	declaration(`(?m)^[ \t]*(?:async[ \t]+)?def[ \t]+(`+ident+`)(?:\[[^\]\n]*\])?[ \t]*\(`, "function", "indent"),
	declaration(`(?m)^[ \t]*class[ \t]+(`+ident+`)(?:\[[^\]\n]*\])?[ \t]*[(:]`, "type", "indent"),
}

var rustDecls = []declPattern{
	declaration(`\bfn[ \t]+(`+ident+`)(?:[ \t]*<[^<>\n]*>)?[ \t]*\(`, "function", "brace"),
	declaration(`\b(?:struct|enum|trait|union|mod)[ \t]+(`+ident+`)[^;{\n]*\{`, "type", "scope"),
	declaration(`\b(?:struct|type|mod)[ \t]+(`+ident+`)[^;{\n]*;`, "type", "line"),
	declaration(`\bimpl(?:[ \t]*<[^<>\n]*>)?[ \t]+(?:[^{\n]+[ \t]+for[ \t]+)?(`+ident+`)[^{\n]*\{`, "", "scope"),
}

const cFunction = `(?m)(?:^[ \t]*|[;{}][ \t]*)(?:(?:[A-Za-z_][\w:]*|[*&]+)[ \t\n]+)+[*& \t]*(` + ident + `(?:::[~]?[A-Za-z_]\w*)*)[ \t]*\(`

var headerPrototype = declaration(cFunction, "declaration", "prototype")

var cDecls = []declPattern{
	declaration(cFunction, "function", "brace"),
	declaration(`(?m)(?:^[ \t]*|[;{}][ \t]*)(`+ident+`::[~]?[A-Za-z_]\w*)[ \t]*\(`, "method", "brace"),
	declaration(`\b(?:struct|class|enum(?:[ \t]+class)?|union|namespace)[ \t]+(`+ident+`)[^;{\n]*\{`, "type", "scope"),
	declaration(`\b(?:using[ \t]+(`+ident+`)[ \t]*=|typedef[ \t]+[^;{}\n]+[ \t*](`+ident+`)[ \t]*;)`, "type", "line"),
}

var lexicalCalls = regexp.MustCompile(`(` + ident + `(?:[ \t]*(?:\?\.|\.|::|->)[ \t]*` + ident + `)*)[ \t]*\(`)
var callParts = regexp.MustCompile(ident)
var cNoise = regexp.MustCompile(`(?s)/\*.*?(?:\*/|$)|//[^\n]*|"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*'|\x60(?:\\.|[^\x60\\])*\x60`)
var pyNoise = regexp.MustCompile(`(?s)#[^\n]*|""".*?(?:"""|$)|'''.*?(?:'''|$)|"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*'`)
var controlWords = strings.Fields("if else for foreach while switch catch with match return throw sizeof alignof decltype static_assert assert class function def fn super")

type lexicalDecl struct {
	name, qualified, kind string
	start, body, end      int
	scope                 bool
	calls                 []callRef
}

func extractLexical(rel, language string, src []byte) []symbol {
	patterns, noise := cDecls, cNoise
	switch language {
	case "typescript", "javascript":
		patterns = jsDecls
	case "python":
		patterns, noise = pyDecls, pyNoise
	case "rust":
		patterns = rustDecls
	case "header":
		patterns = append([]declPattern{headerPrototype}, cDecls...)
	}
	code := bytes.Clone(src)
	for _, span := range noise.FindAllIndex(src, -1) {
		for i := span[0]; i < span[1]; i++ {
			if code[i] != '\n' && code[i] != '\r' {
				code[i] = ' '
			}
		}
	}
	var starts = []int{0}
	for i, c := range code {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	line := func(offset int) int {
		return sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
	}
	var declarations []lexicalDecl
	for _, pattern := range patterns {
		for _, m := range pattern.re.FindAllSubmatchIndex(code, -1) {
			nameAt, nameEnd := m[2], m[3]
			if nameAt < 0 {
				nameAt, nameEnd = m[4], m[5]
			}
			name := strings.ReplaceAll(string(code[nameAt:nameEnd]), "::", ".")
			if nonCall(name) {
				continue
			}
			body, end, ok := lexicalBounds(code, m[1], nameAt, pattern.shape)
			if !ok {
				continue
			}
			start := m[0]
			if strings.ContainsRune(";{}", rune(code[start])) {
				start++
			}
			declarations = append(declarations, lexicalDecl{
				name: name, qualified: name, kind: pattern.kind, start: start,
				body: body, end: end, scope: pattern.shape == "scope",
			})
		}
	}
	sort.SliceStable(declarations, func(i, j int) bool {
		a, b := declarations[i], declarations[j]
		if a.start == b.start {
			return a.end > b.end
		}
		return a.start < b.start
	})
	var decls []*lexicalDecl
	var parents []*lexicalDecl
	for i := range declarations {
		d := &declarations[i]
		for len(parents) > 0 && d.start >= parents[len(parents)-1].end {
			parents = parents[:len(parents)-1]
		}
		if len(parents) > 0 {
			parent := parents[len(parents)-1]
			if d.end > parent.end || d.body == parent.body && d.end == parent.end ||
				d.kind == "declaration" && !parent.scope ||
				(language == "typescript" || language == "javascript") && d.kind == "method" && !parent.scope {
				continue
			}
			if parent.scope || language == "python" {
				d.qualified = parent.qualified + "." + d.name
				if d.kind == "function" && parent.scope || language == "python" && parent.kind == "type" && d.kind == "function" {
					d.kind = "method"
				}
			}
		} else if (language == "typescript" || language == "javascript") && d.kind == "method" {
			continue
		}
		if last := strings.LastIndexByte(d.name, '.'); last >= 0 {
			d.name = d.name[last+1:]
			if d.kind == "function" {
				d.kind = "method"
			}
		}
		decls = append(decls, d)
		parents = append(parents, d)
	}
	parents = parents[:0]
	next := 0
	for _, m := range lexicalCalls.FindAllSubmatchIndex(code, -1) {
		for next < len(decls) && decls[next].start <= m[0] {
			for len(parents) > 0 && decls[next].start >= parents[len(parents)-1].end {
				parents = parents[:len(parents)-1]
			}
			parents = append(parents, decls[next])
			next++
		}
		for len(parents) > 0 && m[0] >= parents[len(parents)-1].end {
			parents = parents[:len(parents)-1]
		}
		if len(parents) == 0 {
			continue
		}
		d := parents[len(parents)-1]
		if m[0] < d.body || d.kind != "function" && d.kind != "method" {
			continue
		}
		parts := callParts.FindAllString(string(code[m[2]:m[3]]), -1)
		name := parts[len(parts)-1]
		if nonCall(name) {
			continue
		}
		qualifier := strings.Join(parts[:len(parts)-1], ".")
		if qualifier == "this" || qualifier == "self" || qualifier == "Self" {
			if last := strings.LastIndexByte(d.qualified, '.'); last >= 0 {
				qualifier = d.qualified[:last]
			}
		}
		d.calls = append(d.calls, callRef{Name: name, Qualifier: qualifier})
	}
	var result []symbol
	for _, d := range decls {
		if d.kind != "" {
			result = append(result, makeSymbol(rel, language, d.name, d.qualified, d.kind,
				line(d.start), line(d.end-1), src[d.start:d.end], "", d.calls))
		}
	}
	return result
}

func nonCall(name string) bool {
	for _, word := range controlWords {
		if name == word {
			return true
		}
	}
	return false
}

// lexicalBounds returns a half-open body range. Source masking preserves offsets.
func lexicalBounds(code []byte, after, nameAt int, shape string) (int, int, bool) {
	if shape == "line" {
		end := lineEnd(code, after)
		return end, end, true
	}
	if shape == "scope" {
		end := matching(code, after-1, '{', '}')
		return after, end, end > 0
	}
	at := after
	if code[after-1] == '(' {
		at = matching(code, after-1, '(', ')')
		if at < 0 {
			return 0, 0, false
		}
	}
	if shape == "indent" {
		if code[after-1] == ':' {
			at = after - 1
		} else {
			for at < len(code) && code[at] != ':' && code[at] != '\n' {
				at++
			}
		}
		if at == len(code) || code[at] != ':' {
			return 0, 0, false
		}
		column := indent(code[bytes.LastIndexByte(code[:nameAt], '\n')+1:])
		end := lineEnd(code, at)
		last := end
		for end < len(code) {
			start := end + 1
			end = lineEnd(code, start)
			if len(bytes.TrimSpace(code[start:end])) == 0 {
				continue
			}
			if indent(code[start:end]) <= column {
				break
			}
			last = end
		}
		return at + 1, last, true
	}

	if shape == "arrow" && code[after-1] != '>' {
		next := code[at:lineEnd(code, at)]
		i := bytes.Index(next, []byte("=>"))
		if i < 0 {
			return 0, 0, false
		}
		prefix := bytes.TrimSpace(next[:i])
		if len(prefix) > 0 && prefix[0] != ':' {
			return 0, 0, false
		}
		at += i + 2
	}
	if shape == "arrow" {
		for at < len(code) && (code[at] == ' ' || code[at] == '\t' || code[at] == '\n' || code[at] == '\r') {
			at++
		}
		if at == len(code) || code[at] != '{' {
			return at, lineEnd(code, at), at < len(code)
		}
	} else {
		for at < len(code) && !strings.ContainsRune("{;=()}", rune(code[at])) {
			at++
		}
		if shape == "prototype" {
			end := at + 1
			return end, end, at < len(code) && code[at] == ';'
		}
		if at == len(code) || code[at] != '{' {
			return 0, 0, false
		}
	}
	end := matching(code, at, '{', '}')
	return at + 1, end, end > 0
}

func matching(code []byte, start int, open, close byte) int {
	depth := 0
	for i := start; i < len(code); i++ {
		switch code[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func lineEnd(code []byte, start int) int {
	if next := bytes.IndexByte(code[start:], '\n'); next >= 0 {
		return start + next
	}
	return len(code)
}

func indent(line []byte) int {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 8 - n%8
		default:
			return n
		}
	}
	return n
}

func makeSymbol(rel, language, name, qualified, kind string, line, end int, body []byte, doc string, calls []callRef) symbol {
	terms := make(map[string]int)
	words(string(body), func(word string) { terms[word]++ })
	words(doc, func(word string) { terms[word]++ })
	words(qualified, func(word string) { terms[word] += 4 })
	preview := string(body)
	if len(preview) > 512 {
		end := 512
		for !utf8.RuneStart(preview[end]) {
			end--
		}
		preview = preview[:end]
	}
	return symbol{
		Symbol: Symbol{ID: rel + ":" + strconv.Itoa(line) + ":" + qualified, Name: name, Qualified: qualified,
			Kind: kind, Language: language, Path: rel, Line: line, EndLine: end, Preview: strings.TrimSpace(preview)},
		Terms: terms, Vector: embedding(terms), Calls: calls,
	}
}

// words emits case-folded identifier components, including acronym boundaries.
func words(text string, emit func(string)) {
	start, prev := 0, rune(0)
	active := false
	for i, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			if active {
				emit(strings.ToLower(text[start:i]))
			}
			active = false
			continue
		}
		if !active {
			start, active = i, true
		} else if unicode.IsUpper(r) {
			next, _ := utf8.DecodeRuneInString(text[i+utf8.RuneLen(r):])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || unicode.IsUpper(prev) && unicode.IsLower(next) {
				emit(strings.ToLower(text[start:i]))
				start = i
			}
		}
		prev = r
	}
	if active {
		emit(strings.ToLower(text[start:]))
	}
}

func embedding(terms map[string]int) [128]int8 {
	var bins [128]int
	add := func(feature string, domain uint32, weight int) {
		hash := uint32(2166136261) ^ domain
		for i := 0; i < len(feature); i++ {
			hash = (hash ^ uint32(feature[i])) * 16777619
		}
		// Mix high bits into the low bits before taking the bucket.
		hash ^= hash >> 16
		if hash&(1<<31) != 0 {
			weight = -weight
		}
		bins[hash&127] += weight
	}
	for term, count := range terms {
		weight := min(count, 4)
		add(term, 1, weight*2)
		runes := []rune("^" + term + "$")
		for i := 0; i+2 < len(runes); i++ {
			add(string(runes[i:i+3]), 2, weight)
		}
	}
	var result [128]int8
	scale := 0
	for _, n := range bins {
		scale = max(scale, n, -n)
	}
	if scale != 0 {
		for i, n := range bins {
			result[i] = int8(math.Round(float64(n) * 127 / float64(scale)))
		}
	}
	return result
}
