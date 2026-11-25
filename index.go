package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const indexFormat = 1

type Symbol struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	Language  string `json:"language"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Preview   string `json:"preview"`
}

type symbol struct {
	Symbol
	Terms  map[string]int `json:"terms"`
	Vector [128]int8      `json:"vector"`
	Calls  []callRef      `json:"calls,omitempty"`
}

type fileRecord struct {
	MTime   int64    `json:"mtime_ns"`
	Size    int64    `json:"size"`
	Hash    [32]byte `json:"sha256"`
	Package string   `json:"package,omitempty"`
	Binary  bool     `json:"binary,omitempty"`
	Symbols []symbol `json:"symbols"`
}

type Index struct {
	Format  int                   `json:"format"`
	Root    string                `json:"root"`
	Module  string                `json:"go_module,omitempty"`
	Files   map[string]fileRecord `json:"files"`
	Callers [][]int               `json:"callers"`
	Callees [][]int               `json:"callees"`

	symbols      []*symbol
	labels       map[string][]int
	ids          map[string]int
	postings     map[string][]posting
	length, norm []float64
	average      float64
}

type posting struct{ id, count int }

type Stats struct {
	Root    string `json:"root"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
	Edges   int    `json:"edges"`
}

type BuildStats struct {
	Stats
	Parsed  int `json:"parsed"`
	Reused  int `json:"reused"`
	Removed int `json:"removed"`
}

type fileStamp struct {
	MTime int64 `json:"mtime_ns"`
	Size  int64 `json:"size"`
}

type indexManifest struct {
	Format int `json:"format"`
	Stats
	Module string               `json:"go_module,omitempty"`
	Files  map[string]fileStamp `json:"stamps"`
}

type sourceFile struct {
	filename, rel, language string
	info                    fs.FileInfo
}

