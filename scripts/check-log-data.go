//go:build ignore

// check-log-data.go — OGA-1036, copied into the kit SDK by OGA-1134
//
// A tripwire for log statements that would write user or customer data to the
// logs of a kit sidecar (and of the platform's Knowledge Agent, which links the
// agent pipeline). It parses every non-test Go file in the SDK's package
// directories (every top-level directory except scripts/, so a new package is
// scanned without editing this file), finds log calls (Debug/Info/Warn/Error
// and their *Context forms, Log, LogAttrs, on any receiver: slog, a
// *slog.Logger, kitlog's logger), and flags an attribute whose KEY names a kind
// of data that must not be logged without the operator's approval:
// credentials, personal data (email, phone, chat id, names) and customer
// content (prompts, message text, queries, bodies, payloads, tool arguments and
// results).
//
// It is a denylist over attribute keys. It cannot see a value that carries data
// under an innocent key ("error", err with a response body in it); that is on
// the author, and internal/redact is the SDK's tool for it.
//
// Keys are found in three shapes: passed to the call directly
// (logger.Info(msg, "text", s)), appended to a slice spread into it
// (logger.Info(msg, append(base, "text", s)...)), and held in a slice built
// before the call, in the same file (attrs := []any{"text", s}; then
// logger.Info(msg, attrs...), including attrs = append(attrs, ...)). A slice
// that arrives as a parameter is not followed.
//
// A flagged line passes only with an approval annotation on the call or the
// line above it, or on the attribute's own line or the line above it:
//
//	// log-data:approved <approver> <date or ticket>: <why this data must be logged>
//
// The annotation is the operator's approval made visible in review
// (.kiro/steering/shared/tenant-data-privacy.md). An attribute whose value is
// len(...), a comparison, or a logsafe.* call is a size, a flag or a redaction,
// not the data, and passes without one.
//
// This file is a copy. The original is the platform's scripts/check-log-data.go
// (oga-platform). The copy lives here because this repository is public and
// the platform's shared tools are not. A change to the denylist, the annotation
// rule or the detector is made in both, in the same change; only scanDirs and
// this header differ.
//
// Usage:
//
//	go run scripts/check-log-data.go              # scan the repo
//	go run scripts/check-log-data.go -self-test   # prove the detector fires
//	go run scripts/check-log-data.go -root <dir>  # scan another tree
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// logMethods are the method names treated as log calls.
var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true,
}

// attrConstructors are slog attribute constructors whose first argument is a key.
var attrConstructors = map[string]bool{
	"String": true, "Any": true, "Int": true, "Int64": true, "Uint64": true,
	"Float64": true, "Bool": true, "Time": true, "Duration": true, "Group": true,
}

// deniedKeys maps a lowercased attribute key to the category it belongs to.
var deniedKeys = func() map[string]string {
	m := map[string]string{}
	add := func(category string, keys ...string) {
		for _, k := range keys {
			m[k] = category
		}
	}
	add("credential",
		"token", "access_token", "id_token", "refresh_token", "bearer", "jwt",
		"password", "passwd", "secret", "client_secret", "api_key", "apikey",
		"authorization", "cookie", "seed", "creds", "credentials", "bot_token",
		"signature", "presigned_url", "upstream_token", "upload_token")
	add("personal data",
		"email", "emails", "email_address", "admin_email", "seeded_admin_email",
		"invitee", "recipient", "recipients", "target_users",
		"phone", "phone_number", "msisdn", "whatsapp_phone",
		"chat_id", "telegram_chat_id", "username", "user_name", "admin_username",
		"display_name", "first_name", "last_name", "full_name",
		"ip", "ip_address", "client_ip", "remote_addr", "remote_ip")
	add("customer content",
		"prompt", "system_prompt", "user_prompt", "composed_prompt",
		"completion", "llm_output", "raw_response", "raw_content",
		"body", "request_body", "response_body", "raw_body", "body_preview",
		"payload", "raw_payload", "content", "message_text", "text",
		"query", "original_query", "simplified_query", "question", "answer",
		"original", "rewritten", "reasoning", "rationale", "thought", "evidence",
		"preview", "response_preview", "data_preview", "result_preview",
		"args", "arguments", "tool_args", "tool_result",
		"properties", "props", "record", "row", "location", "comment", "note",
		"filename", "file_name")
	return m
}()

var approval = regexp.MustCompile(`log-data:approved\s+\S+[^:\n]*:\s*\S`)

type finding struct {
	pos      token.Position
	key      string
	category string
}

