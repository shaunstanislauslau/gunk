package convert

import (
	"fmt"
	"go/format"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/scanner"

	"github.com/emicklei/proto"
	"github.com/knq/snaker"
)

type builder struct {
	// The current filename of file being converted
	filename string

	// The converted proto to gunk declarations. This only stores
	// the messages, enums and services. These get converted to as
	// they are found.
	translatedDeclarations []string

	// The package, option and imports from the proto file.
	// These are converted to gunk after we have converted
	// the rest of the proto declarations.
	pkg     *proto.Package
	pkgOpts []*proto.Option
	imports []*proto.Import

	// Imports that are required to ro generate a valid Gunk file.
	// Mostly these will be Gunk annotations.
	importsUsed map[string]bool
}

// Run converts proto files or folders to gunk files, saving the files in
// the same folder as the proto file.
func Run(paths []string, overwrite bool) error {
	for _, path := range paths {
		if err := run(path, overwrite); err != nil {
			return err
		}
	}
	return nil
}

func run(path string, overwrite bool) error {

	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	// Determine whether the path is a file or a directory.
	// If it is a file convert the file.
	if !fi.IsDir() {
		return convertFile(path, overwrite)
	} else if filepath.Ext(path) == ".proto" {
		// If the path is a directory and has a .proto extension then error.
		return fmt.Errorf("%s is a directory, should be a proto file.", path)
	}

	// Handle the case where it is a directory. Loop through
	// the files and if we have a .proto file attempt to
	// convert it.
	files, err := ioutil.ReadDir(path)
	for _, f := range files {
		// If the file is not a .proto file
		if f.IsDir() || filepath.Ext(f.Name()) != ".proto" {
			continue
		}
		if err := convertFile(filepath.Join(path, f.Name()), overwrite); err != nil {
			return err
		}
	}
	return nil
}

func convertFile(path string, overwrite bool) error {
	if filepath.Ext(path) != ".proto" {
		return fmt.Errorf("convert requires a .proto file")
	}
	reader, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("unable to read file %q: %v", path, err)
	}
	defer reader.Close()

	// Parse the proto file.
	parser := proto.NewParser(reader)
	d, err := parser.Parse()
	if err != nil {
		return fmt.Errorf("unable to parse proto file %q: %v", path, err)
	}

	filename := filepath.Base(path)
	fileToWrite := strings.Replace(filename, ".proto", ".gunk", 1)
	fullpath := filepath.Join(filepath.Dir(path), fileToWrite)

	if _, err := os.Stat(fullpath); !os.IsNotExist(err) && !overwrite {
		return fmt.Errorf("path already exists %q, use --overwrite", fullpath)
	}

	// Start converting the proto declarations to gunk.
	b := builder{
		filename:    filename,
		importsUsed: map[string]bool{},
	}
	for _, e := range d.Elements {
		if err := b.handleProtoType(e); err != nil {
			return fmt.Errorf("%v\n", err)
		}
	}

	// Convert the proto package and imports to gunk.
	translatedPkg, err := b.handlePackage()
	if err != nil {
		return err
	}
	translatedImports := b.handleImports()

	// Add the converted package and imports, and then
	// add all the rest of the converted types. This will
	// keep the order that things were declared.
	w := &strings.Builder{}
	w.WriteString(translatedPkg)
	w.WriteString("\n\n")
	// If we have imports, output them.
	if translatedImports != "" {
		w.WriteString(translatedImports)
		w.WriteString("\n")
	}
	for _, l := range b.translatedDeclarations {
		w.WriteString("\n")
		w.WriteString(l)
		w.WriteString("\n")
	}

	// TODO: We should run this through the Gunk generator to
	// make sure that it compiles?

	result := []byte(w.String())
	result, err = format.Source(result)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(fullpath, result, 0644); err != nil {
		return fmt.Errorf("unable to write to file %q: %v", fullpath, err)
	}

	return nil
}