func BuildIndex(dir, output string, force bool) (BuildStats, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return BuildStats{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return BuildStats{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return BuildStats{}, err
	}
	if !info.IsDir() {
		return BuildStats{}, fmt.Errorf("%s is not a directory", dir)
	}
	out, err := filepath.Abs(output)
	if err != nil {
		return BuildStats{}, err
	}

	var manifest indexManifest
	var body *bufio.Reader
	if !force {
		f, err := os.Open(out)
		switch {
		case err == nil:
			defer f.Close()
			body = bufio.NewReader(f)
			manifest, err = readManifest(body)
			if err != nil {
				return BuildStats{}, fmt.Errorf("read index %q: %w (use --force to replace it)", out, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return BuildStats{}, err
		}
	}
	module, err := goModule(root)
	if err != nil {
		return BuildStats{}, err
	}
	sources, err := walkSources(root, out)
	if err != nil {
		return BuildStats{}, fmt.Errorf("index %s: %w", dir, err)
	}
	var old *Index
	if manifest.Root == root {
		if manifest.Module == module && manifest.matches(sources) {
			return BuildStats{Stats: manifest.Stats, Reused: manifest.Stats.Files}, nil
		}
		old, err = decodeIndex(body, manifest)
		if err != nil {
			return BuildStats{}, fmt.Errorf("read index %q: %w (use --force to replace it)", out, err)
		}
	}
	idx, result, err := updateIndex(root, module, old, sources)
	if err != nil {
		return BuildStats{}, fmt.Errorf("index %s: %w", dir, err)
	}
	if err := idx.save(out); err != nil {
		return BuildStats{}, err
	}
	return result, nil
}

func walkSources(root, output string) ([]sourceFile, error) {
	var sources []sourceFile
	ignores := make(map[string][]ignoreRule)
	err := filepath.WalkDir(root, func(filename string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rules := ignores[filepath.Dir(filename)]
		if d.IsDir() {
			if filename != root && (skippedDirs[d.Name()] || ignored(rules, filename, true)) {
				return filepath.SkipDir
			}
			rules, readErr := readIgnore(filename, rules)
			ignores[filename] = rules
			return readErr
		}
		language := languages[strings.ToLower(filepath.Ext(d.Name()))]
		if language == "" || filename == output || !d.Type().IsRegular() || ignored(rules, filename, false) {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sources = append(sources, sourceFile{filename, filepath.ToSlash(rel), language, info})
		return nil
	})
	return sources, err
}

func (f sourceFile) stamp() fileStamp {
	return fileStamp{f.info.ModTime().UnixNano(), f.info.Size()}
}

func (m indexManifest) matches(sources []sourceFile) bool {
	if len(m.Files) != len(sources) {
		return false
	}
	for _, source := range sources {
		stamp, ok := m.Files[source.rel]
		if !ok || stamp != source.stamp() {
			return false
		}
	}
	return true
}

func updateIndex(root, module string, old *Index, sources []sourceFile) (*Index, BuildStats, error) {
	idx := &Index{Format: indexFormat, Root: root, Module: module, Files: make(map[string]fileRecord, len(sources))}
	rebuildGraph := old == nil || old.Module != module
	var result BuildStats
	for _, source := range sources {
		var prior fileRecord
		var exists bool
		if old != nil {
			prior, exists = old.Files[source.rel]
		}
		stamp := source.stamp()
		if exists && prior.MTime == stamp.MTime && prior.Size == stamp.Size {
			idx.Files[source.rel] = prior
			if !prior.Binary {
				result.Reused++
			}
			continue
		}
		data, err := readSource(source.filename, source.info)
		if err != nil {
			return nil, BuildStats{}, err
		}
		record := fileRecord{MTime: stamp.MTime, Size: stamp.Size, Hash: sha256.Sum256(data)}
		switch {
		case exists && prior.Hash == record.Hash:
			record.Package, record.Symbols, record.Binary = prior.Package, prior.Symbols, prior.Binary
			if !record.Binary {
				result.Reused++
			}
		case bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data):
			record.Binary = true
			rebuildGraph = rebuildGraph || exists && !prior.Binary
		default:
			record.Package, record.Symbols = extract(source.rel, source.language, data)
			rebuildGraph = true
			result.Parsed++
		}
		idx.Files[source.rel] = record
	}
	if old != nil {
		for name, prior := range old.Files {
			if _, exists := idx.Files[name]; !exists && !prior.Binary {
				result.Removed++
				rebuildGraph = true
			}
		}
	}
	if rebuildGraph {
		idx.flatten()
		idx.buildGraph()
	} else {
		idx.Callers, idx.Callees = old.Callers, old.Callees
	}
	result.Stats = idx.Stats()
	return idx, result, nil
}
func readSource(filename string, before fs.FileInfo) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(before, actual) {
		return nil, fmt.Errorf("%s was replaced during indexing", filename)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() != int64(len(data)) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("%s changed during indexing; run index again", filename)
	}
	return data, nil
}

func goModule(root string) (string, error) {
	filename := filepath.Join(root, "go.mod")
	info, err := os.Lstat(filename)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			name := fields[1]
			if unquoted, err := strconv.Unquote(name); err == nil {
				name = unquoted
			}
			return name, nil
		}
	}
	return "", nil
}

func (idx *Index) flatten() {
	names := make([]string, 0, len(idx.Files))
	for name, record := range idx.Files {
		if len(record.Symbols) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		syms := idx.Files[name].Symbols
		for i := range syms {
			idx.symbols = append(idx.symbols, &syms[i])
		}
	}
}

