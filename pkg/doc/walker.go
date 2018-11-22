// Copyright 2011 Gary Burd
// Copyright 2013 Unknown
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package doc

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Unknwon/com"
)

// WalkDepth indicates how far the process goes.
type WalkDepth uint

const (
	WD_Imports WalkDepth = iota
	WD_All
)

// WalkType indicates which way to get data for walker.
type WalkType uint

const (
	WT_Local WalkType = iota
	WT_Memory
	WT_Zip
	WT_TarGz
	WT_Http
)

// WalkMode indicates which things to do.
type WalkMode uint

const (
	WM_All WalkMode = 1 << iota
	WM_NoReadme
	WM_NoExample
)

type WalkRes struct {
	WalkDepth
	WalkType
	WalkMode
	RootPath string    // For WT_Local mode.
	Srcs     []*Source // For WT_Memory mode.
	BuildAll bool
}

// ------------------------------
// WT_Local
// ------------------------------

func (w *Walker) setLocalContext(ctxt *build.Context) {
	ctxt.IsAbsPath = path.IsAbs
}

// ------------------------------
// WT_Memory
// ------------------------------

func (w *Walker) readDir(dir string) ([]os.FileInfo, error) {
	if dir != w.Pdoc.ImportPath {
		panic("unexpected")
	}
	fis := make([]os.FileInfo, 0, len(w.SrcFiles))
	for _, src := range w.SrcFiles {
		fis = append(fis, src)
	}
	return fis, nil
}

func (w *Walker) openFile(path string) (io.ReadCloser, error) {
	if strings.HasPrefix(path, w.Pdoc.ImportPath+"/") {
		if src, ok := w.SrcFiles[path[len(w.Pdoc.ImportPath)+1:]]; ok {
			return ioutil.NopCloser(bytes.NewReader(src.Data())), nil
		}
	}
	return nil, os.ErrNotExist
}

func (w *Walker) setMemoryContext(ctxt *build.Context) {
	ctxt.JoinPath = path.Join
	ctxt.IsAbsPath = path.IsAbs
	ctxt.IsDir = func(path string) bool { return true }
	ctxt.HasSubdir = func(root, dir string) (rel string, ok bool) { panic("unexpected") }
	ctxt.ReadDir = func(dir string) (fi []os.FileInfo, err error) { return w.readDir(dir) }
	ctxt.OpenFile = func(path string) (r io.ReadCloser, err error) { return w.openFile(path) }
}

var badSynopsisPrefixes = []string{
	"Autogenerated by Thrift Compiler",
	"Automatically generated ",
	"Auto-generated by ",
	"Copyright ",
	"COPYRIGHT ",
	`THE SOFTWARE IS PROVIDED "AS IS"`,
	"TODO: ",
	"vim:",
}

// Synopsis extracts the first sentence from s. All runs of whitespace are
// replaced by a single space.
func synopsis(s string) string {
	parts := strings.SplitN(s, "\n\n", 2)
	s = parts[0]

	var buf []byte
	const (
		other = iota
		period
		space
	)
	last := space
Loop:
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch b {
		case ' ', '\t', '\r', '\n':
			switch last {
			case period:
				break Loop
			case other:
				buf = append(buf, ' ')
				last = space
			}
		case '.':
			last = period
			buf = append(buf, b)
		default:
			last = other
			buf = append(buf, b)
		}
	}

	// Ensure that synopsis fits an App Engine datastore text property.
	const m = 297
	if len(buf) > m {
		buf = buf[:m]
		if i := bytes.LastIndex(buf, []byte{' '}); i >= 0 {
			buf = buf[:i]
		}
		buf = append(buf, " ..."...)
	}

	s = string(buf)

	r, n := utf8.DecodeRuneInString(s)
	if n < 0 || unicode.IsPunct(r) || unicode.IsSymbol(r) {
		// ignore Markdown headings, editor settings, Go build constraints, and * in poorly formatted block comments.
		s = ""
	} else {
		for _, prefix := range badSynopsisPrefixes {
			if strings.HasPrefix(s, prefix) {
				s = ""
				break
			}
		}
	}

	return strings.TrimRight(s, " \t\n\r")
}

