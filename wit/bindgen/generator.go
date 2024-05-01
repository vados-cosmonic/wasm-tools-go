// Package bindgen generates Go source code from a fully-resolved WIT package.
// It generates one or more Go packages, with functions, types, constants, and variables,
// along with the associated code to lift and lower Go types into Canonical ABI representation.
package bindgen

import (
	"bytes"
	"fmt"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/ydnar/wasm-tools-go/cm"
	"github.com/ydnar/wasm-tools-go/internal/codec"
	"github.com/ydnar/wasm-tools-go/internal/go/gen"
	"github.com/ydnar/wasm-tools-go/internal/stringio"
	"github.com/ydnar/wasm-tools-go/wit"
)

const (
	GoSuffix     = ".wit.go"
	BuildDefault = "!wasip1"
	cmPackage    = "github.com/ydnar/wasm-tools-go/cm"
	emptyAsm     = `// This file exists for testing this package without WebAssembly,
// allowing empty function bodies with a //go:wasmimport directive.
// See https://pkg.go.dev/cmd/compile for more information.
`
)

type typeDecl struct {
	file  *gen.File // The Go file this type belongs to
	scope gen.Scope // Scope for type-local declarations like method names
	name  string    // The unique Go name for this type
}

type funcDecl struct {
	f        function // The exported Go function
	imported function // The go:wasmimport function
	exported function // The go:wasmexport function
}

// function represents a Go function created from a Component Model function
type function struct {
	file     *gen.File // The Go file this function belongs to
	scope    gen.Scope // Scope for function-local declarations
	name     string    // The scoped unique Go name for this function (method names are scoped to receiver type)
	receiver param     // The method receiver, if any
	params   []param   // Function param(s), with unique Go name(s)
	results  []param   // Function result(s), with unique Go name(s)
}

func (f *function) isMethod() bool {
	return f.receiver.typ != nil
}

// param represents a Go function parameter or result.
// name is a unique Go name within the function scope.
type param struct {
	name string
	typ  wit.Type
}

func goFunction(file *gen.File, f *wit.Function, goName string) function {
	scope := gen.NewScope(file)
	out := function{
		file:    file,
		scope:   scope,
		name:    goName,
		params:  goParams(scope, f.Params),
		results: goParams(scope, f.Results),
	}
	if len(out.results) == 1 && out.results[0].name == "" {
		out.results[0].name = "result"
	}
	if f.IsMethod() {
		out.receiver = out.params[0]
		out.params = out.params[1:]
	}
	return out
}

func goParams(scope gen.Scope, params []wit.Param) []param {
	out := make([]param, len(params))
	for i := range params {
		out[i].name = scope.DeclareName(GoName(params[i].Name, false))
		out[i].typ = params[i].Type
	}
	return out
}

type generator struct {
	opts options
	res  *wit.Resolve

	// versioned is set to true if there are multiple versions of a WIT package in res,
	// which affects the generated Go package paths.
	versioned bool

	// packages are Go packages indexed on Go package paths.
	packages map[string]*gen.Package

	// witPackages map WIT identifier paths to Go packages.
	witPackages map[string]*gen.Package

	// typeDefs map [wit.TypeDef] to their Go equivalent.
	typeDefs map[*wit.TypeDef]typeDecl

	// functions map [wit.Function] to their Go equivalent.
	functions map[*wit.Function]funcDecl

	// defined represent whether a type or function has been defined.
	defined map[any]bool
}

func newGenerator(res *wit.Resolve, opts ...Option) (*generator, error) {
	g := &generator{
		packages:    make(map[string]*gen.Package),
		witPackages: make(map[string]*gen.Package),
		typeDefs:    make(map[*wit.TypeDef]typeDecl),
		functions:   make(map[*wit.Function]funcDecl),
		defined:     make(map[any]bool),
	}
	err := g.opts.apply(opts...)
	if err != nil {
		return nil, err
	}
	if g.opts.generatedBy == "" {
		_, file, _, _ := runtime.Caller(0)
		_, g.opts.generatedBy = filepath.Split(filepath.Dir(filepath.Dir(file)))
	}
	if g.opts.cmPackage == "" {
		g.opts.cmPackage = cmPackage
	}
	g.res = res
	return g, nil
}

func (g *generator) generate() ([]*gen.Package, error) {
	g.detectVersionedPackages()
	err := g.defineWorlds()
	if err != nil {
		return nil, err
	}
	var packages []*gen.Package
	for _, path := range codec.SortedKeys(g.packages) {
		packages = append(packages, g.packages[path])
	}
	return packages, nil
}

