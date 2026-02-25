package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/jmespath-community/go-jmespath/pkg/binding"
)

type Program = func(any, binding.Bindings) (any, error)

type Compiler interface {
	Compile(string) (Program, error)
}

type compiler struct {
	env func() (*cel.Env, error)
}

func NewCompiler(env func() (*cel.Env, error)) *compiler {
	return &compiler{
		env: env,
	}
}

func (c *compiler) Compile(statement string) (Program, error) {
	env, err := c.env()
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(statement)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, err
	}
	return func(value any, bindings binding.Bindings) (any, error) {
		return Execute(program, value, bindings)
	}, nil
}

func Execute(program cel.Program, value any, bindings binding.Bindings) (any, error) {
	data := map[string]interface{}{
		"object":   value,
		"bindings": NewVal(bindings, BindingsType),
	}
	out, _, err := program.Eval(data)
	if err != nil {
		return nil, err
	}
	return out.Value(), nil
}
