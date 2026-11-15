// Comando diccionario: recorre el código Go del proyecto y genera un HTML
// autocontenido con el árbol de archivos, sus funciones, tipos y el grafo de
// llamadas. Pensado como material de estudio y mapa del proyecto.
//
//	go run ./cmd/diccionario
//	go run ./cmd/diccionario -dir . -out docs/diccionario/index.html
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed plantilla.html
var plantilla string

type Datos struct {
	Generado string             `json:"generado"`
	Raiz     string             `json:"raiz"`
	Modulo   string             `json:"modulo"`
	Stats    Stats              `json:"stats"`
	Paquetes []Paquete          `json:"paquetes"`
	Archivos []Archivo          `json:"archivos"`
	Simbolos map[string]Simbolo `json:"simbolos"`
}

type Stats struct {
	Archivos  int `json:"archivos"`
	Lineas    int `json:"lineas"`
	Funciones int `json:"funciones"`
	Metodos   int `json:"metodos"`
	Tipos     int `json:"tipos"`
	Paquetes  int `json:"paquetes"`
}

type Paquete struct {
	ID       string   `json:"id"`
	Nombre   string   `json:"nombre"`
	Ruta     string   `json:"ruta"`
	Doc      string   `json:"doc"`
	Archivos []string `json:"archivos"`
	Lineas   int      `json:"lineas"`
}

type Archivo struct {
	Ruta       string      `json:"ruta"`
	Paquete    string      `json:"paquete"`
	Lineas     int         `json:"lineas"`
	Doc        string      `json:"doc"`
	Imports    []Import    `json:"imports"`
	DependeDe  []string    `json:"depende_de"`
	UsadoPor   []string    `json:"usado_por"`
	Funciones  []Funcion   `json:"funciones"`
	Tipos      []Tipo      `json:"tipos"`
	Constantes []Constante `json:"constantes"`
	Variables  []Variable  `json:"variables"`
}

type Import struct {
	Alias   string `json:"alias"`
	Ruta    string `json:"ruta"`
	Interno bool   `json:"interno"`
}

type Funcion struct {
	ID          string   `json:"id"`
	Nombre      string   `json:"nombre"`
	Receptor    string   `json:"receptor,omitempty"`
	Firma       string   `json:"firma"`
	Doc         string   `json:"doc"`
	Explicacion string   `json:"explicacion"`
	Notas       []string `json:"notas"`
	Llamadas    []string `json:"llamadas"`
	LlamadoPor  []string `json:"llamado_por"`
	Rasgos      []string `json:"rasgos"`
	Linea       int      `json:"linea"`
	Exportada   bool     `json:"exportada"`
}

type Tipo struct {
	ID        string   `json:"id"`
	Nombre    string   `json:"nombre"`
	Clase     string   `json:"clase"`
	Doc       string   `json:"doc"`
	Campos    []Campo  `json:"campos"`
	Metodos   []string `json:"metodos"`
	Linea     int      `json:"linea"`
	Exportado bool     `json:"exportado"`
}

type Campo struct {
	Nombre   string `json:"nombre"`
	Tipo     string `json:"tipo"`
	Doc      string `json:"doc"`
	Etiqueta string `json:"etiqueta,omitempty"`
}

type Constante struct {
	Nombre string `json:"nombre"`
	Tipo   string `json:"tipo"`
	Valor  string `json:"valor"`
	Doc    string `json:"doc"`
}

type Variable struct {
	Nombre string `json:"nombre"`
	Tipo   string `json:"tipo"`
	Doc    string `json:"doc"`
}

type Simbolo struct {
	ID      string `json:"id"`
	Nombre  string `json:"nombre"`
	Paquete string `json:"paquete"`
	Archivo string `json:"archivo"`
	Tipo    string `json:"tipo"`
	Firma   string `json:"firma,omitempty"`
	Linea   int    `json:"linea"`
}

type archivoAST struct {
	ruta string
	file *ast.File
}