// poorMansImporter returns a (dummy) package object named
// by the last path component of the provided package path
// (as is the convention for packages). This is sufficient
// to resolve package identifiers without doing an actual
// import. It never returns an error.
//
func poorMansImporter(imports map[string]*ast.Object, path string) (*ast.Object, error) {
	pkg := imports[path]
	if pkg == nil {
		// Guess the package name without importing it. Start with the last
		// element of the path.
		name := path[strings.LastIndex(path, "/")+1:]

		// Trim commonly used prefixes and suffixes containing illegal name
		// runes.
		name = strings.TrimSuffix(name, ".go")
		name = strings.TrimSuffix(name, "-go")
		name = strings.TrimPrefix(name, "go.")
		name = strings.TrimPrefix(name, "go-")
		name = strings.TrimPrefix(name, "biogo.")

		pkg = ast.NewObj(ast.Pkg, name)
		pkg.Data = ast.NewScope(nil) // required by ast.NewPackage for dot-import
		imports[path] = pkg
	}
	return pkg, nil
}

type sliceWriter struct{ p *[]byte }

func (w sliceWriter) Write(p []byte) (int, error) {
	*w.p = append(*w.p, p...)
	return len(p), nil
}

func (w *Walker) printNode(node interface{}) string {
	w.Buf = w.Buf[:0]
	err := (&printer.Config{Mode: printer.UseSpaces, Tabwidth: 4}).Fprint(sliceWriter{&w.Buf}, w.Fset, node)
	if err != nil {
		return err.Error()
	}
	return string(w.Buf)
}

var exampleOutputRx = regexp.MustCompile(`(?i)//[[:space:]]*output:`)

func (w *Walker) getExamples() {
	var docs []*Example
	for _, e := range w.Examples {
		e.Name = strings.TrimPrefix(e.Name, "_")

		output := e.Output
		code := w.printNode(&printer.CommentedNode{
			Node:     e.Code,
			Comments: e.Comments,
		})

		// additional formatting if this is a function body
		if i := len(code); i >= 2 && code[0] == '{' && code[i-1] == '}' {
			// remove surrounding braces
			code = code[1 : i-1]
			// unindent
			code = strings.Replace(code, "\n    ", "\n", -1)
			// remove output comment
			if j := exampleOutputRx.FindStringIndex(code); j != nil {
				code = strings.TrimSpace(code[:j[0]])
			}
		} else {
			// drop output, as the output comment will appear in the code
			output = ""
		}

		// play := ""
		// if e.Play != nil {
		// 	w.buf = w.buf[:0]
		// 	if err := format.Node(sliceWriter{&w.buf}, w.fset, e.Play); err != nil {
		// 		play = err.Error()
		// 	} else {
		// 		play = string(w.buf)
		// 	}
		// }

		docs = append(docs, &Example{
			Name:   e.Name,
			Doc:    e.Doc,
			Code:   code,
			Output: output,
		})
		//Play:   play
	}

	w.Pdoc.Examples = docs
}

func (w *Walker) printDecl(decl ast.Node) string {
	var d Code
	d, w.Buf = printDecl(decl, w.Fset, w.Buf)
	return d.Text
}

func (w *Walker) printPos(pos token.Pos) string {
	position := w.Fset.Position(pos)
	src := w.SrcFiles[position.Filename]
	if src == nil || src.BrowseUrl == "" {
		// src can be nil when line comments are used (//line <file>:<line>).
		return ""
	}
	return src.BrowseUrl + fmt.Sprintf(w.LineFmt, position.Line)
}

func (w *Walker) values(vdocs []*doc.Value) (vals []*Value) {
	for _, d := range vdocs {
		vals = append(vals, &Value{
			Decl: w.printDecl(d.Decl),
			URL:  w.printPos(d.Decl.Pos()),
			Doc:  d.Doc,
		})
	}

	return vals
}

// printCode returns function or method code from source files.
func (w *Walker) printCode(decl ast.Node) string {
	pos := decl.Pos()
	posPos := w.Fset.Position(pos)
	src := w.SrcFiles[posPos.Filename]
	if src == nil || src.BrowseUrl == "" {
		// src can be nil when line comments are used (//line <file>:<line>).
		return ""
	}

	code, ok := w.SrcLines[posPos.Filename]
	// Check source file line arrays.
	if !ok {
		// Split source file to array and save into map when at the 1st time.
		w.SrcLines[posPos.Filename] = strings.Split(string(src.Data()), "\n")
		code = w.SrcLines[posPos.Filename]
	}

	// Get code.
	var buf bytes.Buffer
	l := len(code)
CutCode:
	for i := posPos.Line; i < l; i++ {
		// Check end of code block.
		switch {
		case len(code[i]) > 0 && code[i][0] == '}': // Normal end.
			break CutCode
		case (i == posPos.Line) && len(code[i]) == 0 && (strings.Index(code[i-1], "{") == -1): // Package `builtin`.
			break CutCode
		case len(code[i-1]) > 4 && code[i-1][:4] == "func" &&
			code[i-1][len(code[i-1])-1] == '}': // One line functions.
			line := code[i-1]
			buf.WriteString("       ")
			buf.WriteString(line[strings.Index(line, "{")+1 : len(line)-1])
			buf.WriteByte('\n')
			break CutCode
		}

		buf.WriteString(code[i])
		buf.WriteByte('\n')
	}
	return buf.String()
}