func (g *generator) detectVersionedPackages() {
	if g.opts.versioned {
		g.versioned = true
		// fmt.Fprintf(os.Stderr, "Generated versions for all package(s)\n")
		return
	}
	packages := make(map[string]string)
	for _, pkg := range g.res.Packages {
		id := pkg.Name
		id.Version = nil
		path := id.String()
		if packages[path] != "" && packages[path] != pkg.Name.String() {
			g.versioned = true
		} else {
			packages[path] = pkg.Name.String()
		}
	}
	// if g.versioned {
	// 	fmt.Fprintf(os.Stderr, "Multiple versions of package(s) detected\n")
	// }
}

// By default, each WIT interface and world maps to a single Go package.
// Options might override the Go package, including combining multiple
// WIT interfaces and/or worlds into a single Go package.
func (g *generator) defineWorlds() error {
	// fmt.Fprintf(os.Stderr, "Generating Go for %d world(s)\n", len(g.res.Worlds))
	for i, w := range g.res.Worlds {
		if matchWorld(w, g.opts.world) || (g.opts.world == "" && i == len(g.res.Worlds)-1) {
			g.defineWorld(w)
		}
	}
	return nil
}

func matchWorld(w *wit.World, name string) bool {
	if name == w.Name {
		return true
	}
	id := w.Package.Name
	id.Extension = w.Name
	if name == id.String() {
		return true
	}
	id.Version = nil
	return name == id.String()
}

func (g *generator) defineWorld(w *wit.World) error {
	if g.defined[w] {
		return nil
	}
	id := w.Package.Name
	id.Extension = w.Name
	pkg := g.packageFor(id)
	file := g.fileFor(id)

	{
		var b strings.Builder
		stringio.Write(&b, "Package ", pkg.Name, " represents the ", w.WITKind(), " \"", id.String(), "\".\n")
		if w.Docs.Contents != "" {
			b.WriteString("\n")
			b.WriteString(w.Docs.Contents)
		}
		file.PackageDocs = b.String()
	}

	var err error
	w.Imports.All()(func(name string, v wit.WorldItem) bool {
		switch v := v.(type) {
		case *wit.Interface:
			err = g.defineInterface(v, name)
		case *wit.TypeDef:
			err = g.defineTypeDef(v, name)
		case *wit.Function:
			if v.IsFreestanding() {
				err = g.defineFunction(id, v)
			}
		}
		return err == nil
	})
	if err != nil {
		return err
	}

	g.defined[w] = true

	return nil
}

func (g *generator) defineInterface(i *wit.Interface, name string) error {
	if g.defined[i] {
		return nil
	}
	if i.Name != nil {
		name = *i.Name
	}
	id := i.Package.Name
	id.Extension = name
	pkg := g.packageFor(id)
	file := g.fileFor(id)

	{
		var b strings.Builder
		stringio.Write(&b, "Package ", pkg.Name, " represents the ", i.WITKind(), " \"", id.String(), "\".\n")
		if i.Docs.Contents != "" {
			b.WriteString("\n")
			b.WriteString(i.Docs.Contents)
		}
		file.PackageDocs = b.String()
	}

	// Define types
	i.TypeDefs.All()(func(name string, td *wit.TypeDef) bool {
		g.defineTypeDef(td, name)
		return true
	})

	// Declare all functions
	i.Functions.All()(func(_ string, f *wit.Function) bool {
		g.declareFunction(id, f)
		return true
	})

	// Define standalone functions
	i.Functions.All()(func(_ string, f *wit.Function) bool {
		if f.IsFreestanding() {
			g.defineFunction(id, f)
		}
		return true
	})

	// Define export interface
	if g.opts.exports {
		var b bytes.Buffer
		stringio.Write(&b, "type ", pkg.GetName("Interface"), " interface {\n")

		i.Functions.All()(func(_ string, f *wit.Function) bool {
			if !f.IsMethod() {
				d := g.functions[f]
				stringio.Write(&b, g.functionSignature(file, d.f), "\n")
			}
			return true
		})

		b.WriteString("}\n\n")
		stringio.Write(&b, "var ", pkg.GetName("instance"), " ", pkg.GetName("Interface"), "\n\n")

		// TODO: enable writing exports interface
		_, err := file.Write(b.Bytes())
		if err != nil {
			return err
		}
	}

	g.defined[i] = true

	return nil
}

