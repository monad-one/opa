// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package repl

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/topdown"
	"github.com/open-policy-agent/opa/version"

	"github.com/peterh/liner"
)

// REPL represents an instance of the interactive shell.
type REPL struct {
	output      io.Writer
	dataStore   *storage.DataStore
	policyStore *storage.PolicyStore

	currentModuleID string
	buffer          []string
	initialized     bool
	nextID          int

	// TODO(tsandall): replace this state with rule definitions
	// inside the default module.
	outputFormat string
	trace        bool
	historyPath  string
	initPrompt   string
	bufferPrompt string
}

// New returns a new instance of the REPL.
func New(dataStore *storage.DataStore, policyStore *storage.PolicyStore, historyPath string, output io.Writer, outputFormat string) *REPL {
	return &REPL{
		output:          output,
		outputFormat:    outputFormat,
		trace:           false,
		dataStore:       dataStore,
		policyStore:     policyStore,
		currentModuleID: "repl",
		historyPath:     historyPath,
		initPrompt:      "> ",
		bufferPrompt:    "| ",
	}
}

// Loop will run until the user enters "exit", Ctrl+C, Ctrl+D, or an unexpected error occurs.
func (r *REPL) Loop() {

	// Initialize the liner library.
	line := liner.NewLiner()
	defer line.Close()
	line.SetCtrlCAborts(true)
	line.SetMultiLineMode(true)
	r.loadHistory(line)

	fmt.Fprintf(r.output, "OPA %v (commit %v, built at %v)\n", version.Version, version.Vcs, version.Timestamp)
	fmt.Fprintf(r.output, "\n")
	fmt.Fprintf(r.output, "Run 'help' to see a list of commands.\n")
	fmt.Fprintf(r.output, "\n")

	for true {

		input, err := line.Prompt(r.getPrompt())

		if err == liner.ErrPromptAborted || err == io.EOF {
			fmt.Fprintln(r.output, "Exiting")
			break
		}

		if err != nil {
			fmt.Fprintln(r.output, "error (fatal):", err)
			os.Exit(1)
		}

		if r.OneShot(input) {
			fmt.Fprintln(r.output, "Exiting")
			break
		}

		line.AppendHistory(input)
	}

	r.saveHistory(line)
}

// OneShot evaluates a single line and prints the result. Returns true if caller should exit.
func (r *REPL) OneShot(line string) bool {

	if r.init() {
		return true
	}

	if len(r.buffer) == 0 {
		if cmd := newCommand(line); cmd != nil {
			switch cmd.op {
			case "dump":
				return r.cmdDump()
			case "json":
				return r.cmdFormat("json")
			case "unset":
				return r.cmdUnset(cmd.args)
			case "pretty":
				return r.cmdFormat("pretty")
			case "trace":
				return r.cmdTrace()
			case "help":
				return r.cmdHelp()
			case "exit":
				return r.cmdExit()
			}
		}
		r.buffer = append(r.buffer, line)
		return r.evalBufferOne()
	}

	r.buffer = append(r.buffer, line)
	if len(line) == 0 {
		return r.evalBufferMulti()
	}

	return false
}

func (r *REPL) cmdDump() bool {
	fmt.Fprintln(r.output, r.dataStore)
	return false
}

func (r *REPL) cmdExit() bool {
	return true
}

func (r *REPL) cmdFormat(s string) bool {
	r.outputFormat = s
	return false
}

func (r *REPL) cmdHelp() bool {

	all := extra[:]
	all = append(all, builtin[:]...)

	maxLength := 0

	for _, c := range all {
		length := len(c.syntax())
		if length > maxLength {
			maxLength = length
		}
	}

	f := fmt.Sprintf("%%%dv : %%v\n", maxLength)

	for _, c := range all {
		fmt.Printf(f, c.syntax(), c.help)
	}

	return false
}

func (r *REPL) cmdTrace() bool {
	r.trace = !r.trace
	return false
}