// format will write output to a string builder, adding in indentation
// where required. It will write the comment first if there is one,
// and then write the rest.
//
// TODO(vishen): this currently doesn't handle inline comments (and each proto
// declaration has an 'InlineComment', as well as a 'Comment' field), only the
// leading comment is currently passed through. This function should take
// the inline comment as well. However, this will require that the this function
// checks for a \n, and add the inline comment before that?
func (b *builder) format(w *strings.Builder, indent int, comments *proto.Comment, s string, args ...interface{}) {
	if comments != nil {
		for _, c := range comments.Lines {
			for i := 0; i < indent; i++ {
				fmt.Fprintf(w, "\t")
			}
			fmt.Fprintf(w, "//%s\n", c)
		}
	}
	// If we are just writing a comment bail out.
	if s == "" {
		return
	}
	for i := 0; i < indent; i++ {
		fmt.Fprintf(w, "\t")
	}
	fmt.Fprintf(w, s, args...)
}

// formatError will return an error formatted to include the current position in
// the file.
func (b *builder) formatError(pos scanner.Position, s string, args ...interface{}) error {
	return fmt.Errorf("%s:%d:%d: %v", b.filename, pos.Line, pos.Column, fmt.Errorf(s, args...))
}

// goType will turn a proto type to a known Go type. If the
// Go type isn't recognised, it is assumed to be a custom type.
func (b *builder) goType(fieldType string) string {
	// https://github.com/golang/protobuf/blob/1918e1ff6ffd2be7bed0553df8650672c3bfe80d/protoc-gen-go/generator/generator.go#L1601
	// https://developers.google.com/protocol-buffers/docs/proto3#scalar
	switch fieldType {
	case "bool":
		return "bool"
	case "string":
		return "string"
	case "bytes":
		return "[]byte"
	case "double":
		return "float64"
	case "float":
		return "float32"
	case "int32":
		return "int"
	case "sint32", "sfixed32":
		return "int32"
	case "int64", "sint64", "sfixed64":
		return "int64"
	case "uint32", "fixed32":
		return "uint32"
	case "uint64", "fixed64":
		return "uint64"
	default:
		// This is either an unrecognised type, or a custom type.
		return fieldType
	}
}

func (b *builder) handleProtoType(typ proto.Visitee) error {
	var err error
	switch typ.(type) {
	case *proto.Syntax:
		// Do nothing with syntax
	case *proto.Package:
		// This gets translated at the very end because it is used
		// in conjuction with the option "go_package" when writting
		// a Gunk package decleration.
		b.pkg = typ.(*proto.Package)
	case *proto.Import:
		// All imports need to be grouped and written out together. This
		// happens at the end.
		b.imports = append(b.imports, typ.(*proto.Import))
	case *proto.Message:
		err = b.handleMessage(typ.(*proto.Message))
	case *proto.Enum:
		err = b.handleEnum(typ.(*proto.Enum))
	case *proto.Service:
		err = b.handleService(typ.(*proto.Service))
	case *proto.Option:
		o := typ.(*proto.Option)
		b.pkgOpts = append(b.pkgOpts, o)
	default:
		return fmt.Errorf("unhandled proto type %T", typ)
	}
	return err
}

// handleMessageField will convert a messages field to gunk.
func (b *builder) handleMessageField(w *strings.Builder, field proto.Visitee) error {
	var (
		name     string
		typ      string
		sequence int
		repeated bool
		comment  *proto.Comment
		options  []*proto.Option
	)

	switch field.(type) {
	case *proto.NormalField:
		ft := field.(*proto.NormalField)
		name = ft.Name
		typ = b.goType(ft.Type)
		sequence = ft.Sequence
		comment = ft.Comment
		repeated = ft.Repeated
		options = ft.Options
	case *proto.MapField:
		ft := field.(*proto.MapField)
		name = ft.Field.Name
		sequence = ft.Field.Sequence
		comment = ft.Comment
		keyType := b.goType(ft.KeyType)
		fieldType := b.goType(ft.Field.Type)
		typ = fmt.Sprintf("map[%s]%s", keyType, fieldType)
		options = ft.Options
	default:
		return fmt.Errorf("unhandled message field type %T", field)
	}

	if repeated {
		typ = "[]" + typ
	}

	for _, o := range options {
		fmt.Fprintln(os.Stderr, b.formatError(o.Position, "unhandled field option %q", o.Name))
	}

	// TODO(vishen): Is this correct to explicitly camelcase the variable name and
	// snakecase the json name???
	// If we do, gunk should probably have an option to set the variable name
	// in the proto to something else? That way we can use best practises for
	// each language???
	b.format(w, 1, comment, "%s %s", snaker.ForceCamelIdentifier(name), typ)
	b.format(w, 0, nil, " `pb:\"%d\" json:\"%s\"`\n", sequence, snaker.CamelToSnake(name))
	return nil
}

