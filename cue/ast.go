// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cue

import (
	"fmt"
	"strconv"
	"strings"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/literal"
	"cuelang.org/go/cue/token"
	"cuelang.org/go/internal"
)

// insertFile inserts the given file at the root of the instance.
//
// The contents will be merged (unified) with any pre-existing value. In this
// case an error may be reported, but only if the merge failed at the top-level.
// Other errors will be recorded at the respective values in the tree.
//
// There should be no unresolved identifiers in file, meaning the Node field
// of all identifiers should be set to a non-nil value.
func (inst *Instance) insertFile(f *ast.File) error {
	// TODO: insert by converting to value first so that the trim command can
	// also remove top-level fields.
	// First process single file.
	v := newVisitor(inst.index, inst.inst, inst.rootStruct, inst.scope)
	v.astState.astMap[f] = inst.rootStruct
	// TODO: fix cmd/import to resolve references in the AST before
	// inserting. For now, we accept errors that did not make it up to the tree.
	result := v.walk(f)
	if isBottom(result) {
		if v.errors.Len() > 0 {
			return v.errors
		}
		return &callError{result.(*bottom)}
	}

	return nil
}

type astVisitor struct {
	*astState
	object *structLit

	parent *astVisitor
	sel    string // label or index; may be '*'
	// For single line fields, the doc comment is applied to the inner-most
	// field value.
	//
	//   // This comment is for bar.
	//   foo bar: value
	//
	doc *docNode

	inSelector int
}

func (v *astVisitor) ctx() *context {
	return v.astState.ctx
}

type astState struct {
	ctx *context
	*index
	inst *build.Instance

	litParser   *litParser
	resolveRoot *structLit

	// make unique per level to avoid reuse of structs being an issue.
	astMap map[ast.Node]scope

	errors errors.List
}

func (s *astState) mapScope(n ast.Node) (m scope) {
	if m = s.astMap[n]; m == nil {
		m = newStruct(newNode(n))
		s.astMap[n] = m
	}
	return m
}

func (s *astState) setScope(n ast.Node, v scope) {
	if m, ok := s.astMap[n]; ok && m != v {
		panic("already defined")
	}
	s.astMap[n] = v
}

func newVisitor(idx *index, inst *build.Instance, obj, resolveRoot *structLit) *astVisitor {
	ctx := idx.newContext()
	return newVisitorCtx(ctx, inst, obj, resolveRoot)
}

func newVisitorCtx(ctx *context, inst *build.Instance, obj, resolveRoot *structLit) *astVisitor {
	v := &astVisitor{
		object: obj,
	}
	v.astState = &astState{
		ctx:         ctx,
		index:       ctx.index,
		inst:        inst,
		litParser:   &litParser{ctx: ctx},
		resolveRoot: resolveRoot,
		astMap:      map[ast.Node]scope{},
	}
	return v
}

func (v *astVisitor) errf(n ast.Node, format string, args ...interface{}) evaluated {
	v.astState.errors.Add(&nodeError{
		path: v.appendPath(nil),
		n:    n,
		Message: errors.Message{
			Format: format,
			Args:   args,
		},
	})
	arguments := append([]interface{}{format}, args...)
	return v.mkErr(newNode(n), arguments...)
}

func (v *astVisitor) appendPath(a []string) []string {
	if v.parent != nil {
		a = v.parent.appendPath(a)
	}
	if v.sel != "" {
		a = append(a, v.sel)
	}
	return a
}

func (v *astVisitor) resolve(n *ast.Ident) value {
	ctx := v.ctx()
	label := v.label(n.Name, true)
	if r := v.resolveRoot; r != nil {
		for _, a := range r.arcs {
			if a.feature == label {
				return &selectorExpr{newExpr(n),
					&nodeRef{baseValue: newExpr(n), node: r}, label}
			}
		}
		if v.inSelector > 0 {
			if p := getBuiltinShorthandPkg(ctx, n.Name); p != nil {
				return &nodeRef{baseValue: newExpr(n), node: p}
			}
		}
	}
	return nil
}

func (v *astVisitor) loadImport(imp *ast.ImportSpec) evaluated {
	ctx := v.ctx()
	path, err := literal.Unquote(imp.Path.Value)
	if err != nil {
		return v.errf(imp, "illformed import spec")
	}
	// TODO: allow builtin *and* imported package. The result is a unified
	// struct.
	if p := getBuiltinPkg(ctx, path); p != nil {
		return p
	}
	bimp := v.inst.LookupImport(path)
	if bimp == nil {
		return v.errf(imp, "package %q not found", path)
	}
	impInst := v.index.loadInstance(bimp)
	return impInst.rootValue.evalPartial(ctx)
}