type generador struct {
	dir         string
	modulo      string
	fset        *token.FileSet
	archivos    []archivoAST
	paqRuta     map[string]string // ruta de import -> paquete
	simbolos    map[string]Simbolo
	porPaq      map[string]map[string]string // paquete -> nombre -> id
	metodos     map[string][]string          // nombre de método -> ids
	metodosTipo map[string][]string          // pkg.Tipo -> ids de sus métodos
	lineas      map[string]int
}

func main() {
	dir := flag.String("dir", ".", "raíz del proyecto")
	out := flag.String("out", "docs/diccionario/index.html", "HTML de salida")
	jsonOut := flag.String("json", "docs/diccionario/diccionario.json", "JSON de salida (vacío para omitir)")
	web := flag.String("web", "internal/server/web/diccionario.html", "copia embebida por el servidor (vacío para omitir)")
	flag.Parse()

	d, err := generar(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diccionario:", err)
		os.Exit(1)
	}
	datos, err := json.Marshal(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diccionario:", err)
		os.Exit(1)
	}
	// Evita que un "</script>" dentro de un comentario cierre el bloque de datos.
	seguro := strings.ReplaceAll(string(datos), "</", "<\\/")
	html := strings.ReplaceAll(plantilla, "__DATOS__", seguro)
	html = strings.ReplaceAll(html, "__GENERADO__", d.Generado)

	if err := escribir(*out, html); err != nil {
		fmt.Fprintln(os.Stderr, "diccionario:", err)
		os.Exit(1)
	}
	if *jsonOut != "" {
		if err := escribir(*jsonOut, string(datos)); err != nil {
			fmt.Fprintln(os.Stderr, "diccionario:", err)
			os.Exit(1)
		}
	}
	if *web != "" {
		if err := escribir(*web, html); err != nil {
			fmt.Fprintln(os.Stderr, "diccionario:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("Diccionario generado: %d archivos, %d funciones, %d tipos, %d líneas\n",
		d.Stats.Archivos, d.Stats.Funciones, d.Stats.Tipos, d.Stats.Lineas)
	fmt.Printf("  %s\n", *out)
	if *web != "" {
		fmt.Printf("  %s\n", *web)
	}
}

func escribir(ruta, contenido string) error {
	if err := os.MkdirAll(filepath.Dir(ruta), 0o755); err != nil {
		return err
	}
	return os.WriteFile(ruta, []byte(contenido), 0o644)
}

func generar(dir string) (*Datos, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	modulo, err := leerModulo(abs)
	if err != nil {
		return nil, err
	}

	g := &generador{
		dir:         abs,
		modulo:      modulo,
		fset:        token.NewFileSet(),
		paqRuta:     map[string]string{},
		simbolos:    map[string]Simbolo{},
		porPaq:      map[string]map[string]string{},
		metodos:     map[string][]string{},
		metodosTipo: map[string][]string{},
		lineas:      map[string]int{},
	}

	rutas, err := archivosGo(abs)
	if err != nil {
		return nil, err
	}

	// Primera pasada: parsear, registrar paquetes y símbolos.
	for _, ruta := range rutas {
		file, err := parser.ParseFile(g.fset, ruta, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ruta, err)
		}
		rel := relativa(abs, ruta)
		g.archivos = append(g.archivos, archivoAST{ruta: rel, file: file})
		g.lineas[rel] = g.fset.Position(file.End()).Line
		pkg := file.Name.Name
		g.paqRuta[g.modulo+"/"+filepath.ToSlash(filepath.Dir(rel))] = pkg
		if g.porPaq[pkg] == nil {
			g.porPaq[pkg] = map[string]string{}
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil {
					continue // los métodos se indexan después de conocer el tipo
				}
				id := pkg + "." + d.Name.Name
				g.porPaq[pkg][d.Name.Name] = id
				g.simbolos[id] = Simbolo{ID: id, Nombre: d.Name.Name, Paquete: pkg,
					Archivo: rel, Tipo: "función", Firma: g.firma(d), Linea: g.linea(d)}
			case *ast.GenDecl:
				g.registrarTipos(pkg, rel, d)
			}
		}
	}

	// Segunda pasada: métodos indexados por tipo y por nombre.
	for _, a := range g.archivos {
		pkg := a.file.Name.Name
		for _, decl := range a.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil {
				continue
			}
			recv := nombreReceptor(fd.Recv.List[0].Type)
			if recv == "" {
				continue
			}
			id := pkg + "." + recv + "." + fd.Name.Name
			g.metodos[fd.Name.Name] = append(g.metodos[fd.Name.Name], id)
			tipoID := pkg + "." + recv
			g.metodosTipo[tipoID] = append(g.metodosTipo[tipoID], id)
			g.simbolos[id] = Simbolo{ID: id, Nombre: fd.Name.Name, Paquete: pkg,
				Archivo: a.ruta, Tipo: "método", Firma: g.firma(fd), Linea: g.linea(fd)}
		}
	}

	out := &Datos{
		Generado: timeNow(),
		Raiz:     abs,
		Modulo:   modulo,
		Simbolos: g.simbolos,
	}

	paquetes := map[string]*Paquete{}
	var totalFunciones, totalMetodos, totalTipos int

	for _, a := range g.archivos {
		pkg := a.file.Name.Name
		ar := Archivo{
			Ruta:    a.ruta,
			Paquete: pkg,
			Lineas:  g.lineas[a.ruta],
			Doc:     comentario(a.file.Doc),
		}
		alias := map[string]string{} // alias local -> paquete
		vistos := map[string]bool{}
		for _, imp := range a.file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			name := ""
			if imp.Name != nil {
				name = imp.Name.Name
			}
			interno := strings.HasPrefix(path, modulo+"/")
			if interno {
				if p, ok := g.paqRuta[path]; ok {
					if name == "" {
						name = p
					}
					if !vistos[path] {
						ar.DependeDe = append(ar.DependeDe, path)
						vistos[path] = true
					}
				}
			}
			if name == "" {
				parts := strings.Split(path, "/")
				name = parts[len(parts)-1]
			}
			ar.Imports = append(ar.Imports, Import{Alias: name, Ruta: path, Interno: interno})
			alias[name] = path
		}

		for _, decl := range a.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				fn := g.analizarFuncion(pkg, a.ruta, d, alias, a.file.Comments)
				ar.Funciones = append(ar.Funciones, *fn)
				if fn.Receptor == "" {
					totalFunciones++
				} else {
					totalMetodos++
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						t := g.analizarTipo(pkg, a.ruta, spec.(*ast.TypeSpec), d.Doc)
						ar.Tipos = append(ar.Tipos, *t)
						totalTipos++
					}
				case token.CONST:
					for _, spec := range d.Specs {
						ar.Constantes = append(ar.Constantes, g.analizarConstantes(spec.(*ast.ValueSpec))...)
					}
				case token.VAR:
					for _, spec := range d.Specs {
						ar.Variables = append(ar.Variables, g.analizarVariables(spec.(*ast.ValueSpec))...)
					}
				}
			}
		}

		sort.Slice(ar.Funciones, func(i, j int) bool { return ar.Funciones[i].Linea < ar.Funciones[j].Linea })
		sort.Slice(ar.Tipos, func(i, j int) bool { return ar.Tipos[i].Linea < ar.Tipos[j].Linea })

		clavePaq := g.modulo + "/" + filepath.ToSlash(filepath.Dir(a.ruta))
		if p := paquetes[clavePaq]; p != nil {
			p.Archivos = append(p.Archivos, ar.Ruta)
			p.Lineas += ar.Lineas
			if p.Doc == "" && ar.Doc != "" {
				p.Doc = ar.Doc
			}
		} else {
			paquetes[clavePaq] = &Paquete{ID: clavePaq, Nombre: pkg, Ruta: clavePaq,
				Doc: ar.Doc, Archivos: []string{ar.Ruta}, Lineas: ar.Lineas}
		}
		out.Archivos = append(out.Archivos, ar)
	}

	// "usado por": paquetes que importan cada paquete.
	claveDe := func(ruta string) string {
		return g.modulo + "/" + filepath.ToSlash(filepath.Dir(ruta))
	}
	usado := map[string]map[string]bool{}
	for _, ar := range out.Archivos {
		for _, dep := range ar.DependeDe {
			if usado[dep] == nil {
				usado[dep] = map[string]bool{}
			}
			usado[dep][claveDe(ar.Ruta)] = true
		}
	}
	for i := range out.Archivos {
		actual := claveDe(out.Archivos[i].Ruta)
		set := usado[actual]
		for p := range set {
			if p != actual {
				out.Archivos[i].UsadoPor = append(out.Archivos[i].UsadoPor, p)
			}
		}
		sort.Strings(out.Archivos[i].UsadoPor)
	}

	// Invertir llamadas y completar explicaciones.
	inversas := map[string][]string{}
	for _, ar := range out.Archivos {
		for _, fn := range ar.Funciones {
			for _, callee := range fn.Llamadas {
				inversas[callee] = append(inversas[callee], fn.ID)
			}
		}
	}
	for i := range out.Archivos {
		for j := range out.Archivos[i].Funciones {
			fn := &out.Archivos[i].Funciones[j]
			fn.LlamadoPor = dedupe(inversas[fn.ID])
			sort.Strings(fn.LlamadoPor)
			fn.Explicacion = explicar(fn, &out.Archivos[i], g)
		}
	}

	out.Paquetes = make([]Paquete, 0, len(paquetes))
	for _, p := range paquetes {
		sort.Strings(p.Archivos)
		out.Paquetes = append(out.Paquetes, *p)
	}
	sort.Slice(out.Paquetes, func(i, j int) bool { return out.Paquetes[i].ID < out.Paquetes[j].ID })
	sort.Slice(out.Archivos, func(i, j int) bool { return out.Archivos[i].Ruta < out.Archivos[j].Ruta })

	out.Stats = Stats{
		Archivos: len(out.Archivos), Lineas: sumLineas(out.Archivos),
		Funciones: totalFunciones, Metodos: totalMetodos, Tipos: totalTipos,
		Paquetes: len(out.Paquetes),
	}
	normalizar(out)
	return out, nil
}

