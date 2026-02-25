package cel

import (
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	"github.com/jmespath-community/go-jmespath/pkg/binding"
)

var (
	BindingsType = cel.OpaqueType("bindings")
	BaseEnv      = sync.OnceValues(func() (*cel.Env, error) {
		return cel.NewEnv(
			cel.Variable("object", cel.DynType),
			cel.Variable("bindings", BindingsType),
			cel.Function("resolve",
				cel.MemberOverload("bindings_resolve_string",
					[]*cel.Type{BindingsType, cel.StringType},
					cel.AnyType,
					cel.BinaryBinding(func(lhs, rhs ref.Val) ref.Val {
						bindings, ok := lhs.(Val[binding.Bindings])
						if !ok {
							return types.ValOrErr(bindings, "invalid bindings type")
						}
						name, ok := rhs.(types.String)
						if !ok {
							return types.ValOrErr(name, "invalid name type")
						}
						value, err := binding.Resolve("$"+string(name), bindings.Unwrap())
						if err != nil {
							return types.WrapErr(err)
						}
						return types.DefaultTypeAdapter.NativeToValue(value)
					}),
				),
			),
		)
	})
	DefaultEnv = sync.OnceValues(func() (*cel.Env, error) {
		if env, err := BaseEnv(); err != nil {
			return nil, err
		} else if env, err := env.Extend(
			cel.HomogeneousAggregateLiterals(),
			cel.EagerlyValidateDeclarations(true),
			cel.DefaultUTCTimeZone(true),
			cel.CrossTypeNumericComparisons(true),
			cel.OptionalTypes(),
			ext.Strings(),
			ext.Sets(),
		); err != nil {
			return nil, err
		} else {
			return env, nil
		}
	})
)