func (r *REPL) cmdUnset(args []string) bool {

	if len(args) != 1 {
		fmt.Fprintln(r.output, "error: unset <var>: expects exactly one argument")
		return false
	}

	term, err := ast.ParseTerm(args[0])
	if err != nil {
		fmt.Fprintln(r.output, "error: argument must identify a rule")
		return false
	}

	v, ok := term.Value.(ast.Var)
	if !ok {
		fmt.Fprintln(r.output, "error: argument must identify a rule")
		return false
	}

	modules := r.policyStore.List()
	mod := modules[r.currentModuleID]
	rules := []*ast.Rule{}

	for _, r := range mod.Rules {
		if !r.Name.Equal(v) {
			rules = append(rules, r)
		}
	}

	if len(rules) == len(mod.Rules) {
		fmt.Fprintln(r.output, "warning: no matching rules in current module")
		return false
	}

	cpy := *mod
	cpy.Rules = rules
	modules[r.currentModuleID] = &cpy

	c := ast.NewCompiler()
	if c.Compile(modules); c.Failed() {
		fmt.Fprintln(r.output, "error:", c.FlattenErrors())
		return false
	}

	err = r.policyStore.Add(r.currentModuleID, c.Modules[r.currentModuleID], nil, false)
	if err != nil {
		fmt.Fprintln(r.output, "error:", err)
		return true
	}

	return false
}

func (r *REPL) compileBody(body ast.Body) (ast.Body, error) {

	name := fmt.Sprintf("repl%d", r.nextID)
	r.nextID++

	rule := &ast.Rule{
		Name: ast.Var(name),
		Body: body,
	}

	modules := r.policyStore.List()
	mod := modules[r.currentModuleID]
	prev := mod.Rules
	mod.Rules = append(mod.Rules, rule)

	c := ast.NewCompiler()
	if c.Compile(modules); c.Failed() {
		mod.Rules = prev
		return nil, fmt.Errorf(c.FlattenErrors())
	}

	return mod.Rules[len(prev)].Body, nil
}

func (r *REPL) compileRule(rule *ast.Rule) (*ast.Module, error) {

	modules := r.policyStore.List()
	mod := modules[r.currentModuleID]
	prev := mod.Rules
	mod.Rules = append(mod.Rules, rule)

	c := ast.NewCompiler()
	if c.Compile(modules); c.Failed() {
		mod.Rules = prev
		return nil, fmt.Errorf(c.FlattenErrors())
	}

	return mod, nil
}

func (r *REPL) evalBufferOne() bool {

	line := strings.Join(r.buffer, "\n")

	if len(strings.TrimSpace(line)) == 0 {
		r.buffer = []string{}
		return false
	}

	// The user may enter lines with comments on the end or
	// multiple lines with comments interspersed. In these cases
	// the parser will return multiple statements.
	stmts, err := ast.ParseStatements(line)

	if err != nil {
		return false
	}

	r.buffer = []string{}

	for _, stmt := range stmts {
		r.evalStatement(stmt)
	}

	return false
}

func (r *REPL) evalBufferMulti() bool {

	line := strings.Join(r.buffer, "\n")
	r.buffer = []string{}

	if len(strings.TrimSpace(line)) == 0 {
		return false
	}

	stmts, err := ast.ParseStatements(line)

	if err != nil {
		fmt.Fprintln(r.output, "error:", err)
		return false
	}

	for _, stmt := range stmts {
		r.evalStatement(stmt)
	}

	return false
}

func (r *REPL) evalStatement(stmt interface{}) bool {
	switch s := stmt.(type) {
	case ast.Body:
		s, err := r.compileBody(s)
		if err != nil {
			fmt.Fprintln(r.output, "error:", err)
			return false
		}
		if s := ast.ParseConstantRule(s); s != nil {
			mod, err := r.compileRule(s)
			if err != nil {
				fmt.Fprintln(r.output, "error:", err)
				return false
			}
			return r.evalModule(mod, s)
		}
		return r.evalBody(s)
	case *ast.Rule:
		mod, err := r.compileRule(s)
		if err != nil {
			fmt.Fprintln(r.output, "error:", err)
			return false
		}
		return r.evalModule(mod, s)
	case *ast.Import:
		return r.evalImport(s)
	case *ast.Package:
		return r.evalPackage(s)
	}
	return false
}