func main() {
	root := flag.String("root", ".", "repository root to scan")
	selfTest := flag.Bool("self-test", false, "run the detector against built-in samples")
	flag.Parse()

	if *selfTest {
		os.Exit(runSelfTest())
	}
	dirs, err := scanDirs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-log-data:", err)
		os.Exit(2)
	}
	findings, files, err := scanTree(*root, dirs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-log-data:", err)
		os.Exit(2)
	}
	if len(findings) == 0 {
		fmt.Printf("check-log-data: no unapproved user or customer data in log statements (%d files).\n", files)
		return
	}
	for _, f := range findings {
		fmt.Printf("  %s:%d: log attribute %q is %s\n", f.pos.Filename, f.pos.Line, f.key, f.category)
	}
	fmt.Printf("\ncheck-log-data: %d log attribute(s) would write user or customer data to the logs.\n", len(findings))
	fmt.Println("Log an id, a count, a length or a logsafe.* form instead. If the data itself is")
	fmt.Println("needed, get the operator's approval and annotate the call:")
	fmt.Println("  // log-data:approved <approver> <date or ticket>: <reason>")
	fmt.Println("See .kiro/steering/shared/tenant-data-privacy.md (OGA-1036).")
	os.Exit(1)
}

// scanDirs names the directories under root that hold the code to scan. It is
// the one function that differs between this copy and the platform's: here it
// is every top-level directory except scripts/ and hidden ones, so a package
// added to the SDK is scanned without editing this file.
func scanDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		switch name {
		case "scripts", "testdata", "vendor", "node_modules":
			continue
		}
		dirs = append(dirs, name)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no package directories under %s", root)
	}
	return dirs, nil
}

func scanTree(root string, dirs []string) ([]finding, int, error) {
	var findings []finding
	files := 0
	for _, d := range dirs {
		base := filepath.Join(root, d)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				switch de.Name() {
				case "testdata", "vendor", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			name := de.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".pb.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			fs, err := scanFile(rel, src)
			if err != nil {
				return err
			}
			files++
			findings = append(findings, fs...)
			return nil
		})
		if err != nil {
			return nil, files, err
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].pos.Filename != findings[j].pos.Filename {
			return findings[i].pos.Filename < findings[j].pos.Filename
		}
		return findings[i].pos.Line < findings[j].pos.Line
	})
	return findings, files, nil
}

func scanFile(name string, src []byte) ([]finding, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	approvedLines := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if approval.MatchString(c.Text) {
				approvedLines[fset.Position(c.Slash).Line] = true
			}
		}
	}
	s := &scanner{defs: sliceDefs(f)}
	var out []finding
	seen := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !logMethods[sel.Sel.Name] || len(call.Args) < 2 {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && notLoggers[id.Name] {
			return true
		}
		start := fset.Position(call.Pos()).Line
		end := fset.Position(call.End()).Line
		approved := approvedLines[start-1]
		for l := start; l <= end && !approved; l++ {
			approved = approvedLines[l]
		}
		for _, a := range s.attrs(sel.Sel.Name, call.Args, call.Ellipsis.IsValid()) {
			category, denied := deniedKeys[strings.ToLower(a.key)]
			line := fset.Position(a.pos).Line
			if !denied || safeValue(a.val) || approved || approvedLines[line] || approvedLines[line-1] {
				continue
			}
			// A slice built once and spread into several calls is one finding.
			if seen[a.pos] {
				continue
			}
			seen[a.pos] = true
			out = append(out, finding{pos: fset.Position(a.pos), key: a.key, category: category})
		}
		return true
	})
	return out, nil
}

type attr struct {
	key string
	val ast.Expr
	pos token.Pos
}

// scanner follows the slices a file builds for its log calls.
type scanner struct {
	// defs holds, for each variable, every slice value assigned to it in the
	// file: a composite literal ([]any{"k", v}) or an append(...) call.
	defs map[*ast.Object][]ast.Expr
}

// sliceDefs collects the slice values assigned to each variable in f
// (x := []any{...}, var x = []any{...}, x = append(x, ...)). Identifiers are
// matched by the parser's object resolution, so a name reused in another
// function is a different variable.
func sliceDefs(f *ast.File) map[*ast.Object][]ast.Expr {
	defs := map[*ast.Object][]ast.Expr{}
	record := func(id *ast.Ident, v ast.Expr) {
		if id == nil || id.Obj == nil || !isSliceValue(v) {
			return
		}
		defs[id.Obj] = append(defs[id.Obj], v)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == len(s.Rhs) {
				for i, l := range s.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						record(id, s.Rhs[i])
					}
				}
			}
		case *ast.ValueSpec:
			if len(s.Names) == len(s.Values) {
				for i, id := range s.Names {
					record(id, s.Values[i])
				}
			}
		}
		return true
	})
	return defs
}