func (w *Walker) funcs(fdocs []*doc.Func) (funcs []*Func, ifuncs []*Func) {
	isBuiltIn := w.Pdoc.ImportPath == "builtin"
	for _, d := range fdocs {
		if unicode.IsUpper(rune(d.Name[0])) || isBuiltIn {
			// var exampleName string
			// switch {
			// case d.Recv == "":
			// 	exampleName = d.Name
			// case d.Recv[0] == '*':
			// 	exampleName = d.Recv[1:] + "_" + d.Name
			// default:
			// 	exampleName = d.Recv + "_" + d.Name
			// }
			funcs = append(funcs, &Func{
				Decl: w.printDecl(d.Decl),
				URL:  w.printPos(d.Decl.Pos()),
				Doc:  d.Doc,
				Name: d.Name,
				Code: w.printCode(d.Decl),
				// Recv:     d.Recv,
				// Examples: w.getExamples(exampleName),
			})
			continue
		}

		ifuncs = append(ifuncs, &Func{
			Decl: w.printDecl(d.Decl),
			URL:  w.printPos(d.Decl.Pos()),
			Doc:  d.Doc,
			Name: d.Name,
			Code: w.printCode(d.Decl),
		})
	}

	return funcs, ifuncs
}

func (w *Walker) types(tdocs []*doc.Type) (tps []*Type, itps []*Type) {
	isBuiltIn := w.Pdoc.ImportPath == "builtin"
	for _, d := range tdocs {
		funcs, ifuncs := w.funcs(d.Funcs)
		meths, imeths := w.funcs(d.Methods)

		if unicode.IsUpper(rune(d.Name[0])) || isBuiltIn {
			tps = append(tps, &Type{
				Doc:      d.Doc,
				Name:     d.Name,
				Decl:     w.printDecl(d.Decl),
				URL:      w.printPos(d.Decl.Pos()),
				Consts:   w.values(d.Consts),
				Vars:     w.values(d.Vars),
				Funcs:    funcs,
				IFuncs:   ifuncs,
				Methods:  meths,
				IMethods: imeths,
				// Examples: w.getExamples(d.Name),
			})
			continue
		}

		itps = append(itps, &Type{
			Doc:      d.Doc,
			Name:     d.Name,
			Decl:     w.printDecl(d.Decl),
			URL:      w.printPos(d.Decl.Pos()),
			Consts:   w.values(d.Consts),
			Vars:     w.values(d.Vars),
			Funcs:    funcs,
			IFuncs:   ifuncs,
			Methods:  meths,
			IMethods: imeths,
		})
	}
	return tps, itps
}

func (w *Walker) isCgo() bool {
	for _, name := range w.Pdoc.Imports {
		if name == "C" || name == "os/user" {
			return true
		}
	}
	return false
}

var goEnvs = []struct{ GOOS, GOARCH string }{
	{"linux", "amd64"},
	{"darwin", "amd64"},
	{"windows", "amd64"},
}