// Resolution is deliberately conservative when a scope contains duplicate names.
func (idx *Index) buildGraph() {
	type key struct{ scope, name string }
	local := make(map[key][]int)
	global := make(map[key][]int)
	packagePath := func(s *symbol) string {
		return path.Dir(s.Path) + ":" + idx.Files[s.Path].Package
	}
	goImports := make(map[key][]int)
	for id, s := range idx.symbols {
		if s.Kind != "function" && s.Kind != "method" {
			continue
		}
		if s.Language == "go" {
			local[key{packagePath(s), s.Qualified}] = append(local[key{packagePath(s), s.Qualified}], id)
			if s.Kind == "function" {
				k := key{path.Dir(s.Path), s.Qualified}
				goImports[k] = append(goImports[k], id)
			}
			continue
		}
		k := key{s.Path, s.Qualified}
		local[k] = append(local[k], id)
		// JS/TS and the C family can call across extensions.
		k = key{languageGroup(s.Language), s.Qualified}
		global[k] = append(global[k], id)
	}
	idx.Callers = make([][]int, len(idx.symbols))
	idx.Callees = make([][]int, len(idx.symbols))
	for from, s := range idx.symbols {
		for _, call := range s.Calls {
			name := call.Name
			if call.Qualifier != "" {
				name = call.Qualifier + "." + name
			}
			var targets []int
			switch {
			case s.Language == "go" && call.Import == "":
				targets = local[key{packagePath(s), name}]
			case s.Language == "go":
				if idx.Module != "" {
					if call.Import == idx.Module {
						targets = goImports[key{".", name}]
					} else if dir, ok := strings.CutPrefix(call.Import, idx.Module+"/"); ok {
						targets = goImports[key{dir, name}]
					}
				}
			default:
				if call.Qualifier == "" {
					if i := strings.LastIndexByte(s.Qualified, '.'); i >= 0 {
						targets = local[key{s.Path, s.Qualified[:i+1] + name}]
					}
				}
				if len(targets) == 0 {
					targets = local[key{s.Path, name}]
				}
				if len(targets) == 0 {
					targets = global[key{languageGroup(s.Language), name}]
				}
			}
			if len(targets) == 1 {
				idx.Callees[from] = append(idx.Callees[from], targets[0])
			}
		}
		sort.Ints(idx.Callees[from])
		idx.Callees[from] = uniqueInts(idx.Callees[from])
		for _, to := range idx.Callees[from] {
			idx.Callers[to] = append(idx.Callers[to], from)
		}
	}
}

func uniqueInts(sorted []int) []int {
	n := 0
	for _, x := range sorted {
		if n == 0 || sorted[n-1] != x {
			sorted[n] = x
			n++
		}
	}
	return sorted[:n]
}

func languageGroup(language string) string {
	switch language {
	case "typescript", "javascript":
		return "js"
	case "c", "cpp", "header":
		return "c"
	default:
		return language
	}
}

func (idx *Index) save(filename string) error {
	f, err := os.CreateTemp(filepath.Dir(filename), ".cadastre-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	manifest := indexManifest{Format: idx.Format, Stats: idx.Stats(), Module: idx.Module,
		Files: make(map[string]fileStamp, len(idx.Files))}
	for name, rec := range idx.Files {
		manifest.Files[name] = fileStamp{rec.MTime, rec.Size}
	}
	if err := writeMember(f, manifest); err != nil {
		return err
	}
	if err := writeMember(f, idx); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filename)
}

func writeMember(w io.Writer, value any) error {
	zw := gzip.NewWriter(w)
	if err := json.NewEncoder(zw).Encode(value); err != nil {
		return err
	}
	return zw.Close()
}

func readIndex(filename string) (*Index, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open index %q: %w", filename, err)
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	manifest, err := readManifest(reader)
	if err != nil {
		return nil, fmt.Errorf("read index %q: %w", filename, err)
	}
	idx, err := decodeIndex(reader, manifest)
	if err != nil {
		return nil, fmt.Errorf("read index %q: %w", filename, err)
	}
	return idx, nil
}

func readMember(reader *bufio.Reader, value any) error {
	zr, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer zr.Close()
	zr.Multistream(false)
	dec := json.NewDecoder(zr)
	if err := dec.Decode(value); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("extra index data")
		}
		return err
	}
	return nil
}

func readManifest(reader *bufio.Reader) (indexManifest, error) {
	var m indexManifest
	if err := readMember(reader, &m); err != nil {
		return m, err
	}
	if m.Format != indexFormat || m.Root == "" || m.Files == nil {
		return m, errors.New("invalid or unsupported index format")
	}
	return m, nil
}