// handleMessage will convert a proto message to Gunk.
func (b *builder) handleMessage(m *proto.Message) error {
	w := &strings.Builder{}
	b.format(w, 0, m.Comment, "type %s struct {\n", m.Name)
	for _, e := range m.Elements {
		switch e.(type) {
		case *proto.NormalField:
			f := e.(*proto.NormalField)
			if err := b.handleMessageField(w, f); err != nil {
				return b.formatError(f.Position, "error with message field: %v", err)
			}
		case *proto.Enum:
			// Handle the nested enum. This will create a new
			// top level enum as Gunk doesn't currently support
			// nested data structures.
			b.handleEnum(e.(*proto.Enum))
		case *proto.Comment:
			b.format(w, 1, e.(*proto.Comment), "")
		case *proto.MapField:
			mf := e.(*proto.MapField)
			if err := b.handleMessageField(w, mf); err != nil {
				return b.formatError(mf.Position, "error with message field: %v", err)
			}
		case *proto.Option:
			o := e.(*proto.Option)
			fmt.Fprintln(os.Stderr, b.formatError(o.Position, "unhandled message option %q", o.Name))
		default:
			return b.formatError(m.Position, "unexpected type %T in message", e)
		}
	}
	b.format(w, 0, nil, "}")
	b.translatedDeclarations = append(b.translatedDeclarations, w.String())
	return nil
}

// handleEnum will output a proto enum as a Go const. It will output
// the enum using Go iota if each enum value is incrementing by 1
// (starting from 0). Otherwise we output each enum value as a straight
// conversion.
func (b *builder) handleEnum(e *proto.Enum) error {
	w := &strings.Builder{}
	b.format(w, 0, e.Comment, "type %s int\n", e.Name)
	b.format(w, 0, nil, "\nconst (\n")

	// Check to see if we can output the enum using an iota. This is
	// currently only possible if every enum value is an increment of 1
	// from the previous enum value.
	outputIota := true
	for i, c := range e.Elements {
		switch c.(type) {
		case *proto.EnumField:
			ef := c.(*proto.EnumField)
			if i != ef.Integer {
				outputIota = false
			}
		case *proto.Option:
			o := c.(*proto.Option)
			fmt.Fprintln(os.Stderr, b.formatError(o.Position, "unhandled enum option %q", o.Name))
		default:
			return b.formatError(e.Position, "unexpected type %T in enum, expected enum field", c)
		}
	}

	// Now we can output the enum as a const.
	for i, c := range e.Elements {
		ef, ok := c.(*proto.EnumField)
		if !ok {
			// We should have caught any errors when checking if we can output as
			// iota (above).
			// TODO(vishen): handle enum option
			continue
		}

		for _, e := range ef.Elements {
			if o, ok := e.(*proto.Option); ok && o != nil {
				fmt.Fprintln(os.Stderr, b.formatError(o.Position, "unhandled enumvalue option %q", o.Name))
			}

		}

		// If we can't output as an iota.
		if !outputIota {
			b.format(w, 1, ef.Comment, "%s %s = %d\n", ef.Name, e.Name, ef.Integer)
			continue
		}

		// If we can output as an iota, output the first element as the
		// iota and output the rest as just the enum field name.
		if i == 0 {
			b.format(w, 1, ef.Comment, "%s %s = iota\n", ef.Name, e.Name)
		} else {
			b.format(w, 1, ef.Comment, "%s\n", ef.Name)
		}
	}
	b.format(w, 0, nil, ")")
	b.translatedDeclarations = append(b.translatedDeclarations, w.String())
	return nil
}