func (g *generator) defineTypeDef(t *wit.TypeDef, name string) error {
	if g.defined[t] {
		return nil
	}
	if t.Name != nil {
		name = *t.Name
	}

	decl, err := g.declareTypeDef(nil, t, "")
	if err != nil {
		return err
	}
	owner := typeDefOwner(t)

	// If an alias, get root
	root := t.Root()
	rootOwner := typeDefOwner(root)
	rootName := name
	if root.Name != nil {
		rootName = *root.Name
	}

	// Define the type
	var b bytes.Buffer
	stringio.Write(&b, "// ", decl.name, " represents the ", root.WITKind(), " \"", rootOwner.String(), "#", rootName, "\".\n")
	b.WriteString("//\n")
	if root != t {
		// Type alias
		stringio.Write(&b, "// See [", g.typeRep(decl.file, root), "] for more information.\n")
		stringio.Write(&b, "type ", decl.name, " = ", g.typeRep(decl.file, root), "\n\n")
	} else {
		b.WriteString(formatDocComments(t.Docs.Contents, false))
		b.WriteString("//\n")
		b.WriteString(formatDocComments(t.WIT(nil, ""), true))
		stringio.Write(&b, "type ", decl.name, " ", g.typeDefRep(decl.file, t, decl.name), "\n\n")
	}

	_, err = decl.file.Write(b.Bytes())
	if err != nil {
		return err
	}

	g.defined[t] = true

	// Define any associated functions
	if f := t.ResourceDrop(); f != nil {
		err := g.defineFunction(owner, f)
		if err != nil {
			return nil
		}
	}

	if f := t.Constructor(); f != nil {
		err := g.defineFunction(owner, f)
		if err != nil {
			return nil
		}
	}

	for _, f := range t.StaticFunctions() {
		err := g.defineFunction(owner, f)
		if err != nil {
			return nil
		}
	}

	for _, f := range t.Methods() {
		err := g.defineFunction(owner, f)
		if err != nil {
			return nil
		}
	}

	return nil
}

func (g *generator) declareTypeDef(file *gen.File, t *wit.TypeDef, goName string) (typeDecl, error) {
	decl, ok := g.typeDefs[t]
	if ok {
		return decl, nil
	}
	if goName == "" {
		if t.Name == nil {
			return decl, nil
		}
		goName = GoName(*t.Name, true)
	}
	if file == nil {
		file = g.fileFor(typeDefOwner(t))
	}
	decl = typeDecl{
		file:  file,
		name:  file.DeclareName(goName),
		scope: gen.NewScope(nil),
	}
	g.typeDefs[t] = decl
	// fmt.Fprintf(os.Stderr, "Type:\t%s.%s\n\t%s.%s\n", owner.String(), name, decl.Package.Path, decl.Name)
	return decl, nil
}

func typeDefOwner(t *wit.TypeDef) wit.Ident {
	var id wit.Ident
	switch owner := t.Owner.(type) {
	case *wit.World:
		id = owner.Package.Name
		id.Extension = owner.Name
	case *wit.Interface:
		id = owner.Package.Name
		if owner.Name == nil {
			id.Extension = "unknown"
		} else {
			id.Extension = *owner.Name
		}
	}
	return id
}

func (g *generator) typeDefRep(file *gen.File, t *wit.TypeDef, goName string) string {
	return g.typeDefKindRep(file, t.Kind, goName)
}

func (g *generator) typeDefKindRep(file *gen.File, kind wit.TypeDefKind, goName string) string {
	switch kind := kind.(type) {
	case *wit.Pointer:
		return g.pointerRep(file, kind)
	case wit.Type:
		return g.typeRep(file, kind)
	case *wit.Record:
		return g.recordRep(file, kind, goName)
	case *wit.Tuple:
		return g.tupleRep(file, kind)
	case *wit.Flags:
		return g.flagsRep(file, kind, goName)
	case *wit.Enum:
		return g.enumRep(file, kind, goName)
	case *wit.Variant:
		return g.variantRep(file, kind, goName)
	case *wit.Result:
		return g.resultRep(file, kind)
	case *wit.Option:
		return g.optionRep(file, kind)
	case *wit.List:
		return g.listRep(file, kind)
	case *wit.Resource:
		return g.resourceRep(file, kind)
	case *wit.Own:
		return g.ownRep(file, kind)
	case *wit.Borrow:
		return g.borrowRep(file, kind)
	case *wit.Future:
		return "any /* TODO: *wit.Future */"
	case *wit.Stream:
		return "any /* TODO: *wit.Stream */"
	default:
		panic(fmt.Sprintf("BUG: unknown wit.TypeDefKind %T", kind)) // should never reach here
	}
}