func (r *REPL) evalBody(body ast.Body) bool {

	// Special case for positive, single term inputs.
	if len(body) == 1 {
		expr := body[0]
		if !expr.Negated {
			if _, ok := expr.Terms.(*ast.Term); ok {
				if singleValue(body) {
					return r.evalTermSingleValue(body)
				}
				return r.evalTermMultiValue(body)
			}
		}
	}

	ctx := topdown.NewContext(body, r.dataStore)
	if r.trace {
		ctx.Tracer = &topdown.StdoutTracer{}
	}

	// Flag indicates whether the query was defined for some context.
	// If the query does not include any ground terms, the results will
	// be empty, but we still want to output "true". If there are
	// no results, this will remain "false" and we will output "false".
	var isTrue = false

	// Store bindings as slice of maps where map keys are variables
	// and values are the underlying Go values.
	var results []map[string]interface{}

	// Execute query and accumulate results.
	err := topdown.Eval(ctx, func(ctx *topdown.Context) error {
		var err error
		row := map[string]interface{}{}
		ctx.Locals.Iter(func(k, v ast.Value) bool {
			name, ok := k.(ast.Var)
			if !ok {
				return false
			}
			if strings.HasPrefix(string(name), ast.WildcardPrefix) {
				return false
			}
			r, e := topdown.ValueToInterface(v, ctx)
			if e != nil {
				err = e
				return true
			}
			row[k.String()] = r
			return false
		})

		if err != nil {
			return err
		}

		isTrue = true

		if len(row) > 0 {
			results = append(results, row)
		}

		return nil
	})

	if err != nil {
		fmt.Fprintf(r.output, "error: %v\n", err)
		return false
	}

	if isTrue {
		if len(results) >= 1 {
			r.printResults(getHeaderForBody(body), results)
		} else {
			fmt.Fprintln(r.output, "true")
		}
	} else {
		fmt.Fprintln(r.output, "false")
	}

	return false
}

func (r *REPL) evalModule(mod *ast.Module, stmt *ast.Rule) bool {

	err := r.policyStore.Add(r.currentModuleID, mod, nil, false)
	if err != nil {
		fmt.Fprintln(r.output, "error:", err)
		return true
	}

	return false
}

func (r *REPL) evalImport(i *ast.Import) bool {

	modules := r.policyStore.List()
	mod := modules[r.currentModuleID]

	for _, x := range mod.Imports {
		if x.Equal(i) {
			return false
		}
	}

	prev := mod.Imports
	mod.Imports = append(mod.Imports, i)

	c := ast.NewCompiler()
	if c.Compile(modules); c.Failed() {
		mod.Imports = prev
		fmt.Fprintln(r.output, "error:", c.FlattenErrors())
		return false
	}

	err := r.policyStore.Add(r.currentModuleID, c.Modules[r.currentModuleID], nil, false)
	if err != nil {
		fmt.Fprint(r.output, "error:", err)
		return true
	}

	return false
}

func (r *REPL) evalPackage(p *ast.Package) bool {

	modules := r.policyStore.List()
	moduleID := p.Path[1:].String()
	if _, ok := modules[moduleID]; ok {
		r.currentModuleID = moduleID
		return false
	}

	err := r.policyStore.Add(moduleID, &ast.Module{Package: p}, nil, false)
	if err != nil {
		fmt.Fprint(r.output, "error:", err)
		return true
	}

	r.currentModuleID = moduleID

	return false
}

// evalTermSingleValue evaluates and prints terms in cases where the term evaluates to a
// single value, e.g., "1", true, [1,2,"foo"], [x | x = a[i], a = [1,2,3]], etc. Ground terms
// and comprehensions always evaluate to a single value. To handle references, this function
// still executes the query, except it does so by rewriting the body to assign the term
// to a variable. This allows the REPL to obtain the result even if the term is false.
func (r *REPL) evalTermSingleValue(body ast.Body) bool {

	term := body[0].Terms.(*ast.Term)
	outputVar := ast.VarTerm("$")
	body = ast.Body{ast.Equality.Expr(term, outputVar)}

	ctx := topdown.NewContext(body, r.dataStore)
	if r.trace {
		ctx.Tracer = &topdown.StdoutTracer{}
	}

	var result interface{}
	isTrue := false

	err := topdown.Eval(ctx, func(ctx *topdown.Context) error {
		p := ctx.Locals.Get(outputVar.Value)
		v, err := topdown.ValueToInterface(p, ctx)
		if err != nil {
			return err
		}
		result = v
		isTrue = true
		return nil
	})

	if err != nil {
		fmt.Fprintln(r.output, "error:", err)
	} else if isTrue {
		r.printJSON(result)
	} else {
		r.printUndefined()
	}

	return false
}

