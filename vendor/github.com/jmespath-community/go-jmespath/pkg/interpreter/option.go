package interpreter

type Option func(Options) Options

type Options struct {
	FunctionCaller FunctionCaller
}

func WithFunctionCaller(functionCaller FunctionCaller) Option {
	return func(o Options) Options {
		o.FunctionCaller = functionCaller
		return o
	}
}