func decodeIndex(reader *bufio.Reader, manifest indexManifest) (*Index, error) {
	var idx Index
	if err := readMember(reader, &idx); err != nil {
		return nil, err
	}
	if idx.Format != indexFormat || idx.Module != manifest.Module || idx.Root != manifest.Root ||
		idx.Files == nil || idx.Stats() != manifest.Stats || len(idx.Files) != len(manifest.Files) {
		return nil, errors.New("index payload does not match its manifest")
	}
	if _, err := reader.Peek(1); err != io.EOF {
		if err == nil {
			return nil, errors.New("extra index members")
		}
		return nil, err
	}
	count := 0
	for name, rec := range idx.Files {
		stamp, ok := manifest.Files[name]
		if !ok || stamp.MTime != rec.MTime || stamp.Size != rec.Size {
			return nil, errors.New("file metadata does not match the manifest")
		}
		count += len(rec.Symbols)
	}
	for _, lists := range [][][]int{idx.Callers, idx.Callees} {
		if len(lists) != count {
			return nil, errors.New("invalid graph size")
		}
		for _, links := range lists {
			last := -1
			for _, id := range links {
				if id <= last || id >= count {
					return nil, errors.New("invalid graph edge")
				}
				last = id
			}
		}
	}
	return &idx, nil
}
func LoadIndex(filename string) (*Index, error) {
	idx, err := readIndex(filename)
	if err != nil {
		return nil, err
	}
	idx.flatten()
	idx.prepareSearch()
	return idx, nil
}

func (idx *Index) Stats() Stats {
	s := Stats{Root: idx.Root, Symbols: len(idx.Callees)}
	for _, record := range idx.Files {
		if !record.Binary {
			s.Files++
		}
	}
	for _, links := range idx.Callees {
		s.Edges += len(links)
	}
	return s
}

func (idx *Index) prepareSearch() {
	n := len(idx.symbols)
	idx.labels = make(map[string][]int)
	idx.ids = make(map[string]int, n)
	idx.postings = make(map[string][]posting)
	idx.length, idx.norm = make([]float64, n), make([]float64, n)
	for id, s := range idx.symbols {
		idx.ids[s.ID] = id
		for _, label := range []string{s.Qualified, s.Name} {
			k := strings.ToLower(label)
			ids := idx.labels[k]
			if len(ids) == 0 || ids[len(ids)-1] != id {
				idx.labels[k] = append(ids, id)
			}
		}
		for term, count := range s.Terms {
			idx.postings[term] = append(idx.postings[term], posting{id, count})
			idx.length[id] += float64(count)
		}
		idx.norm[id] = math.Sqrt(float64(dot(s.Vector, s.Vector)))
		idx.average += idx.length[id]
	}
	if n > 0 {
		idx.average /= float64(n)
	}
}

func (idx *Index) lookup(label string) []int {
	label = strings.TrimSpace(label)
	if id, ok := idx.ids[label]; ok {
		return []int{id}
	}
	return idx.labels[strings.ToLower(label)]
}

type SearchOptions struct {
	Mode  string
	Limit int
}

type Hit struct {
	Symbol Symbol  `json:"symbol"`
	Score  float64 `json:"score"`
	BM25   float64 `json:"bm25"`
	Cosine float64 `json:"cosine"`
	Graph  float64 `json:"graph"`
}