// evalTermMultiValue evaluates and prints terms in cases where the term may evaluate to multiple
// ground values, e.g., a[i], [servers[x]], etc.
func (r *REPL) evalTermMultiValue(body ast.Body) bool {

	ctx := topdown.NewContext(body, r.dataStore)
	if r.trace {
		ctx.Tracer = &topdown.StdoutTracer{}
	}

	term := body[0].Terms.(*ast.Term)

	vars := map[string]struct{}{}
	results := []map[string]interface{}{}
	resultKey := string(term.Location.Text)

	// Do not include the value of the input term if the input term was a set reference. E.g.,
	// for "p[x]", the value users are interested in is "x" not p[x] which is always defined
	// as true.
	includeValue := !r.isSetReference(term)

	err := topdown.Eval(ctx, func(ctx *topdown.Context) error {

		result := map[string]interface{}{}

		var err error

		ctx.Locals.Iter(func(k, v ast.Value) bool {
			if k, ok := k.(ast.Var); ok {
				name := string(k)
				if strings.HasPrefix(name, ast.WildcardPrefix) {
					return false
				}
				x, e := topdown.ValueToInterface(v, ctx)
				if e != nil {
					err = e
					return true
				}
				result[name] = x
				vars[name] = struct{}{}
			}
			return false
		})

		if err != nil {
			return err
		}

		if includeValue {
			p := topdown.PlugTerm(term, ctx)
			v, err := topdown.ValueToInterface(p.Value, ctx)
			if err != nil {
				return err
			}
			result[resultKey] = v
		}

		results = append(results, result)

		return nil
	})

	if err != nil {
		fmt.Fprintln(r.output, "error:", err)
	} else if len(results) > 0 {
		keys := []string{}
		for v := range vars {
			keys = append(keys, v)
		}
		sort.Strings(keys)
		if includeValue {
			keys = append(keys, resultKey)
		}
		r.printResults(keys, results)
	} else {
		r.printUndefined()
	}

	return false
}

func (r *REPL) getPrompt() string {
	if len(r.buffer) > 0 {
		return r.bufferPrompt
	}
	return r.initPrompt
}

func (r *REPL) init() bool {

	if r.initialized {
		return false
	}

	mod := ast.MustParseModule(fmt.Sprintf(`
	package %s
	`, r.currentModuleID))

	modules := r.policyStore.List()
	modules[r.currentModuleID] = mod

	c := ast.NewCompiler()
	if c.Compile(modules); c.Failed() {
		fmt.Fprintln(r.output, "error:", c.FlattenErrors())
		return true
	}

	if err := r.policyStore.Add(r.currentModuleID, c.Modules[r.currentModuleID], nil, false); err != nil {
		fmt.Fprintln(r.output, "error:", err)
		return true
	}

	r.initialized = true

	return false
}

// isSetReference returns true if term is a reference that refers to a set document.
func (r *REPL) isSetReference(term *ast.Term) bool {
	ref, ok := term.Value.(ast.Ref)
	if !ok {
		return false
	}
	p := ast.Ref{}
	for _, x := range ref {
		p = append(p, x)
		if node, err := r.dataStore.GetRef(p); err == nil {
			if rs, ok := node.([]*ast.Rule); ok {
				if rs[0].DocKind() == ast.PartialSetDoc {
					return true
				}
			}
		}
	}
	return false
}

func (r *REPL) loadHistory(prompt *liner.State) {
	if f, err := os.Open(r.historyPath); err == nil {
		prompt.ReadHistory(f)
		f.Close()
	}
}

func (r *REPL) printResults(keys []string, results []map[string]interface{}) {
	switch r.outputFormat {
	case "json":
		r.printJSON(results)
	default:
		r.printPretty(keys, results)
	}
}