func (g *generator) pointerRep(file *gen.File, p *wit.Pointer) string {
	return "*" + g.typeRep(file, p.Type)
}

func (g *generator) typeRep(file *gen.File, t wit.Type) string {
	switch t := t.(type) {
	// Special-case nil for the _ in result<T, _>
	case nil:
		return "struct{}"

	case *wit.TypeDef:
		if td, ok := g.typeDefs[t]; ok {
			return file.RelativeName(td.file.Package, td.name)
		}
		// FIXME: this is only valid for built-in WIT types.
		// User-defined types must be named, so the Ident check above must have succeeded.
		// See https://component-model.bytecodealliance.org/design/wit.html#built-in-types
		// and https://component-model.bytecodealliance.org/design/wit.html#user-defined-types.
		// TODO: add wit.Type.BuiltIn() method?
		return g.typeDefRep(file, t, "")
	case wit.Primitive:
		return g.primitiveRep(t)
	default:
		panic(fmt.Sprintf("BUG: unknown wit.Type %T", t)) // should never reach here
	}
}

func (g *generator) primitiveRep(p wit.Primitive) string {
	switch p := p.(type) {
	case wit.Bool:
		return "bool"
	case wit.S8:
		return "int8"
	case wit.U8:
		return "uint8"
	case wit.S16:
		return "int16"
	case wit.U16:
		return "uint16"
	case wit.S32:
		return "int32"
	case wit.U32:
		return "uint32"
	case wit.S64:
		return "int64"
	case wit.U64:
		return "uint64"
	case wit.F32:
		return "float32"
	case wit.F64:
		return "float64"
	case wit.Char:
		return "rune"
	case wit.String:
		return "string"
	default:
		panic(fmt.Sprintf("BUG: unknown wit.Primitive %T", p)) // should never reach here
	}
}

func (g *generator) recordRep(file *gen.File, r *wit.Record, goName string) string {
	exported := token.IsExported(goName)
	var b strings.Builder
	b.WriteString("struct {")
	for i, f := range r.Fields {
		if i == 0 || i > 0 && f.Docs.Contents != "" {
			b.WriteRune('\n')
		}
		b.WriteString(formatDocComments(f.Docs.Contents, false))
		stringio.Write(&b, fieldName(f.Name, exported), " ", g.typeRep(file, f.Type), "\n")
	}
	b.WriteRune('}')
	return b.String()
}

// Field names are implicitly scoped to their parent struct,
// so we don't need to track the mapping between WIT names and Go names.
func fieldName(name string, export bool) string {
	if name == "" {
		return ""
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "f" + name
	}
	return GoName(name, export)
}

func (g *generator) tupleRep(file *gen.File, t *wit.Tuple) string {
	var b strings.Builder
	if typ := t.Type(); typ != nil {
		stringio.Write(&b, "[", strconv.Itoa(len(t.Types)), "]", g.typeRep(file, typ))
	} else if len(t.Types) == 0 || len(t.Types) > cm.MaxTuple {
		// Force struct representation
		return g.typeDefKindRep(file, t.Despecialize(), "")
	} else {
		stringio.Write(&b, file.Import(g.opts.cmPackage), ".Tuple")
		if len(t.Types) > 2 {
			b.WriteString(strconv.Itoa(len(t.Types)))
		}
		b.WriteRune('[')
		for i, typ := range t.Types {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(g.typeRep(file, typ))
		}
		b.WriteRune(']')
	}
	return b.String()
}

func (g *generator) flagsRep(file *gen.File, flags *wit.Flags, goName string) string {
	var b strings.Builder

	// FIXME: this isn't ideal
	var typ wit.Type
	size := flags.Size()
	switch size {
	case 1:
		typ = wit.U8{}
	case 2:
		typ = wit.U16{}
	case 4:
		typ = wit.U32{}
	case 8:
		typ = wit.U64{}
	default:
		panic(fmt.Sprintf("FIXME: cannot emit a flags type with %d cases", len(flags.Flags)))
	}

	b.WriteString(g.typeRep(file, typ))
	b.WriteString("\n\n")
	b.WriteString("const (\n")
	for i, flag := range flags.Flags {
		if i > 0 && flag.Docs.Contents != "" {
			b.WriteRune('\n')
		}
		b.WriteString(formatDocComments(flag.Docs.Contents, false))
		flagName := file.DeclareName(goName + GoName(flag.Name, true))
		b.WriteString(flagName)
		if i == 0 {
			stringio.Write(&b, " ", goName, " = 1 << iota")
		}
		b.WriteRune('\n')
	}
	b.WriteString(")\n")
	return b.String()
}

