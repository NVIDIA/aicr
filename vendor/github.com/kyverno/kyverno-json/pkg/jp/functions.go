package jp

import (
	"context"

	jpfunctions "github.com/jmespath-community/go-jmespath/pkg/functions"
	"github.com/kyverno/kyverno-json/pkg/jp/functions"
	kyvernofunctions "github.com/kyverno/kyverno-json/pkg/jp/kyverno"
)

func GetFunctions(ctx context.Context) []jpfunctions.FunctionEntry {
	var funcs []jpfunctions.FunctionEntry
	funcs = append(funcs, jpfunctions.GetDefaultFunctions()...)
	funcs = append(funcs, functions.GetFunctions()...)
	funcs = append(funcs, kyvernofunctions.GetBareFunctions()...)
	return funcs
}