// normalizar convierte los slices nil en listas vacías para que el JSON tenga
// siempre el mismo tipo (el JavaScript usa .map y .length sin comprobaciones).
func normalizar(d *Datos) {
	if d.Paquetes == nil {
		d.Paquetes = []Paquete{}
	}
	for i := range d.Paquetes {
		if d.Paquetes[i].Archivos == nil {
			d.Paquetes[i].Archivos = []string{}
		}
	}
	for i := range d.Archivos {
		a := &d.Archivos[i]
		if a.Imports == nil {
			a.Imports = []Import{}
		}
		if a.DependeDe == nil {
			a.DependeDe = []string{}
		}
		if a.UsadoPor == nil {
			a.UsadoPor = []string{}
		}
		if a.Funciones == nil {
			a.Funciones = []Funcion{}
		}
		if a.Tipos == nil {
			a.Tipos = []Tipo{}
		}
		if a.Constantes == nil {
			a.Constantes = []Constante{}
		}
		if a.Variables == nil {
			a.Variables = []Variable{}
		}
		for j := range a.Funciones {
			f := &a.Funciones[j]
			if f.Llamadas == nil {
				f.Llamadas = []string{}
			}
			if f.LlamadoPor == nil {
				f.LlamadoPor = []string{}
			}
			if f.Notas == nil {
				f.Notas = []string{}
			}
			if f.Rasgos == nil {
				f.Rasgos = []string{}
			}
		}
		for j := range a.Tipos {
			t := &a.Tipos[j]
			if t.Campos == nil {
				t.Campos = []Campo{}
			}
			if t.Metodos == nil {
				t.Metodos = []string{}
			}
		}
	}
}