func (g *generator) enumRep(file *gen.File, e *wit.Enum, goName string) string {
	var b strings.Builder
	disc := wit.Discriminant(len(e.Cases))
	b.WriteString(g.typeRep(file, disc))
	b.WriteString("\n\n")
	b.WriteString("const (\n")
	for i, c := range e.Cases {
		if i > 0 && c.Docs.Contents != "" {
			b.WriteRune('\n')
		}
		b.WriteString(formatDocComments(c.Docs.Contents, false))
		b.WriteString(file.DeclareName(goName + GoName(c.Name, true)))
		if i == 0 {
			b.WriteRune(' ')
			b.WriteString(goName)
			b.WriteString(" = iota")
		}
		b.WriteRune('\n')
	}
	b.WriteString(")\n")
	return b.String()
}

func (g *generator) variantRep(file *gen.File, v *wit.Variant, goName string) string {
	// If the variant has no associated types, represent the variant as an enum.
	if e := v.Enum(); e != nil {
		return g.enumRep(file, e, goName)
	}

	disc := wit.Discriminant(len(v.Cases))
	shape := variantShape(v)
	align := variantAlign(v)

	// Emit type
	var b strings.Builder
	cm := file.Import(g.opts.cmPackage)
	stringio.Write(&b, cm, ".Variant[", g.typeRep(file, disc), ", ", g.typeRep(file, shape), ", ", g.typeRep(file, align), "]\n\n")

	// Emit cases
	for i, c := range v.Cases {
		caseNum := strconv.Itoa(i)
		caseName := GoName(c.Name, true)
		constructorName := file.DeclareName(goName + caseName)
		typeRep := g.typeRep(file, c.Type)

		// Emit constructor
		stringio.Write(&b, "// ", constructorName, " returns a [", goName, "] of case \"", c.Name, "\".\n")
		b.WriteString("//\n")
		b.WriteString(formatDocComments(c.Docs.Contents, false))
		stringio.Write(&b, "func ", constructorName, "(")
		dataName := "data"
		if c.Type != nil {
			stringio.Write(&b, dataName, " ", typeRep)
		}
		stringio.Write(&b, ") ", goName, " {")
		if c.Type == nil {
			stringio.Write(&b, "var ", dataName, " ", typeRep, "\n")
		}
		stringio.Write(&b, "return ", cm, ".New[", goName, "](", caseNum, ", ", dataName, ")\n")
		b.WriteString("}\n\n")

		// Emit getter
		if c.Type == nil {
			// Case without an associated type returns bool
			stringio.Write(&b, "// ", caseName, " returns true if [", goName, "] represents the variant case \"", c.Name, "\".\n")
			stringio.Write(&b, "func (self *", goName, ") ", caseName, "() bool {\n")
			stringio.Write(&b, "return ", cm, ".Tag(self) == ", caseNum)
			b.WriteString("}\n\n")
		} else {
			// Case with associated type T returns *T
			stringio.Write(&b, "// ", caseName, " returns a non-nil *[", typeRep, "] if [", goName, "] represents the variant case \"", c.Name, "\".\n")
			stringio.Write(&b, "func (self *", goName, ") ", caseName, "() *", typeRep, " {\n")
			stringio.Write(&b, "return ", cm, ".Case[", typeRep, "](self, ", caseNum, ")")
			b.WriteString("}\n\n")
		}
	}
	return b.String()
}

func (g *generator) resultRep(file *gen.File, r *wit.Result) string {
	var b strings.Builder
	b.WriteString(file.Import(g.opts.cmPackage))
	if r.OK == nil && r.Err == nil {
		b.WriteString(".Result")
	} else if r.OK == nil || (r.Err != nil && r.Err.Size() > r.OK.Size()) {
		stringio.Write(&b, ".ErrResult[", g.typeRep(file, r.OK), ", ", g.typeRep(file, r.Err), "]")
	} else {
		stringio.Write(&b, ".OKResult[", g.typeRep(file, r.OK), ", ", g.typeRep(file, r.Err), "]")
	}
	return b.String()
}

func (g *generator) optionRep(file *gen.File, o *wit.Option) string {
	var b strings.Builder
	stringio.Write(&b, file.Import(g.opts.cmPackage), ".Option[", g.typeRep(file, o.Type), "]")
	return b.String()
}