// isSliceValue reports whether v is a slice the scanner can read: a composite
// literal or an append(...) call.
func isSliceValue(v ast.Expr) bool {
	switch e := unparen(v).(type) {
	case *ast.CompositeLit:
		return true
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		return ok && id.Name == "append"
	}
	return false
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// notLoggers are selector receivers whose Error/Info/... methods are not log
// calls (http.Error(w, msg, code), status.Error(code, msg)).
var notLoggers = map[string]bool{"http": true, "status": true, "errors": true, "fmt": true}

// messageIndex is where slog puts the message for a method: Debug/Info/Warn/
// Error take (msg, args...), the *Context forms (ctx, msg, args...), and
// Log/LogAttrs (ctx, level, msg, args...). The Temporal workflow logger shares
// the first shape.
func messageIndex(method string) int {
	switch {
	case method == "Log" || method == "LogAttrs":
		return 2
	case strings.HasSuffix(method, "Context"):
		return 1
	default:
		return 0
	}
}

// attrs returns the attributes of a log call. The message position comes from
// the method name, so a message held in a variable does not hide the first key.
// When the call spreads its last argument (spread: args...), that argument is a
// slice, and its elements are read through expand.
func (s *scanner) attrs(method string, args []ast.Expr, spread bool) []attr {
	i := messageIndex(method) + 1
	if i >= len(args) {
		return nil
	}
	rest := args[i:]
	if !spread {
		return pairs(rest)
	}
	out := pairs(rest[:len(rest)-1])
	return append(out, s.expand(rest[len(rest)-1], map[*ast.Object]bool{})...)
}

// expand returns the attributes held in a slice expression: a composite
// literal, an append(...) call (its base slice and the elements it adds), or a
// variable whose slice values were collected by sliceDefs. Anything else (a
// parameter, a function's result) is not followed. visited stops a variable
// that appends to itself (x = append(x, ...)) from recursing forever.
func (s *scanner) expand(e ast.Expr, visited map[*ast.Object]bool) []attr {
	switch v := unparen(e).(type) {
	case *ast.CompositeLit:
		return pairs(v.Elts)
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		if !ok || id.Name != "append" || len(v.Args) == 0 {
			return nil
		}
		out := s.expand(v.Args[0], visited)
		extra := v.Args[1:]
		if v.Ellipsis.IsValid() && len(extra) > 0 {
			out = append(out, pairs(extra[:len(extra)-1])...)
			return append(out, s.expand(extra[len(extra)-1], visited)...)
		}
		return append(out, pairs(extra)...)
	case *ast.Ident:
		if v.Obj == nil || visited[v.Obj] {
			return nil
		}
		visited[v.Obj] = true
		var out []attr
		for _, d := range s.defs[v.Obj] {
			out = append(out, s.expand(d, visited)...)
		}
		return out
	}
	return nil
}

// pairs walks a list of log arguments the way slog does: a string literal is a
// key whose value is the next element, and anything else occupies one slot (an
// Attr, or a value the scanner cannot read).
func pairs(args []ast.Expr) []attr {
	var out []attr
	i := 0
	for i < len(args) {
		switch a := args[i].(type) {
		case *ast.BasicLit:
			if a.Kind == token.STRING && i+1 < len(args) {
				out = append(out, attr{key: strings.Trim(a.Value, "`\""), val: args[i+1], pos: a.Pos()})
				i += 2
				continue
			}
		case *ast.CallExpr:
			if s, ok := a.Fun.(*ast.SelectorExpr); ok && attrConstructors[s.Sel.Name] && len(a.Args) >= 2 {
				if id, ok := s.X.(*ast.Ident); ok && id.Name == "slog" {
					if lit, ok := a.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out = append(out, attr{key: strings.Trim(lit.Value, "`\""), val: a.Args[1], pos: lit.Pos()})
					}
				}
			}
		}
		i++
	}
	return out
}

// safeValue reports whether the value is a size, a flag or a redaction rather
// than the data itself.
func safeValue(v ast.Expr) bool {
	switch e := v.(type) {
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "len" {
			return true
		}
		if s, ok := e.Fun.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok && id.Name == "logsafe" {
				return true
			}
		}
	case *ast.BinaryExpr:
		switch e.Op {
		case token.EQL, token.NEQ, token.LSS, token.GTR, token.LEQ, token.GEQ, token.LAND, token.LOR:
			return true
		}
	case *ast.UnaryExpr:
		return e.Op == token.NOT
	}
	return false
}

