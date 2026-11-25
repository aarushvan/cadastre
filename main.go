package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

var version = "v0.1.0"

const defaultIndex = ".cadastre.idx"

const usage = `Usage:
  cadastre index <dir> [--out .cadastre.idx] [--force] [--json]
  cadastre query <text> [--index .cadastre.idx] [--mode hybrid|lexical|graph] [--limit 10] [--json]
  cadastre callers <symbol> [--index .cadastre.idx] [--depth 3] [--callees] [--json]
  cadastre stats [--index .cadastre.idx] [--json]
  cadastre mcp [--index .cadastre.idx]
  cadastre version
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "cadastre:", err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := io.WriteString(out, usage)
		return err
	case "index", "query", "callers", "stats", "mcp", "version":
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}

	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	indexPath, mode := defaultIndex, "hybrid"
	limit, depth := 10, 3
	var jsonOutput, force, callees bool
	wantOperand := false
	switch command {
	case "index":
		flags.StringVar(&indexPath, "out", defaultIndex, "output index")
		flags.BoolVar(&force, "force", false, "rebuild every file")
		wantOperand = true
	case "query":
		flags.StringVar(&mode, "mode", "hybrid", "search mode")
		flags.IntVar(&limit, "limit", 10, "result limit")
		wantOperand = true
	case "callers":
		flags.IntVar(&depth, "depth", 3, "maximum traversal depth")
		flags.BoolVar(&callees, "callees", false, "follow outgoing calls")
		wantOperand = true
	}
	switch command {
	case "query", "callers", "stats", "mcp":
		flags.StringVar(&indexPath, "index", defaultIndex, "index to read")
	}
	switch command {
	case "index", "query", "callers", "stats":
		flags.BoolVar(&jsonOutput, "json", false, "write JSON")
	}

	operand, err := parseFlags(flags, args[1:], wantOperand)
	if errors.Is(err, flag.ErrHelp) {
		_, err = io.WriteString(out, usage)
		return err
	}
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	switch command {
	case "version":
		_, err := fmt.Fprintln(out, version)
		return err
	case "index":
		s, err := BuildIndex(operand, indexPath, force)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(s)
		}
		_, err = fmt.Fprintf(out, "%d files, %d symbols, %d edges (%d parsed, %d reused, %d removed)\n",
			s.Files, s.Symbols, s.Edges, s.Parsed, s.Reused, s.Removed)
		return err
	}
	idx, err := LoadIndex(indexPath)
	if err != nil {
		return err
	}
	switch command {
	case "mcp":
		return serveMCP(in, out, idx)
	case "stats":
		s := idx.Stats()
		if jsonOutput {
			return json.NewEncoder(out).Encode(s)
		}
		_, err = fmt.Fprintf(out, "%s\n%d files, %d symbols, %d edges\n", s.Root, s.Files, s.Symbols, s.Edges)
		return err
	}

	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if command == "query" {
		results, err := Search(idx, operand, SearchOptions{Mode: mode, Limit: limit})
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(results)
		}
		fmt.Fprintln(table, "SCORE\tSYMBOL\tKIND\tLOCATION")
		for _, hit := range results {
			fmt.Fprintf(table, "%.4f\t%s\t%s\t%s:%d\n", hit.Score, hit.Symbol.Qualified, hit.Symbol.Kind, hit.Symbol.Path, hit.Symbol.Line)
		}
		return table.Flush()
	}

	trace, err := traceGraph(idx, operand, depth, callees)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(trace)
	}
	fmt.Fprintln(table, "DEPTH\tSYMBOL\tVIA\tLOCATION")
	for _, step := range trace {
		fmt.Fprintf(table, "%d\t%s\t%s\t%s:%d\n", step.Depth, step.Symbol.Qualified, step.Via.Qualified, step.Symbol.Path, step.Symbol.Line)
	}
	return table.Flush()
}

// Move flags before operands for flag.FlagSet, preserving both their order and
// the meaning of the explicit end-of-options marker.
func parseFlags(flags *flag.FlagSet, args []string, wantOperand bool) (string, error) {
	var options, operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			operands = append(operands, arg)
			continue
		}
		options = append(options, arg)
		name, _, assigned := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if f := flags.Lookup(name); f != nil && !assigned {
			b, ok := f.Value.(interface{ IsBoolFlag() bool })
			if !ok || !b.IsBoolFlag() {
				if i+1 == len(args) {
					return "", fmt.Errorf("flag %s needs a value", arg)
				}
				i++
				options = append(options, args[i])
			}
		}
	}
	if err := flags.Parse(options); err != nil {
		return "", err
	}
	if wantOperand && len(operands) == 1 {
		return operands[0], nil
	}
	if !wantOperand && len(operands) == 0 {
		return "", nil
	}
	if wantOperand {
		return "", errors.New("expected one positional argument (quote multiword text)")
	}
	return "", errors.New("unexpected positional argument")
}