func (g *generator) listRep(file *gen.File, l *wit.List) string {
	var b strings.Builder
	stringio.Write(&b, file.Import(g.opts.cmPackage), ".List[", g.typeRep(file, l.Type), "]")
	return b.String()
}

func (g *generator) resourceRep(file *gen.File, r *wit.Resource) string {
	var b strings.Builder
	stringio.Write(&b, file.Import(g.opts.cmPackage), ".Resource")
	return b.String()
}

func (g *generator) ownRep(file *gen.File, o *wit.Own) string {
	return g.typeRep(file, o.Type)
}

func (g *generator) borrowRep(file *gen.File, b *wit.Borrow) string {
	return g.typeRep(file, b.Type)
}

func (g *generator) declareFunction(owner wit.Ident, f *wit.Function) (funcDecl, error) {
	d, ok := g.functions[f]
	if ok {
		return d, nil
	}

	file := g.fileFor(owner)

	// Setup
	exported := f.CoreFunction(wit.Exported)
	imported := f.CoreFunction(wit.Imported)

	const (
		pfxExport = "wasmexport_"
		pfxImport = "wasmimport_"
	)

	var funcName string
	var exportName string
	var importName string
	switch f.Kind.(type) {
	case *wit.Freestanding:
		funcName = file.DeclareName(GoName(f.BaseName(), true))
		exportName = file.DeclareName(pfxExport + funcName)
		importName = file.DeclareName(pfxImport + funcName)

	case *wit.Constructor:
		t := f.Type().(*wit.TypeDef)
		funcName = file.DeclareName("New" + g.typeDefs[t].name)
		exportName = file.DeclareName(pfxExport + funcName)
		importName = file.DeclareName(pfxImport + funcName)

	case *wit.Static:
		t := f.Type().(*wit.TypeDef)
		funcName = file.DeclareName(g.typeDefs[t].name + GoName(f.BaseName(), true))
		exportName = file.DeclareName(pfxExport + funcName)
		importName = file.DeclareName(pfxImport + funcName)

	case *wit.Method:
		t := f.Type().(*wit.TypeDef)
		if t.Package().Name.Package != owner.Package {
			return d, fmt.Errorf("cannot emit functions in package %s to type %s", owner.Package, t.Package().Name.String())
		}
		scope := g.typeDefs[t].scope
		funcName = scope.DeclareName(GoName(f.BaseName(), true))
		exportName = file.DeclareName(pfxExport + g.typeDefs[t].name + funcName)
		if imported.IsMethod() {
			importName = scope.DeclareName(pfxImport + funcName)
		} else {
			importName = file.DeclareName(pfxImport + g.typeDefs[t].name + funcName)
		}
	}

	d = funcDecl{
		f:        goFunction(file, f, funcName),
		exported: goFunction(file, exported, exportName),
		imported: goFunction(file, imported, importName),
	}

	g.functions[f] = d

	return d, nil
}

