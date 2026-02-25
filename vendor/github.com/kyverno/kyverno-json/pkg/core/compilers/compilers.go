package compilers

import (
	"github.com/kyverno/kyverno-json/pkg/core/compilers/cel"
	"github.com/kyverno/kyverno-json/pkg/core/compilers/jp"
	"github.com/kyverno/kyverno-json/pkg/core/expression"
)

const (
	CompilerCEL = expression.CompilerCEL
	CompilerJP  = expression.CompilerJP
)

var DefaultCompilers = Compilers{
	Jp:  jp.NewCompiler(),
	Cel: cel.NewCompiler(cel.DefaultEnv),
}

type Compilers struct {
	Jp      jp.Compiler
	Cel     cel.Compiler
	Default cel.Compiler
}

func (c Compilers) Compiler(compiler string) Compiler {
	switch compiler {
	case expression.CompilerJP:
		return c.Jp
	case expression.CompilerCEL:
		return c.Cel
	case expression.CompilerDefault:
		return c.Default
	}
	return nil
}

func (c Compilers) WithDefaultCompiler(defaultCompiler string) Compilers {
	switch defaultCompiler {
	case expression.CompilerJP:
		c.Default = c.Jp
	case expression.CompilerCEL:
		c.Default = c.Cel
	}
	return c
}