func Search(idx *Index, query string, opts SearchOptions) ([]Hit, error) {
	if opts.Limit < 1 {
		return nil, errors.New("limit must be at least 1")
	}
	switch opts.Mode {
	case "hybrid", "lexical", "graph":
	default:
		return nil, fmt.Errorf("unknown search mode %q", opts.Mode)
	}
	terms := make(map[string]int)
	words(query, func(word string) { terms[word] = 1 })
	if len(terms) == 0 {
		return nil, errors.New("query needs at least one word")
	}

	queryTerms := make([]string, 0, len(terms))
	for term := range terms {
		queryTerms = append(queryTerms, term)
	}
	sort.Strings(queryTerms)
	n := len(idx.symbols)
	bm25 := make([]float64, n)
	const k1, b = 1.2, 0.75
	maxBM25 := 0.0
	for _, term := range queryTerms {
		posts := idx.postings[term]
		df := float64(len(posts))
		idf := math.Log1p((float64(n) - df + 0.5) / (df + 0.5))
		for _, p := range posts {
			freq := float64(p.count)
			scale := k1 * (1 - b + b*idx.length[p.id]/idx.average)
			bm25[p.id] += idf * freq * (k1 + 1) / (freq + scale)
			maxBM25 = max(maxBM25, bm25[p.id])
		}
	}

	seed, neighbor := make([]float64, n), make([]float64, n)
	if opts.Mode != "lexical" {
		for id, score := range bm25 {
			if score > 0 {
				seed[id] = score / maxBM25
			}
		}
		for _, id := range idx.lookup(query) {
			seed[id] = 1.25
		}
		for from, score := range seed {
			if score < 0.25 {
				continue
			}
			for _, links := range [][]int{idx.Callers[from], idx.Callees[from]} {
				for _, to := range links {
					if from != to {
						neighbor[to] = max(neighbor[to], min(1, score))
					}
				}
			}
		}
	}

	queryVector := embedding(terms)
	queryNorm := math.Sqrt(float64(dot(queryVector, queryVector)))
	results := make([]Hit, 0)
	for id, s := range idx.symbols {
		hit := Hit{Symbol: s.Symbol, BM25: bm25[id]}
		if opts.Mode == "lexical" {
			hit.Score = hit.BM25
		} else {
			if seed[id] > 0 || neighbor[id] > 0 {
				degree := float64(2*len(idx.Callers[id]) + len(idx.Callees[id]))
				hit.Graph = 0.65*neighbor[id] + 0.35*degree/(degree+4)
			}
			if opts.Mode == "graph" {
				hit.Score = 0.45*seed[id] + 0.55*hit.Graph
			} else {
				if queryNorm > 0 && idx.norm[id] > 0 {
					hit.Cosine = float64(dot(queryVector, s.Vector)) / (queryNorm * idx.norm[id])
				}
				if hit.BM25 > 0 || hit.Cosine >= 0.22 || neighbor[id] > 0 || seed[id] > 0 {
					hit.Score = 0.58*seed[id] + 0.30*max(0, hit.Cosine) + 0.12*hit.Graph
				}
			}
		}
		if hit.Score > 0 {
			results = append(results, hit)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.Score == b.Score {
			return a.Symbol.ID < b.Symbol.ID
		}
		return a.Score > b.Score
	})
	return results[:min(len(results), opts.Limit)], nil
}

func dot(a, b [128]int8) int32 {
	var result int32
	for i, x := range a {
		result += int32(x) * int32(b[i])
	}
	return result
}

type TraceStep struct {
	Symbol Symbol `json:"symbol"`
	Via    Symbol `json:"via"`
	Depth  int    `json:"depth"`
}

func TraceCallers(idx *Index, symbol string, maxDepth int) ([]TraceStep, error) {
	return traceGraph(idx, symbol, maxDepth, false)
}

func TraceCallees(idx *Index, symbol string, maxDepth int) ([]TraceStep, error) {
	return traceGraph(idx, symbol, maxDepth, true)
}

func traceGraph(idx *Index, label string, maxDepth int, callees bool) ([]TraceStep, error) {
	if maxDepth < 0 {
		return nil, errors.New("depth must be nonnegative")
	}
	roots := idx.lookup(label)
	if len(roots) == 0 {
		return nil, fmt.Errorf("symbol %q not found", label)
	}
	adj := idx.Callers
	if callees {
		adj = idx.Callees
	}
	type node struct{ id, depth int }
	queue := make([]node, 0, len(roots))
	seen := make([]bool, len(idx.symbols))
	for _, id := range roots {
		seen[id] = true
		queue = append(queue, node{id, 0})
	}
	result := make([]TraceStep, 0)
	for head := 0; head < len(queue); head++ {
		from := queue[head]
		if from.depth == maxDepth {
			break
		}
		for _, id := range adj[from.id] {
			if seen[id] {
				continue
			}
			seen[id] = true
			depth := from.depth + 1
			queue = append(queue, node{id, depth})
			result = append(result, TraceStep{idx.symbols[id].Symbol, idx.symbols[from.id].Symbol, depth})
		}
	}
	return result, nil
}