func (g *generador) registrarTipos(pkg, rel string, d *ast.GenDecl) {
	if d.Tok != token.TYPE {
		return
	}
	for _, spec := range d.Specs {
		ts := spec.(*ast.TypeSpec)
		clase := "alias"
		switch ts.Type.(type) {
		case *ast.StructType:
			clase = "struct"
		case *ast.InterfaceType:
			clase = "interface"
		}
		id := pkg + "." + ts.Name.Name
		g.porPaq[pkg][ts.Name.Name] = id
		g.simbolos[id] = Simbolo{ID: id, Nombre: ts.Name.Name, Paquete: pkg,
			Archivo: rel, Tipo: clase, Linea: g.fset.Position(ts.Pos()).Line}
	}
}

func (g *generador) analizarFuncion(pkg, rel string, fd *ast.FuncDecl, alias map[string]string, comments []*ast.CommentGroup) *Funcion {
	recv := ""
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		recv = nombreReceptor(fd.Recv.List[0].Type)
	}
	id := pkg + "." + fd.Name.Name
	if recv != "" {
		id = pkg + "." + recv + "." + fd.Name.Name
	}
	fn := &Funcion{
		ID: id, Nombre: fd.Name.Name, Receptor: recv,
		Firma: g.firma(fd), Doc: comentario(fd.Doc),
		Linea: g.linea(fd), Exportada: ast.IsExported(fd.Name.Name),
	}
	if fd.Body == nil {
		return fn
	}

	llamadas := map[string]bool{}
	rasgos := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			switch f := x.Fun.(type) {
			case *ast.Ident:
				if id := g.resolverIdent(pkg, f.Name); id != "" {
					llamadas[id] = true
				}
			case *ast.SelectorExpr:
				if id := g.resolverSelector(pkg, alias, f); id != "" {
					llamadas[id] = true
				}
			}
		case *ast.GoStmt:
			rasgos["goroutine"] = true
		case *ast.DeferStmt:
			rasgos["defer"] = true
		case *ast.ChanType:
			rasgos["canales"] = true
		case *ast.SelectStmt:
			rasgos["select"] = true
		case *ast.TypeAssertExpr:
			rasgos["type-assertion"] = true
		}
		return true
	})

	for id := range llamadas {
		if id == fn.ID {
			delete(llamadas, id)
		}
	}
	fn.Llamadas = claves(llamadas)

	if fd.Type.Params != nil {
		for _, p := range fd.Type.Params.List {
			t := g.tipo(p.Type)
			if strings.Contains(t, "context.Context") {
				rasgos["contexto"] = true
			}
		}
	}
	if fd.Type.Results != nil {
		for _, r := range fd.Type.Results.List {
			if g.tipo(r.Type) == "error" {
				rasgos["error"] = true
			}
		}
	}
	if strings.Contains(fn.Firma, "sync.") || contieneLlamada(fn.Llamadas, "Lock") {
		rasgos["mutex"] = true
	}
	if len(llamadas) == 0 {
		rasgos["hoja"] = true
	}
	fn.Rasgos = claves(rasgos)
	sort.Strings(fn.Rasgos)
	fn.Notas = g.notas(fd, fn.Doc, comments)
	return fn
}