func (b *builder) handleService(s *proto.Service) error {
	w := &strings.Builder{}
	b.format(w, 0, s.Comment, "type %s interface {\n", s.Name)
	for i, e := range s.Elements {
		var r *proto.RPC
		switch e.(type) {
		case *proto.RPC:
			r = e.(*proto.RPC)
		case *proto.Option:
			o := e.(*proto.Option)
			fmt.Fprintln(os.Stderr, b.formatError(o.Position, "unhandled service option %q", o.Name))
			continue
		default:
			return b.formatError(s.Position, "unexpected type %T in service, expected rpc", e)
		}
		// Add a newline between each new function declaration on the interface, only
		// if there is comments or gunk annotations seperating them. We can assume that
		// anything in `Elements` will be a gunk annotation, otherwise an error is
		// returned below.
		if i > 0 && (r.Comment != nil || len(r.Elements) > 0) {
			b.format(w, 0, nil, "\n")
		}
		// The comment to translate. It is possible that when we write
		// the gunk annotations out we also write the comment above the
		// gunk annotation. If that happens we set the comment to nil
		// so it doesn't get written out when translating the field.
		comment := r.Comment
		for _, o := range r.Elements {
			opt, ok := o.(*proto.Option)
			if !ok {
				return b.formatError(r.Position, "unexpected type %T in service rpc, expected option", o)
			}
			switch n := opt.Name; n {
			case "(google.api.http)":
				var err error
				method := ""
				url := ""
				body := ""
				literal := opt.Constant
				if len(literal.OrderedMap) == 0 {
					return b.formatError(opt.Position, "expected option to be a map")
				}
				for _, l := range literal.OrderedMap {
					switch n := l.Name; n {
					case "body":
						body, err = b.handleLiteralString(*l.Literal)
						if err != nil {
							return b.formatError(opt.Position, "option for body should be a string")
						}
					default:
						method = n
						url, err = b.handleLiteralString(*l.Literal)
						if err != nil {
							return b.formatError(opt.Position, "option for %q should be a string (url)", method)
						}
					}
				}

				// Check if we received a valid google http annotation. If
				// so we will convert it to gunk http match.
				if method != "" && url != "" {
					if comment != nil {
						b.format(w, 1, comment, "//\n")
						comment = nil
					}
					b.format(w, 1, nil, "// +gunk http.Match{\n")
					b.format(w, 1, nil, "//     Method: %q,\n", strings.ToUpper(method))
					b.format(w, 1, nil, "//     Path: %q,\n", url)
					if body != "" {
						b.format(w, 1, nil, "//     Body: %q,\n", body)
					}
					b.format(w, 1, nil, "// }\n")
					b.importsUsed["github.com/gunk/opt/http"] = true
				}
			default:
				fmt.Fprintln(os.Stderr, b.formatError(opt.Position, "unhandled method option %q", n))
			}
		}
		// If the request type is the known empty parameter we can convert
		// this to gunk as an empty function parameter.
		requestType := r.RequestType
		returnsType := r.ReturnsType
		if requestType == "google.protobuf.Empty" {
			requestType = ""
		}
		if returnsType == "google.protobuf.Empty" {
			returnsType = ""
		}
		b.format(w, 1, comment, "%s(%s) %s\n", r.Name, requestType, returnsType)
	}
	b.format(w, 0, nil, "}")
	b.translatedDeclarations = append(b.translatedDeclarations, w.String())
	return nil
}

func (b *builder) genAnnotation(name, value string) string {
	return fmt.Sprintf("%s(%s)", name, value)
}