// Build generates documentation from given source files through 'WalkType'.
func (w *Walker) Build(wr *WalkRes) (*Package, error) {
	ctxt := build.Context{
		CgoEnabled:  true,
		ReleaseTags: build.Default.ReleaseTags,
		BuildTags:   build.Default.BuildTags,
		Compiler:    "gc",
	}

	if w.Pdoc.PkgDecl == nil {
		w.Pdoc.PkgDecl = &PkgDecl{}
	}

	// Check 'WalkType'.
	switch wr.WalkType {
	case WT_Local:
		// Check root path.
		if len(wr.RootPath) == 0 {
			return nil, errors.New("WT_Local: empty root path")
		} else if !com.IsDir(wr.RootPath) {
			return nil, errors.New("WT_Local: cannot find specific directory or it's a file")
		}

		w.setLocalContext(&ctxt)
		return nil, errors.New("Hasn't supported yet!")
	case WT_Memory:
		// Convert source files.
		w.SrcFiles = make(map[string]*Source)
		w.Pdoc.Readme = make(map[string][]byte)
		for _, src := range wr.Srcs {
			srcName := strings.ToLower(src.Name()) // For readme comparation.
			switch {
			case strings.HasSuffix(src.Name(), ".go"):
				w.SrcFiles[src.Name()] = src
			case len(w.Pdoc.Tag) > 0 || (wr.WalkMode&WM_NoReadme != 0):
				// This means we are not on the latest version of the code,
				// so we do not collect the README files.
				continue
			case strings.HasPrefix(srcName, "readme_zh") || strings.HasPrefix(srcName, "readme_cn"):
				w.Pdoc.Readme["zh"] = src.Data()
			case strings.HasPrefix(srcName, "readme"):
				w.Pdoc.Readme["en"] = src.Data()
			}
		}

		// Check source files.
		if w.SrcFiles == nil {
			return nil, errors.New("WT_Memory: no Go source file")
		}

		w.setMemoryContext(&ctxt)

	default:
		return nil, errors.New("Hasn't supported yet!")
	}

	var err error
	var bpkg *build.Package

	for _, env := range goEnvs {
		ctxt.GOOS = env.GOOS
		ctxt.GOARCH = env.GOARCH

		bpkg, err = ctxt.ImportDir(w.Pdoc.ImportPath, 0)
		// Continue if there are no Go source files; we still want the directory info.
		_, nogo := err.(*build.NoGoError)
		if err != nil {
			if nogo {
				err = nil
			} else {
				return nil, errors.New("Walker.Build -> ImportDir: " + err.Error())
			}
		}
	}

	w.Pdoc.IsCmd = bpkg.IsCommand()
	w.Pdoc.Synopsis = synopsis(bpkg.Doc)

	w.Pdoc.Imports = bpkg.Imports
	w.Pdoc.IsCgo = w.isCgo()
	w.Pdoc.TestImports = bpkg.TestImports

	// Check depth.
	if wr.WalkDepth <= WD_Imports {
		return w.Pdoc, nil
	}

	w.Fset = token.NewFileSet()
	// Parse the Go files
	files := make(map[string]*ast.File)
	for _, name := range append(bpkg.GoFiles, bpkg.CgoFiles...) {
		file, err := parser.ParseFile(w.Fset, name, w.SrcFiles[name].Data(), parser.ParseComments)
		if err != nil {
			return nil, errors.New("Walker.Build -> parse Go files: " + err.Error())
			continue
		}
		w.Pdoc.Files = append(w.Pdoc.Files, w.SrcFiles[name])
		// w.Pdoc.SourceSize += int64(len(w.SrcFiles[name].Data()))
		files[name] = file
	}

	w.apkg, _ = ast.NewPackage(w.Fset, files, poorMansImporter, nil)

	// Find examples in the test files.
	for _, name := range append(bpkg.TestGoFiles, bpkg.XTestGoFiles...) {
		file, err := parser.ParseFile(w.Fset, name, w.SrcFiles[name].Data(), parser.ParseComments)
		if err != nil {
			return nil, errors.New("Walker.Build -> find examples: " + err.Error())
			continue
		}
		w.Pdoc.TestFiles = append(w.Pdoc.TestFiles, w.SrcFiles[name])
		//w.pdoc.TestSourceSize += len(w.srcs[name].data)

		if wr.WalkMode&WM_NoExample != 0 {
			continue
		}
		w.Examples = append(w.Examples, doc.Examples(file)...)
	}

	mode := doc.Mode(0)
	if w.Pdoc.ImportPath == "builtin" || wr.BuildAll {
		mode |= doc.AllDecls
	}
	pdoc := doc.New(w.apkg, w.Pdoc.ImportPath, mode)

	// Get doc.
	pdoc.Doc = strings.TrimRight(pdoc.Doc, " \t\n\r")
	var buf bytes.Buffer
	doc.ToHTML(&buf, pdoc.Doc, nil)
	w.Pdoc.Doc = buf.String()
	// Highlight first sentence.
	w.Pdoc.Doc = strings.Replace(w.Pdoc.Doc, "<p>", "<p><b>", 1)
	w.Pdoc.Doc = strings.Replace(w.Pdoc.Doc, "</p>", "</b></p>", 1)

	if wr.WalkMode&WM_NoExample == 0 {
		w.getExamples()
	}

	w.SrcLines = make(map[string][]string)
	w.Pdoc.Consts = w.values(pdoc.Consts)
	w.Pdoc.Funcs, w.Pdoc.Ifuncs = w.funcs(pdoc.Funcs)
	w.Pdoc.Types, w.Pdoc.Itypes = w.types(pdoc.Types)
	w.Pdoc.Vars = w.values(pdoc.Vars)
	w.Pdoc.ImportPaths = strings.Join(pdoc.Imports, "|")
	w.Pdoc.ImportNum = int64(len(pdoc.Imports))
	//w.Pdoc.Notes = w.notes(pdoc.Notes)

	return w.Pdoc, nil
}