func (g *generator) defineFunction(owner wit.Ident, f *wit.Function) error {
	if g.defined[f] {
		return nil
	}

	// Setup
	decl, err := g.declareFunction(owner, f)
	if err != nil {
		return err
	}
	file := decl.f.file

	// Bridging between Go and wasmimport function
	callParams := slices.Clone(decl.imported.params)
	for i := range callParams {
		j := i - len(decl.f.params)
		if j < 0 {
			callParams[i].name = decl.f.params[i].name
		} else {
			callParams[i].name = decl.f.results[j].name
		}
	}

	var compoundParams param
	if len(decl.f.params) > 0 && derefAnonRecord(decl.imported.params[0].typ) != nil {
		name := decl.f.scope.DeclareName("params")
		callParams[0].name = name
		t := derefAnonRecord(decl.imported.params[0].typ)
		g.declareTypeDef(file, t, decl.imported.name+"Params")
		compoundParams.name = name
		compoundParams.typ = t
	}

	var compoundResults param
	var resultsRecord *wit.Record
	if len(decl.f.results) > 1 && derefAnonRecord(last(decl.imported.params).typ) != nil {
		name := decl.f.scope.DeclareName("results")
		last(callParams).name = name
		t := derefAnonRecord(last(decl.imported.params).typ)
		g.declareTypeDef(file, t, decl.imported.name+"Results")
		compoundResults.name = name
		compoundResults.typ = t
		resultsRecord = t.Kind.(*wit.Record)
	}

	var b bytes.Buffer

	// Emit Go function
	b.WriteString(g.functionDocs(owner, f, decl.f.name))
	b.WriteString("//go:nosplit\n")
	stringio.Write(&b, "func ", g.functionSignature(file, decl.f))

	// Emit function body
	b.WriteString(" {\n")
	sameResults := slices.Equal(decl.f.results, decl.imported.results)
	if len(decl.f.results) == 1 && !sameResults {
		for _, r := range decl.f.results {
			stringio.Write(&b, "var ", r.name, " ", g.typeRep(file, r.typ), "\n")
		}
	}

	// Emit compound types
	if compoundParams.typ != nil {
		stringio.Write(&b, compoundParams.name, " := ", g.typeRep(file, compoundParams.typ), "{ ")
		if decl.f.receiver.name != "" {
			stringio.Write(&b, decl.f.receiver.name, ", ")
		}
		for i, p := range decl.f.params {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(p.name)
		}
		b.WriteString(" }\n")
	}
	if compoundResults.typ != nil {
		stringio.Write(&b, "var ", compoundResults.name, " ", g.typeRep(file, compoundResults.typ), "\n")
	}

	// Emit call to wasmimport function
	if sameResults && len(decl.imported.results) > 0 {
		b.WriteString("return ")
	}
	if decl.imported.isMethod() {
		stringio.Write(&b, decl.imported.receiver.name, ".")
	}
	stringio.Write(&b, decl.imported.name, "(")
	for i, p := range callParams {
		if i > 0 {
			b.WriteString(", ")
		}
		if isPointer(p.typ) {
			b.WriteRune('&')
		}
		b.WriteString(callParams[i].name)
	}
	b.WriteString(")\n")
	if !sameResults {
		b.WriteString("return ")
		if resultsRecord != nil {
			for i, f := range resultsRecord.Fields {
				if i > 0 {
					b.WriteString(", ")
				}
				stringio.Write(&b, compoundResults.name, ".", fieldName(f.Name, false))
			}
		} else {
			for i, r := range decl.f.results {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(r.name)
			}
		}
		b.WriteRune('\n')
	}
	b.WriteString("}\n\n")

	// Emit wasmimport function
	stringio.Write(&b, "//go:wasmimport ", owner.String(), " ", f.Name, "\n")
	b.WriteString("//go:noescape\n")
	b.WriteString("func ")
	if decl.imported.isMethod() {
		stringio.Write(&b, "(", decl.imported.receiver.name, " ", g.typeRep(file, decl.imported.receiver.typ), ") ", decl.imported.name)
	} else {
		b.WriteString(decl.imported.name)
	}
	b.WriteRune('(')

	// Emit params
	for i, p := range decl.imported.params {
		if i > 0 {
			b.WriteString(", ")
		}
		stringio.Write(&b, p.name, " ", g.typeRep(file, p.typ))
	}
	b.WriteString(") ")

	// Emit results
	if len(decl.imported.results) == 1 {
		b.WriteString(g.typeRep(file, decl.imported.results[0].typ))
	} else if len(decl.imported.results) > 0 {
		b.WriteRune('(')
		for i, r := range decl.imported.results {
			if i > 0 {
				b.WriteString(", ")
			}
			stringio.Write(&b, r.name, " ", g.typeRep(file, r.typ))
		}
		b.WriteRune(')')
	}
	b.WriteString("\n\n")

	// Emit shared types
	if t, ok := compoundParams.typ.(*wit.TypeDef); ok {
		goName := g.typeDefs[t].name
		stringio.Write(&b, "// ", goName, " represents the flattened function params for [", decl.imported.name, "].\n")
		stringio.Write(&b, "// See the Canonical ABI flattening rules for more information.\n")
		stringio.Write(&b, "type ", goName, " ", g.typeDefRep(file, t, goName), "\n\n")
	}

	if t, ok := compoundResults.typ.(*wit.TypeDef); ok {
		goName := g.typeDefs[t].name
		stringio.Write(&b, "// ", goName, " represents the flattened function results for [", decl.imported.name, "].\n")
		stringio.Write(&b, "// See the Canonical ABI flattening rules for more information.\n")
		stringio.Write(&b, "type ", goName, " ", g.typeDefRep(file, t, goName), "\n\n")
	}

	// Write to file
	file.Write(b.Bytes())

	g.defined[f] = true

	return g.ensureEmptyAsm(file.Package)
}