func (g *generador) resolverIdent(pkg, nombre string) string {
	if esBuiltin(nombre) {
		return ""
	}
	if m := g.porPaq[pkg]; m != nil {
		if id, ok := m[nombre]; ok {
			return id
		}
	}
	return ""
}

func (g *generador) resolverSelector(pkg string, alias map[string]string, sel *ast.SelectorExpr) string {
	// Llamada a otro paquete: config.LoadServer(...)
	if base, ok := sel.X.(*ast.Ident); ok {
		if path, ok := alias[base.Name]; ok {
			if target, ok := g.paqRuta[path]; ok {
				if m := g.porPaq[target]; m != nil {
					if id, ok := m[sel.Sel.Name]; ok {
						return id
					}
				}
				return ""
			}
		}
	}
	// Llamada a un método a través de una variable: s.store.Record(...).
	// Sin análisis de tipos, sólo se enlaza cuando el nombre es único.
	if ids := g.metodos[sel.Sel.Name]; len(ids) == 1 {
		return ids[0]
	}
	return ""
}

func (g *generador) analizarTipo(pkg, rel string, ts *ast.TypeSpec, doc *ast.CommentGroup) *Tipo {
	id := pkg + "." + ts.Name.Name
	t := &Tipo{ID: id, Nombre: ts.Name.Name, Doc: comentario(ts.Doc),
		Linea: g.fset.Position(ts.Pos()).Line, Exportado: ast.IsExported(ts.Name.Name)}
	if t.Doc == "" {
		t.Doc = comentario(doc)
	}
	switch x := ts.Type.(type) {
	case *ast.StructType:
		t.Clase = "struct"
		if x.Fields != nil {
			for _, f := range x.Fields.List {
				campo := Campo{Tipo: g.tipo(f.Type), Doc: comentario(f.Doc)}
				if f.Tag != nil {
					campo.Etiqueta = strings.Trim(f.Tag.Value, "`")
				}
				if len(f.Names) == 0 {
					campo.Nombre = "(incrustado)"
					t.Campos = append(t.Campos, campo)
					continue
				}
				for _, n := range f.Names {
					c := campo
					c.Nombre = n.Name
					t.Campos = append(t.Campos, c)
				}
			}
		}
	case *ast.InterfaceType:
		t.Clase = "interface"
		if x.Methods != nil {
			for _, m := range x.Methods.List {
				if len(m.Names) == 0 {
					t.Campos = append(t.Campos, Campo{Nombre: "(incrustado)", Tipo: g.tipo(m.Type), Doc: comentario(m.Doc)})
					continue
				}
				for _, n := range m.Names {
					t.Campos = append(t.Campos, Campo{Nombre: n.Name, Tipo: g.tipo(m.Type), Doc: comentario(m.Doc)})
				}
			}
		}
	default:
		t.Clase = "alias"
		t.Campos = []Campo{{Nombre: "tipo", Tipo: g.tipo(ts.Type), Doc: comentario(ts.Doc)}}
	}
	if g.porPaq[pkg] != nil {
		g.porPaq[pkg][ts.Name.Name] = id
	}
	t.Metodos = g.metodosTipo[id]
	sort.Strings(t.Metodos)
	return t
}