func (r *REPL) printJSON(x interface{}) {
	buf, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		fmt.Fprintln(r.output, err)
		return
	}
	fmt.Fprintln(r.output, string(buf))
}

func (r *REPL) printPretty(keys []string, results []map[string]interface{}) {
	table := tablewriter.NewWriter(r.output)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetAutoFormatHeaders(false)
	table.SetHeader(keys)
	for _, row := range results {
		r.printPrettyRow(table, keys, row)
	}
	table.Render()
}

func (r *REPL) printPrettyRow(table *tablewriter.Table, keys []string, row map[string]interface{}) {

	buf := []string{}
	for _, k := range keys {
		js, err := json.Marshal(row[k])
		if err != nil {
			buf = append(buf, err.Error())
		} else {
			buf = append(buf, string(js))
		}
	}

	// Add fields to table in sorted order.
	table.Append(buf)
}

func (r *REPL) printUndefined() {
	fmt.Fprintln(r.output, "undefined")
}

func (r *REPL) saveHistory(prompt *liner.State) {
	if f, err := os.Create(r.historyPath); err == nil {
		prompt.WriteHistory(f)
		f.Close()
	}
}

type commandDesc struct {
	name string
	args []string
	help string
}

func (c commandDesc) syntax() string {
	if len(c.args) > 0 {
		return fmt.Sprintf("%v %v", c.name, strings.Join(c.args, " "))
	}
	return c.name
}

var extra = [...]commandDesc{
	{"<stmt>", []string{}, "evaluate the statement"},
	{"package", []string{"<term>"}, "change currently active package"},
	{"import", []string{"<term>"}, "add import to currently active module"},
}

var builtin = [...]commandDesc{
	{"unset", []string{"<var>"}, "undefine rules in currently active module"},
	{"json", []string{}, "set output format to JSON"},
	{"pretty", []string{}, "set output format to pretty"},
	{"dump", []string{}, "dump the raw storage content"},
	{"trace", []string{}, "toggle stdout tracing"},
	{"help", []string{}, "print this message"},
	{"exit", []string{}, "exit back to shell (or ctrl+c, ctrl+d)"},
	{"ctrl+l", []string{}, "clear the screen"},
}

type command struct {
	op   string
	args []string
}

func newCommand(line string) *command {
	p := strings.Fields(strings.TrimSpace(strings.ToLower(line)))
	if len(p) == 0 {
		return nil
	}
	for _, c := range builtin {
		if c.name == p[0] {
			return &command{
				op:   c.name,
				args: p[1:],
			}
		}
	}
	return nil
}

func buildHeader(fields map[string]struct{}, term *ast.Term) {
	switch v := term.Value.(type) {
	case ast.Ref:
		for _, t := range v[1:] {
			buildHeader(fields, t)
		}
	case ast.Var:
		s := string(v)
		if !strings.HasPrefix(s, ast.WildcardPrefix) {
			fields[s] = struct{}{}
		}
	case ast.Object:
		for _, i := range v {
			buildHeader(fields, i[0])
			buildHeader(fields, i[1])
		}
	case ast.Array:
		for _, e := range v {
			buildHeader(fields, e)
		}
	}
}

func getHeaderForBody(body ast.Body) []string {
	// Build set of fields for the output. The fields are the variables from inside the body.
	// If the variable appears multiple times, we only want a single field so store them in a
	// map/set.
	fields := map[string]struct{}{}

	// TODO(tsandall): perhaps we could refactor this to use a "walk" function on the body.
	for _, expr := range body {
		switch ts := expr.Terms.(type) {
		case []*ast.Term:
			for _, t := range ts[1:] {
				buildHeader(fields, t)
			}
		case *ast.Term:
			buildHeader(fields, ts)
		}
	}

	// Sort/display fields by name.
	keys := []string{}
	for k := range fields {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	return keys
}

// singleValue returns true if body can be evaluated to a single term.
func singleValue(body ast.Body) bool {
	if len(body) != 1 {
		return false
	}
	term, ok := body[0].Terms.(*ast.Term)
	if !ok {
		return false
	}
	switch term.Value.(type) {
	case *ast.ArrayComprehension:
		return true
	default:
		return term.IsGround()
	}
}