func (g *generator) functionSignature(file *gen.File, f function) string {
	var b strings.Builder

	if f.isMethod() {
		stringio.Write(&b, "(", f.receiver.name, " ", g.typeRep(file, f.receiver.typ), ") ", f.name)
	} else {
		b.WriteString(f.name)
	}
	b.WriteRune('(')

	// Emit params
	for i, p := range f.params {
		if i > 0 {
			b.WriteString(", ")
		}
		stringio.Write(&b, p.name, " ", g.typeRep(file, p.typ))
	}
	b.WriteString(") ")

	// Emit results
	if len(f.results) == 1 {
		b.WriteString(g.typeRep(file, f.results[0].typ))
	} else if len(f.results) > 0 {
		b.WriteRune('(')
		for i, r := range f.results {
			if i > 0 {
				b.WriteString(", ")
			}
			stringio.Write(&b, r.name, " ", g.typeRep(file, r.typ))
		}
		b.WriteRune(')')
	}

	return b.String()
}

func last[S ~[]E, E any](s S) *E {
	if len(s) == 0 {
		return nil
	}
	return &s[len(s)-1]
}

func isPointer(t wit.Type) bool {
	if td, ok := t.(*wit.TypeDef); ok {
		if _, ok := td.Kind.(*wit.Pointer); ok {
			return true
		}
	}
	return false
}

func derefTypeDef(t wit.Type) *wit.TypeDef {
	if td, ok := t.(*wit.TypeDef); ok {
		if p, ok := td.Kind.(*wit.Pointer); ok {
			if td, ok := p.Type.(*wit.TypeDef); ok {
				return td
			}
		}
	}
	return nil
}

func derefAnonRecord(t wit.Type) *wit.TypeDef {
	if td := derefTypeDef(t); td != nil && td.Name == nil && td.Owner == nil {
		if _, ok := td.Kind.(*wit.Record); ok {
			return td
		}
	}
	return nil
}

func (g *generator) functionDocs(owner wit.Ident, f *wit.Function, goName string) string {
	var b strings.Builder
	kind := f.WITKind()
	if f.IsAdmin() {
		kind = "the Canonical ABI function"
	}
	stringio.Write(&b, "// ", goName, " represents ", kind, " \"")
	if f.IsFreestanding() {
		stringio.Write(&b, owner.String(), "#", f.Name)
	} else {
		stringio.Write(&b, f.BaseName())
	}
	b.WriteString("\".\n")
	if f.Docs.Contents != "" {
		b.WriteString("//\n")
		b.WriteString(formatDocComments(f.Docs.Contents, false))
	}
	b.WriteString("//\n")
	if !f.IsAdmin() {
		w := strings.TrimSuffix(f.WIT(nil, f.BaseName()), ";")
		b.WriteString(formatDocComments(w, true))
	}
	return b.String()
}

func (g *generator) ensureEmptyAsm(pkg *gen.Package) error {
	f := pkg.File("empty.s")
	if len(f.Content) > 0 {
		return nil
	}
	_, err := f.Write([]byte(emptyAsm))
	return err
}

func (g *generator) fileFor(id wit.Ident) *gen.File {
	pkg := g.packageFor(id)
	file := pkg.File(id.Extension + GoSuffix)
	file.GeneratedBy = g.opts.generatedBy
	file.Build = BuildDefault
	return file
}

func (g *generator) packageFor(id wit.Ident) *gen.Package {
	// Find existing
	pkg := g.witPackages[id.String()]
	if pkg != nil {
		return pkg
	}

	// Create the package path and name
	var segments []string
	if g.opts.packageRoot != "" && g.opts.packageRoot != "std" {
		segments = append(segments, g.opts.packageRoot)
	}
	segments = append(segments, id.Namespace, id.Package)
	if g.versioned && id.Version != nil {
		segments = append(segments, "v"+id.Version.String())
	}
	segments = append(segments, id.Extension)
	path := strings.Join(segments, "/")

	// TODO: write tests for this
	name := GoPackageName(id.Extension)
	// Ensure local name doesn’t conflict with Go keywords or predeclared identifiers
	if gen.UniqueName(name, gen.IsReserved) != name {
		// Try with package prefix, like error -> ioerror
		name = FlatName(id.Package + name)
		if gen.UniqueName(name, gen.IsReserved) != name {
			// Try with namespace prefix, like ioerror -> wasiioerror
			name = gen.UniqueName(FlatName(id.Namespace+name), gen.IsReserved)
		}
	}

	pkg = gen.NewPackage(path + "#" + name)
	g.packages[pkg.Path] = pkg
	g.witPackages[id.String()] = pkg

	// Predeclare a few names
	pkg.DeclareName("Interface")
	pkg.DeclareName("instance")
	pkg.DeclareName("Export")

	return pkg
}