func (g *generador) analizarConstantes(spec *ast.ValueSpec) []Constante {
	var out []Constante
	for i, n := range spec.Names {
		c := Constante{Nombre: n.Name, Doc: comentario(spec.Doc)}
		if spec.Type != nil {
			c.Tipo = g.tipo(spec.Type)
		}
		if i < len(spec.Values) {
			c.Valor = g.tipo(spec.Values[i])
		}
		out = append(out, c)
	}
	return out
}

func (g *generador) analizarVariables(spec *ast.ValueSpec) []Variable {
	var out []Variable
	for _, n := range spec.Names {
		v := Variable{Nombre: n.Name, Doc: comentario(spec.Doc)}
		if spec.Type != nil {
			v.Tipo = g.tipo(spec.Type)
		} else if len(spec.Values) > 0 {
			v.Tipo = g.tipo(spec.Values[0])
		}
		out = append(out, v)
	}
	return out
}

func (g *generador) firma(fd *ast.FuncDecl) string {
	var b bytes.Buffer
	b.WriteString("func ")
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		b.WriteString("(")
		if n := fd.Recv.List[0].Names; len(n) > 0 {
			b.WriteString(n[0].Name)
			b.WriteString(" ")
		}
		b.WriteString(g.tipo(fd.Recv.List[0].Type))
		b.WriteString(") ")
	}
	b.WriteString(fd.Name.Name)
	// printer imprime el FuncType con el prefijo "func"; aquí ya lo escribimos.
	b.WriteString(strings.TrimPrefix(g.tipo(fd.Type), "func"))
	return b.String()
}

func (g *generador) tipo(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	var b bytes.Buffer
	_ = printer.Fprint(&b, g.fset, expr)
	return b.String()
}

func (g *generador) linea(n ast.Node) int { return g.fset.Position(n.Pos()).Line }

// notas recoge los comentarios escritos dentro del cuerpo de la función:
// suelen explicar los pasos concretos del algoritmo.
func (g *generador) notas(fd *ast.FuncDecl, doc string, comments []*ast.CommentGroup) []string {
	if fd.Body == nil {
		return nil
	}
	var out []string
	vistos := map[string]bool{}
	for _, cg := range comments {
		if cg.Pos() < fd.Body.Pos() || cg.End() > fd.Body.End() {
			continue
		}
		text := comentario(cg)
		if text == "" || text == doc || vistos[text] {
			continue
		}
		vistos[text] = true
		out = append(out, text)
	}
	return out
}