func runSelfTest() int {
	// Each denied key in bad.go is distinct, so the test can name the shape
	// that was missed.
	bad := `package p
import "log/slog"
func f(email, prompt string, n int, logger *slog.Logger) {
	slog.Info("invite sent", "tenant_id", "t", "email", email)
	slog.WarnContext(nil, "composed", "prompt", prompt)
	slog.Info("attr form", slog.String("chat_id", email))
	msg := "variable message"
	slog.Info(msg, "first_name", email)
	slog.InfoContext(nil, msg, "system_prompt", prompt)
	slog.Default().Log(nil, slog.LevelInfo, msg, "phone", email)

	// Appended to a slice spread into the call (OGA-1134).
	base := []any{"tenant_id", "t", "seq", n}
	logger.InfoContext(nil, "appended", append(base, "text", prompt)...)
	logger.InfoContext(nil, "appended across lines", append(base,
		"tool", "kg_get_entity",
		"args", prompt)...)
	logger.Info("appended attr", append(base, slog.String("thought", prompt))...)
	logger.Info("appended spread", append(base, []any{"answer", prompt}...)...)

	// Built before the call and spread into it (OGA-1134).
	attrs := []any{"query", prompt}
	logger.Info("prebuilt", attrs...)
	var held = []any{"tenant_id", "t", "comment", prompt}
	logger.Warn("prebuilt var", held...)
	grown := []any{"tenant_id", "t"}
	grown = append(grown, "username", email)
	logger.Info("grown", grown...)
	attrList := []slog.Attr{slog.String("payload", prompt)}
	logger.LogAttrs(nil, slog.LevelInfo, "prebuilt attrs", attrList...)
	logger.Info("inline slice", []any{"preview", prompt}...)
}
`
	wantBad := []string{
		"email", "prompt", "chat_id", "first_name", "system_prompt", "phone",
		"text", "args", "thought", "answer",
		"query", "comment", "username", "payload", "preview",
	}
	good := `package p
import (
	"log/slog"
	"net/http"
)
func f(email, prompt string, n int, logger *slog.Logger, fromCaller []any) {
	slog.Info("email sent", "tenant_id", "t", "count", n)
	slog.Info("lengths", "prompt", len(prompt), "has_email", email != "")
	// log-data:approved philip 2026-10-08 OGA-1036: synthetic test of the annotation
	slog.Info("approved", "email", email)
	slog.Info("msg only")
	slog.Warn("fallback", "intent", "prompt", "locale", "en-US")
	http.Error(nil, "body", 400)

	base := []any{"tenant_id", "t", "seq", n}
	logger.Info("allowed keys", append(base, "tool", "kg_get_entity", "steps", n)...)
	logger.Info("a size", append(base, "text_length", len(prompt), "text", len(prompt))...)
	// log-data:approved philip 2026-10-20 OGA-1134: synthetic test of the annotation on an appended key
	logger.Info("approved append", append(base, "text", prompt)...)
	logger.Info("approved append, multi-line", append(base,
		// log-data:approved philip 2026-10-20 OGA-1134: synthetic test of an annotation inside the call
		"args", prompt)...)
	attrs := []any{
		// log-data:approved philip 2026-10-20 OGA-1134: synthetic test of an annotation in a prebuilt slice
		"query", prompt,
	}
	logger.Info("approved prebuilt", attrs...)
	logger.Info("base only", base...)
	logger.Info("a parameter is not followed", fromCaller...)
}
func h(prompt string) []any {
	notLogged := []any{"query", prompt}
	return notLogged
}
func g(logger *slog.Logger) {
	notLogged := []any{"tenant_id", "t"}
	logger.Info("same name, other function", notLogged...)
}
`
	fb, err := scanFile("bad.go", []byte(bad))
	if err != nil {
		fmt.Println("self-test: parse:", err)
		return 1
	}
	fg, err := scanFile("good.go", []byte(good))
	if err != nil {
		fmt.Println("self-test: parse:", err)
		return 1
	}
	rc := 0
	got := map[string]bool{}
	for _, f := range fb {
		got[f.key] = true
	}
	for _, k := range wantBad {
		if !got[k] {
			fmt.Printf("self-test FAILED: bad.go: key %q was not flagged\n", k)
			rc = 1
		}
	}
	if len(fb) != len(wantBad) {
		fmt.Printf("self-test FAILED: want %d findings in bad.go, got %d: %v\n", len(wantBad), len(fb), fb)
		rc = 1
	}
	if len(fg) != 0 {
		fmt.Printf("self-test FAILED: want 0 findings in good.go, got %d: %v\n", len(fg), fg)
		rc = 1
	}
	if rc == 0 {
		fmt.Println("check-log-data self-test passed")
	}
	return rc
}
