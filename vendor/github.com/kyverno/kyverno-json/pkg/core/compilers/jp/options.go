package jp

import (
	"context"

	"github.com/jmespath-community/go-jmespath/pkg/interpreter"
	"github.com/kyverno/kyverno-json/pkg/jp"
)

var (
	funcs         = jp.GetFunctions(context.Background())
	defaultCaller = interpreter.NewFunctionCaller(funcs...)
)

type Option func(options) options

type options struct {
	functionCaller interpreter.FunctionCaller
}

func WithFunctionCaller(functionCaller interpreter.FunctionCaller) Option {
	return func(o options) options {
		o.functionCaller = functionCaller
		return o
	}
}

func buildOptions(opts ...Option) options {
	var o options
	for _, opt := range opts {
		if opt != nil {
			o = opt(o)
		}
	}
	if o.functionCaller == nil {
		o.functionCaller = defaultCaller
	}
	return o
}
