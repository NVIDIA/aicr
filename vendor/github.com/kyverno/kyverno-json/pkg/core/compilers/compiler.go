package compilers

import (
	"github.com/jmespath-community/go-jmespath/pkg/binding"
)

type Program = func(any, binding.Bindings) (any, error)

type Compiler interface {
	Compile(string) (Program, error)
}

func Execute(statement string, value any, bindings binding.Bindings, compiler Compiler) (any, error) {
	program, err := compiler.Compile(statement)
	if err != nil {
		return nil, err
	}
	return program(value, bindings)
}