// We probably don't need to call Walk.s
func (v *astVisitor) walk(astNode ast.Node) (value value) {
	switch n := astNode.(type) {
	case *ast.File:
		obj := v.object
		v1 := &astVisitor{
			astState: v.astState,
			object:   obj,
		}
		for _, e := range n.Decls {
			switch x := e.(type) {
			case *ast.EmitDecl:
				if v1.object.emit == nil {
					v1.object.emit = v1.walk(x.Expr)
				} else {
					v1.object.emit = mkBin(v.ctx(), token.NoPos, opUnify, v1.object.emit, v1.walk(x.Expr))
				}
			default:
				v1.walk(e)
			}
		}
		value = obj

	case *ast.ImportDecl:
		for _, s := range n.Specs {
			v.walk(s)
		}

	case *ast.ImportSpec:
		val := v.loadImport(n)
		if !isBottom(val) {
			v.setScope(n, val.(*structLit))
		}

	case *ast.StructLit:
		obj := v.mapScope(n).(*structLit)
		v1 := &astVisitor{
			astState: v.astState,
			object:   obj,
			parent:   v,
		}
		passDoc := len(n.Elts) == 1 && !n.Lbrace.IsValid() && v.doc != nil
		if passDoc {
			v1.doc = v.doc
		}
		for _, e := range n.Elts {
			switch x := e.(type) {
			case *ast.EmitDecl:
				// Only allowed at top-level.
				return v1.errf(x, "emitting values is only allowed at top level")
			case *ast.Field, *ast.Alias:
				v1.walk(e)
			case *ast.ComprehensionDecl:
				v1.walk(x)
			}
		}
		if passDoc {
			v.doc = v1.doc // signal usage of document back to parent.
		}
		value = obj

	case *ast.ListLit:
		v1 := &astVisitor{
			astState: v.astState,
			object:   v.object,
			parent:   v,
		}
		arcs := []arc{}
		for i, e := range n.Elts {
			v1.sel = strconv.Itoa(i)
			arcs = append(arcs, arc{feature: label(i), v: v1.walk(e)})
		}
		s := &structLit{baseValue: newExpr(n), arcs: arcs}
		list := &list{baseValue: newExpr(n), elem: s}
		list.initLit()
		if n.Ellipsis != token.NoPos || n.Type != nil {
			list.len = newBound(list.baseValue, opGeq, intKind, list.len)
			if n.Type != nil {
				list.typ = v1.walk(n.Type)
			}
		}
		value = list

	case *ast.ComprehensionDecl:
		yielder := &yield{baseValue: newExpr(n.Field.Value)}
		fc := &fieldComprehension{
			baseValue: newDecl(n),
			clauses:   wrapClauses(v, yielder, n.Clauses),
		}
		field := n.Field
		switch x := field.Label.(type) {
		case *ast.Interpolation:
			v.sel = "?"
			yielder.key = v.walk(x)
			yielder.value = v.walk(field.Value)

		case *ast.TemplateLabel:
			v.sel = "*"
			f := v.label(x.Ident.Name, true)

			sig := &params{}
			sig.add(f, &basicType{newNode(field.Label), stringKind})
			template := &lambdaExpr{newExpr(field.Value), sig, nil}

			v.setScope(field, template)
			template.value = v.walk(field.Value)
			yielder.value = template
			fc.isTemplate = true

		case *ast.BasicLit, *ast.Ident:
			name, ok := ast.LabelName(x)
			if !ok {
				return v.errf(x, "invalid field name: %v", x)
			}
			// TODO: is this correct? Just for info, so not very important.
			v.sel = name

			// TODO: if the clauses do not contain a guard, we know that this
			// field will always be added and we can move the comprehension one
			// level down. This, in turn, has the advantage that it is more
			// likely that the cross-reference limitation for field
			// comprehensions is not violated. To ensure compatibility between
			// implementations, though, we should relax the spec as well.
			// The cross-reference rule is simple and this relaxation seems a
			// bit more complex.

			// TODO: for now we can also consider making this an error if
			// the list of clauses does not contain if and make a suggestion
			// to rewrite it.

			if name != "" {
				yielder.key = &stringLit{newNode(x), name}
				yielder.value = v.walk(field.Value)
			}

		default:
			panic("cue: unknown label type")
		}
		// yielder.key = v.walk(n.Field.Label)
		// yielder.value = v.walk(n.Field.Value)
		v.object.comprehensions = append(v.object.comprehensions, fc)

	case *ast.Field:
		opt := n.Optional != token.NoPos
		switch x := n.Label.(type) {
		case *ast.Interpolation:
			v.sel = "?"
			yielder := &yield{baseValue: newNode(x)}
			fc := &fieldComprehension{
				baseValue: newDecl(n),
				clauses:   yielder,
			}
			yielder.key = v.walk(x)
			yielder.value = v.walk(n.Value)
			yielder.opt = opt
			v.object.comprehensions = append(v.object.comprehensions, fc)

		case *ast.TemplateLabel:
			v.sel = "*"
			f := v.label(x.Ident.Name, true)

			sig := &params{}
			sig.add(f, &basicType{newNode(n.Label), stringKind})
			template := &lambdaExpr{newNode(n), sig, nil}

			v.setScope(n, template)
			template.value = v.walk(n.Value)

			if v.object.template == nil {
				v.object.template = template
			} else {
				v.object.template = mkBin(v.ctx(), token.NoPos, opUnify, v.object.template, template)
			}

		case *ast.BasicLit, *ast.Ident:
			if internal.DropOptional && opt {
				break
			}
			v.sel, _ = ast.LabelName(x)
			attrs, err := createAttrs(v.ctx(), newNode(n), n.Attrs)
			if err != nil {
				return err
			}
			f, ok := v.nodeLabel(x)
			if !ok {
				return v.errf(n.Label, "invalid field name: %v", n.Label)
			}
			if f != 0 {
				var leftOverDoc *docNode
				for _, c := range n.Comments() {
					if c.Position == 0 {
						leftOverDoc = v.doc
						v.doc = &docNode{n: n}
						break
					}
				}
				val := v.walk(n.Value)
				v.object.insertValue(v.ctx(), f, opt, val, attrs, v.doc)
				v.doc = leftOverDoc
			}

		default:
			panic("cue: unknown label type")
		}

	case *ast.Alias:
		// parsed verbatim at reference.

	case *ast.ListComprehension:
		yielder := &yield{baseValue: newExpr(n.Expr)}
		lc := &listComprehension{
			newExpr(n),
			wrapClauses(v, yielder, n.Clauses),
		}
		// we don't support key for lists (yet?)
		yielder.value = v.walk(n.Expr)
		return lc

	// Expressions
	case *ast.Ident:
		if n.Node == nil {
			if value = v.resolve(n); value != nil {
				break
			}

			switch n.Name {
			case "_":
				return &top{newExpr(n)}
			case "string":
				return &basicType{newExpr(n), stringKind}
			case "bytes":
				return &basicType{newExpr(n), bytesKind}
			case "bool":
				return &basicType{newExpr(n), boolKind}
			case "int":
				return &basicType{newExpr(n), intKind}
			case "float":
				return &basicType{newExpr(n), floatKind}
			case "number":
				return &basicType{newExpr(n), numKind}
			case "duration":
				return &basicType{newExpr(n), durationKind}

			case "len":
				return lenBuiltin
			case "and":
				return andBuiltin
			case "or":
				return orBuiltin
			}
			if r, ok := predefinedRanges[n.Name]; ok {
				return r
			}

			value = v.errf(n, "reference %q not found", n.Name)
			break
		}

		if a, ok := n.Node.(*ast.Alias); ok {
			value = v.walk(a.Expr)
			break
		}

		label := v.label(n.Name, true)
		if n.Scope != nil {
			n2 := v.mapScope(n.Scope)
			value = &nodeRef{baseValue: newExpr(n), node: n2}
			value = &selectorExpr{newExpr(n), value, label}
		} else {
			n2 := v.mapScope(n.Node)
			value = &nodeRef{baseValue: newExpr(n), node: n2}
		}

	case *ast.BottomLit:
		// TODO: record inline comment.
		value = &bottom{baseValue: newExpr(n), msg: "from source"}

	case *ast.BadDecl:
		// nothing to do

	case *ast.BadExpr:
		value = v.errf(n, "invalid expression")

	case *ast.BasicLit:
		value = v.litParser.parse(n)

	case *ast.Interpolation:
		if len(n.Elts) == 0 {
			return v.errf(n, "invalid interpolation")
		}
		first, ok1 := n.Elts[0].(*ast.BasicLit)
		last, ok2 := n.Elts[len(n.Elts)-1].(*ast.BasicLit)
		if !ok1 || !ok2 {
			return v.errf(n, "invalid interpolation")
		}
		if len(n.Elts) == 1 {
			value = v.walk(n.Elts[0])
			break
		}
		lit := &interpolation{baseValue: newExpr(n), k: stringKind}
		value = lit
		info, prefixLen, _, err := literal.ParseQuotes(first.Value, last.Value)
		if err != nil {
			return v.errf(n, "invalid interpolation: %v", err)
		}
		prefix := ""
		for i := 0; i < len(n.Elts); i += 2 {
			l, ok := n.Elts[i].(*ast.BasicLit)
			if !ok {
				return v.errf(n, "invalid interpolation")
			}
			s := l.Value
			if !strings.HasPrefix(s, prefix) {
				return v.errf(l, "invalid interpolation: unmatched ')'")
			}
			s = l.Value[prefixLen:]
			x := parseString(v.ctx(), l, info, s)
			lit.parts = append(lit.parts, x)
			if i+1 < len(n.Elts) {
				lit.parts = append(lit.parts, v.walk(n.Elts[i+1]))
			}
			prefix = ")"
			prefixLen = 1
		}

	case *ast.ParenExpr:
		value = v.walk(n.X)

	case *ast.SelectorExpr:
		v.inSelector++
		value = &selectorExpr{
			newExpr(n),
			v.walk(n.X),
			v.label(n.Sel.Name, true),
		}
		v.inSelector--

	case *ast.IndexExpr:
		value = &indexExpr{newExpr(n), v.walk(n.X), v.walk(n.Index)}

	case *ast.SliceExpr:
		slice := &sliceExpr{baseValue: newExpr(n), x: v.walk(n.X)}
		if n.Low != nil {
			slice.lo = v.walk(n.Low)
		}
		if n.High != nil {
			slice.hi = v.walk(n.High)
		}
		value = slice

	case *ast.CallExpr:
		call := &callExpr{baseValue: newExpr(n), x: v.walk(n.Fun)}
		for _, a := range n.Args {
			call.args = append(call.args, v.walk(a))
		}
		value = call

	case *ast.UnaryExpr:
		switch n.Op {
		case token.NOT, token.ADD, token.SUB:
			value = &unaryExpr{
				newExpr(n),
				tokenMap[n.Op],
				v.walk(n.X),
			}
		case token.GEQ, token.GTR, token.LSS, token.LEQ,
			token.NEQ, token.MAT, token.NMAT:
			value = newBound(
				newExpr(n),
				tokenMap[n.Op],
				topKind|nonGround,
				v.walk(n.X),
			)

		case token.MUL:
			return v.errf(n, "preference mark not allowed at this position")
		default:
			return v.errf(n, "unsupported unary operator %q", n.Op)
		}

	case *ast.BinaryExpr:
		switch n.Op {
		case token.OR:
			d := &disjunction{baseValue: newExpr(n)}
			v.addDisjunctionElem(d, n.X, false)
			v.addDisjunctionElem(d, n.Y, false)
			value = d

		default:
			value = &binaryExpr{
				newExpr(n),
				tokenMap[n.Op], // op
				v.walk(n.X),    // left
				v.walk(n.Y),    // right
			}
		}

	case *ast.CommentGroup:
		// Nothing to do for a free-floating comment group.

	// nothing to do
	// case *syntax.EmitDecl:
	default:
		// TODO: unhandled node.
		// value = ctx.mkErr(n, "unknown node type %T", n)
		panic(fmt.Sprintf("unimplemented %T", n))

	}
	return value
}