func timeNow() string { return time.Now().Format("2006-01-02 15:04") }

func explicar(fn *Funcion, ar *Archivo, g *generador) string {
	var b strings.Builder
	if fn.Doc != "" {
		b.WriteString(fn.Doc)
		b.WriteString("\n\n")
	}
	que := "Función"
	if fn.Receptor != "" {
		que = "Método del tipo " + fn.Receptor
	} else if fn.Exportada {
		que = "Función exportada"
	}
	verbo := "definida"
	if fn.Receptor != "" {
		verbo = "definido"
	}
	fmt.Fprintf(&b, "%s del paquete %s, %s en %s (línea %d).\n", que, ar.Paquete, verbo, ar.Ruta, fn.Linea)
	fmt.Fprintf(&b, "Firma: %s\n", fn.Firma)

	if fn.Receptor != "" {
		fmt.Fprintf(&b, "Trabaja sobre una instancia de %s.\n", strings.TrimPrefix(fn.Receptor, "*"))
	}
	if len(fn.Llamadas) > 0 {
		nombres := make([]string, 0, len(fn.Llamadas))
		for _, id := range fn.Llamadas {
			nombres = append(nombres, corto(id))
		}
		fmt.Fprintf(&b, "Llama a: %s.\n", strings.Join(nombres, ", "))
	}
	if len(fn.LlamadoPor) > 0 {
		nombres := make([]string, 0, len(fn.LlamadoPor))
		for _, id := range fn.LlamadoPor {
			nombres = append(nombres, corto(id))
		}
		fmt.Fprintf(&b, "Es llamada desde: %s.\n", strings.Join(nombres, ", "))
	}
	if len(fn.Rasgos) > 0 {
		fmt.Fprintf(&b, "Técnicas que usa: %s.\n", strings.Join(descripcionRasgos(fn.Rasgos), ", "))
	}
	return strings.TrimSpace(b.String())
}

func descripcionRasgos(rasgos []string) []string {
	m := map[string]string{
		"goroutine":      "lanza trabajo concurrente con go",
		"defer":          "aplaza limpieza con defer",
		"canales":        "usa canales para comunicar goroutines",
		"select":         "espera con select",
		"contexto":       "respeta context.Context (cancelación y plazos)",
		"error":          "devuelve error",
		"mutex":          "protege estado compartido con mutex",
		"type-assertion": "comprueba tipos en tiempo de ejecución",
		"hoja":           "no llama a otras funciones del proyecto",
	}
	out := make([]string, 0, len(rasgos))
	for _, r := range rasgos {
		if d, ok := m[r]; ok {
			out = append(out, d)
		} else {
			out = append(out, r)
		}
	}
	return out
}

func corto(id string) string {
	parts := strings.Split(id, ".")
	if len(parts) >= 3 {
		return parts[len(parts)-2] + "." + parts[len(parts)-1]
	}
	if len(parts) == 2 {
		return parts[1]
	}
	return id
}

func contieneLlamada(ids []string, sufijo string) bool {
	for _, id := range ids {
		if strings.HasSuffix(id, "."+sufijo) {
			return true
		}
	}
	return false
}

func esBuiltin(n string) bool {
	switch n {
	case "append", "cap", "clear", "close", "complex", "copy", "delete", "imag",
		"len", "make", "max", "min", "new", "panic", "print", "println", "real", "recover":
		return true
	}
	return false
}

func nombreReceptor(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.StarExpr:
		return nombreReceptor(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return nombreReceptor(x.X)
	case *ast.IndexListExpr:
		return nombreReceptor(x.X)
	}
	return ""
}

func comentario(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimSpace(cg.Text())
}

func leerModulo(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("no se pudo leer go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("go.mod sin línea module")
}

func archivosGo(dir string) ([]string, error) {
	var out []string
	for _, sub := range []string{"cmd", "internal"} {
		base := filepath.Join(dir, sub)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

func relativa(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func claves(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	vistos := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !vistos[s] {
			vistos[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sumLineas(archivos []Archivo) int {
	n := 0
	for _, a := range archivos {
		n += a.Lineas
	}
	return n
}