func (b *builder) genAnnotationString(name, value string) string {
	return fmt.Sprintf("%s(%q)", name, value)
}

func (b *builder) handlePackage() (string, error) {
	w := &strings.Builder{}
	var opt *proto.Option
	gunkAnnotations := []string{}
	for _, o := range b.pkgOpts {
		val := o.Constant.Source
		var impt string
		var value string
		switch n := o.Name; n {
		case "go_package":
			opt = o
			continue
		case "deprecated":
			impt = "github.com/gunk/opt/file"
			value = b.genAnnotation("Deprecated", val)
		case "optimize_for":
			impt = "github.com/gunk/opt/file"
			value = b.genAnnotation("OptimizeFor", val)
		case "java_package":
			impt = "github.com/gunk/opt/file/java"
			value = b.genAnnotationString("Package", val)
		case "java_outer_classname":
			impt = "github.com/gunk/opt/file/java"
			value = b.genAnnotationString("OuterClassname", val)
		case "java_multiple_files":
			impt = "github.com/gunk/opt/file/java"
			value = b.genAnnotation("MultipleFiles", val)
		case "java_string_check_utf8":
			impt = "github.com/gunk/opt/file/java"
			value = b.genAnnotation("StringCheckUtf8", val)
		case "java_generic_services":
			impt = "github.com/gunk/opt/file/java"
			value = b.genAnnotation("GenericServices", val)
		case "swift_prefix":
			impt = "github.com/gunk/opt/file/swift"
		case "csharp_namespace":
			impt = "github.com/gunk/opt/file/csharp"
			value = b.genAnnotationString("Namespace", val)
		case "objc_class_prefix":
			impt = "github.com/gunk/opt/file/objc"
			value = b.genAnnotationString("ClassPrefix", val)
		case "php_generic_services":
			impt = "github.com/gunk/opt/file/php"
			value = b.genAnnotation("GenericServices", val)
		case "cc_generic_services":
			impt = "github.com/gunk/opt/file/cc"
			value = b.genAnnotation("GenericServices", val)
		case "cc_enable_arenas":
			impt = "github.com/gunk/opt/file/cc"
			value = b.genAnnotation("EnableArenas", val)
		default:
			return "", b.formatError(o.Position, "%q is an unhandled proto file option", n)
		}

		b.importsUsed[impt] = true
		pkg := filepath.Base(impt)
		gunkAnnotations = append(gunkAnnotations, fmt.Sprintf("%s.%s", pkg, value))
	}

	// Output the gunk annotations above the package comment. This
	// should be first lines in the file.
	for _, ga := range gunkAnnotations {
		b.format(w, 0, nil, fmt.Sprintf("// +gunk %s\n", ga))
	}

	p := b.pkg
	b.format(w, 0, p.Comment, "")
	if opt != nil {
		b.format(w, 0, opt.Comment, "")
	}
	b.format(w, 0, nil, "package %s", p.Name)
	if opt != nil && opt.Constant.Source != "" {
		b.format(w, 0, nil, " // proto %s", opt.Constant.Source)
	}

	return w.String(), nil
}

func (b *builder) handleImports() string {
	if len(b.importsUsed) == 0 && len(b.imports) == 0 {
		return ""
	}

	w := &strings.Builder{}
	b.format(w, 0, nil, "import (")

	// Imports that have been used during convert
	for i := range b.importsUsed {
		b.format(w, 0, nil, "\n")
		b.format(w, 1, nil, fmt.Sprintf("%q", i))
	}

	// Add any proto imports as comments.
	for _, i := range b.imports {
		b.format(w, 0, nil, "\n")
		b.format(w, 1, nil, "// %q", i.Filename)
	}
	b.format(w, 0, nil, "\n)")
	return w.String()
}

func (b *builder) handleLiteralString(lit proto.Literal) (string, error) {
	if !lit.IsString {
		return "", fmt.Errorf("literal was expected to be a string")
	}
	return lit.Source, nil
}