func (v *astVisitor) addDisjunctionElem(d *disjunction, n ast.Node, mark bool) {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.OR {
			v.addDisjunctionElem(d, x.X, mark)
			v.addDisjunctionElem(d, x.Y, mark)
			return
		}
	case *ast.UnaryExpr:
		if x.Op == token.MUL {
			mark = true
			n = x.X
		}
		d.hasDefaults = true
	}
	d.values = append(d.values, dValue{v.walk(n), mark})
}

func wrapClauses(v *astVisitor, y yielder, clauses []ast.Clause) yielder {
	for _, c := range clauses {
		if n, ok := c.(*ast.ForClause); ok {
			params := &params{}
			fn := &lambdaExpr{newExpr(n.Source), params, nil}
			v.setScope(n, fn)
		}
	}
	for i := len(clauses) - 1; i >= 0; i-- {
		switch n := clauses[i].(type) {
		case *ast.ForClause:
			fn := v.mapScope(n).(*lambdaExpr)
			fn.value = y

			key := "_"
			if n.Key != nil {
				key = n.Key.Name
			}
			f := v.label(key, true)
			fn.add(f, &basicType{newExpr(n.Key), stringKind | intKind})

			f = v.label(n.Value.Name, true)
			fn.add(f, &top{})

			y = &feed{newExpr(n.Source), v.walk(n.Source), fn}

		case *ast.IfClause:
			y = &guard{newExpr(n.Condition), v.walk(n.Condition), y}
		}
	}
	return y
}
