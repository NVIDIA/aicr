package projection

import (
	"errors"
	"reflect"

	"github.com/jmespath-community/go-jmespath/pkg/binding"
	"github.com/kyverno/kyverno-json/pkg/core/compilers"
	"github.com/kyverno/kyverno-json/pkg/core/expression"
	reflectutils "github.com/kyverno/kyverno-json/pkg/utils/reflect"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

type (
	ScalarHandler = func(value any, bindings binding.Bindings) (any, error)
	MapKeyHandler = func(value any, bindings binding.Bindings) (any, bool, error)
)

type Info struct {
	Foreach     bool
	ForeachName string
	Binding     string
}

type Projection struct {
	Info
	Handler MapKeyHandler
}

func ParseMapKey(path *field.Path, in any, compiler compilers.Compilers) (*Projection, *field.Error) {
	var projection Projection
	switch typed := in.(type) {
	case string:
		// 1. if we have a string, parse the expression
		expr := expression.Parse(typed)
		// 2. record projection infos
		projection.Foreach = expr.Foreach
		projection.ForeachName = expr.ForeachName
		projection.Binding = expr.Binding
		// 3. compute the projection func
		if compiler := compiler.Compiler(expr.Compiler); compiler != nil {
			program, err := compiler.Compile(expr.Statement)
			if err != nil {
				return nil, field.Invalid(path, expr.Statement, err.Error())
			}
			projection.Handler = func(value any, bindings binding.Bindings) (any, bool, error) {
				projected, err := program(value, bindings)
				if err != nil {
					return nil, false, err
				}
				return projected, true, nil
			}
		} else {
			projection.Handler = func(value any, bindings binding.Bindings) (any, bool, error) {
				if value == nil {
					return nil, false, nil
				}
				if reflectutils.GetKind(value) == reflect.Map {
					value := reflect.ValueOf(value).MapIndex(reflect.ValueOf(expr.Statement))
					if !value.IsValid() {
						return nil, false, nil
					}
					return value.Interface(), true, nil
				}
				return nil, false, errors.New("projection not recognized")
			}
		}
	default:
		// 1. compute the projection func
		projection.Handler = func(value any, bindings binding.Bindings) (any, bool, error) {
			if value == nil {
				return nil, false, nil
			}
			if reflectutils.GetKind(value) == reflect.Map {
				mapValue := reflect.ValueOf(value).MapIndex(reflect.ValueOf(typed))
				if !mapValue.IsValid() {
					return nil, false, nil
				}
				return mapValue.Interface(), true, nil
			}
			return nil, false, errors.New("projection not recognized")
		}
	}
	return &projection, nil
}

func ParseScalar(path *field.Path, in any, compiler compilers.Compilers) (ScalarHandler, *field.Error) {
	switch typed := in.(type) {
	case string:
		expr := expression.Parse(typed)
		if expr.Foreach {
			return nil, field.Invalid(path, typed, "foreach is not supported in scalar projections")
		}
		if expr.Binding != "" {
			return nil, field.Invalid(path, typed, "binding is not supported in scalar projections")
		}
		if compiler := compiler.Compiler(expr.Compiler); compiler != nil {
			program, err := compiler.Compile(expr.Statement)
			if err != nil {
				return nil, field.Invalid(path, expr.Statement, err.Error())
			}
			return func(value any, bindings binding.Bindings) (any, error) {
				projected, err := program(value, bindings)
				if err != nil {
					return nil, err
				}
				return projected, nil
			}, nil
		} else {
			return func(value any, bindings binding.Bindings) (any, error) {
				return expr.Statement, nil
			}, nil
		}
	default:
		return func(value any, bindings binding.Bindings) (any, error) {
			return typed, nil
		}, nil
	}
}
