package jp

import (
	"sync"

	"github.com/jmespath-community/go-jmespath/pkg/binding"
	"github.com/jmespath-community/go-jmespath/pkg/interpreter"
	"github.com/jmespath-community/go-jmespath/pkg/parsing"
)

type Program = func(any, binding.Bindings) (any, error)

type Compiler interface {
	Compile(string) (Program, error)
	Options() []Option
}

type compiler struct {
	options      []Option
	buildOptions func() options
}

func NewCompiler(opts ...Option) *compiler {
	return &compiler{
		options: opts,
		buildOptions: sync.OnceValue(func() options {
			return buildOptions(opts...)
		}),
	}
}

func (c *compiler) Options() []Option {
	return c.options
}

func (c *compiler) Compile(statement string) (Program, error) {
	parser := parsing.NewParser()
	compiled, err := parser.Parse(statement)
	if err != nil {
		return nil, err
	}
	return func(value any, bindings binding.Bindings) (any, error) {
		return execute(compiled, value, bindings, c.buildOptions())
	}, nil
}

func Execute(ast parsing.ASTNode, value any, bindings binding.Bindings, opts ...Option) (any, error) {
	return execute(ast, value, bindings, buildOptions(opts...))
}

func execute(ast parsing.ASTNode, value any, bindings binding.Bindings, options options) (any, error) {
	vm := interpreter.NewInterpreter(nil, bindings)
	return vm.Execute(ast, value, interpreter.WithFunctionCaller(options.functionCaller))
}
