// Copyright 2020 CUE Authors
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

package adt

// This file contains error encodings.
//
//
// *Bottom:
//    - an adt.Value
//    - always belongs to a single vertex.
//    - does NOT implement error
//    - marks error code used for control flow
//
// errors.Error
//    - CUE default error
//    - implements error
//    - tracks error locations
//    - has error message details
//    - supports multiple errors
//

import (
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/token"
)

// ErrorCode indicates the type of error. The type of error may influence
// control flow. No other aspects of an error may influence control flow.
type ErrorCode int

const (
	// An EvalError is a fatal evaluation error.
	EvalError ErrorCode = iota

	// A UserError is a fatal error originating from the user.
	UserError

	// NotExistError is used to indicate a value does not exist.
	// Mostly used for legacy reasons.
	NotExistError

	// StructuralCycleError means a structural cycle was found. Structural
	// cycles are permanent errors, but they are not passed up recursively,
	// as a unification of a value with a structural cycle with one that
	// doesn't may still give a useful result.
	StructuralCycleError

	// IncompleteError means an evaluation could not complete because of
	// insufficient information that may still be added later.
	IncompleteError

	// A CycleError indicates a reference error. It is considered to be
	// an incomplete error, as reference errors may be broken by providing
	// a concrete value.
	CycleError
)

func (c ErrorCode) String() string {
	switch c {
	case EvalError:
		return "eval"
	case UserError:
		return "user"
	case StructuralCycleError:
		return "structural cycle"
	case IncompleteError:
		return "incomplete"
	case CycleError:
		return "cycle"
	}
	return "unknown"
}

// Bottom represents an error or bottom symbol.
//
// Although a Bottom node holds control data, it should not be created until the
// control information already resulted in an error.
type Bottom struct {
	Src ast.Node
	Err errors.Error

	Code         ErrorCode
	HasRecursive bool
	ChildError   bool // Err is the error of the child
	// Value holds the computed value so far in case
	Value Value
}

func (x *Bottom) Source() ast.Node        { return x.Src }
func (x *Bottom) Kind() Kind              { return BottomKind }
func (x *Bottom) Specialize(k Kind) Value { return x } // XXX remove

func (b *Bottom) IsIncomplete() bool {
	if b == nil {
		return false
	}
	return b.Code == IncompleteError || b.Code == CycleError
}

// isLiteralBottom reports whether x is an error originating from a user.
func isLiteralBottom(x Expr) bool {
	b, ok := x.(*Bottom)
	return ok && b.Code == UserError
}

// isError reports whether v is an error or nil.
func isError(v Value) bool {
	if v == nil {
		return true
	}
	_, ok := v.(*Bottom)
	return ok
}

// isIncomplete reports whether v is associated with an incomplete error.
func isIncomplete(v *Vertex) bool {
	if v == nil {
		return true
	}
	if b, ok := v.Value.(*Bottom); ok {
		return b.IsIncomplete()
	}
	return false
}

// AddChildError updates x to record an error that occurred in one of
// its descendent arcs. The resulting error will record the worst error code of
// the current error or recursive error.
//
// If x is not already an error, the value is recorded in the error for
// reference.
//
func (v *Vertex) AddChildError(recursive *Bottom) {
	v.ChildErrors = CombineErrors(nil, v.ChildErrors, recursive)
	if recursive.IsIncomplete() {
		return
	}
	x := v.Value
	err, _ := x.(*Bottom)
	if err == nil {
		v.Value = &Bottom{
			Code:         recursive.Code,
			Value:        x,
			HasRecursive: true,
			ChildError:   true,
			Err:          recursive.Err,
		}
		return
	}

	err.HasRecursive = true
	if err.Code > recursive.Code {
		err.Code = recursive.Code
	}

	v.Value = err
}

// CombineErrors combines two errors that originate at the same Vertex.
func CombineErrors(src ast.Node, x, y Value) *Bottom {
	a, _ := x.(*Bottom)
	b, _ := y.(*Bottom)

	switch {
	case a != nil && b != nil:
	case a != nil:
		return a
	case b != nil:
		return b
	default:
		return nil
	}

	if a.Code != b.Code {
		if a.Code > b.Code {
			a, b = b, a
		}

		if b.Code >= IncompleteError {
			return a
		}
	}

	return &Bottom{
		Src:  src,
		Err:  errors.Append(a.Err, b.Err),
		Code: a.Code,
	}
}

// A valueError is returned as a result of evaluating a value.
type valueError struct {
	r   Runtime
	v   *Vertex
	pos []token.Pos
	errors.Message
}

func (c *OpContext) errNode() *Vertex {
	return c.vertex
}

func (c *OpContext) Newf(format string, args ...interface{}) *valueError {
	return c.NewPosf(c.pos(), format, args...)
}

func (c *OpContext) NewPosf(pos token.Pos, format string, args ...interface{}) *valueError {
	return &valueError{
		r: c.Runtime,
		v: c.errNode(),
		// TODO: leave ni if it can be derived from the source and save an
		// allocation.
		pos:     []token.Pos{pos},
		Message: errors.NewMessage(format, args),
	}
}

func (e *valueError) Error() string {
	return errors.String(e)
}

func (e *valueError) Position() token.Pos {
	if len(e.pos) == 0 {
		// TODO: retrieve from source
		return token.NoPos
	}
	return e.pos[0]
}

func (e *valueError) InputPositions() []token.Pos {
	return e.pos
}

func (e *valueError) Path() (a []string) {
	for _, f := range appendPath(nil, e.v) {
		a = append(a, f.SelectorString(e.r))
	}
	return a
}