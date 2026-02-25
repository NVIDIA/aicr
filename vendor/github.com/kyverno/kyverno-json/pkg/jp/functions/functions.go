package functions

import (
	"github.com/jmespath-community/go-jmespath/pkg/functions"
)

func GetFunctions() []functions.FunctionEntry {
	return []functions.FunctionEntry{{
		Name: "at",
		Arguments: []functions.ArgSpec{
			{Types: []functions.JpType{functions.JpArray}},
			{Types: []functions.JpType{functions.JpNumber}},
		},
		Handler:     jpfAt,
		Description: "Returns the element in an array at the given index.",
	}, {
		Name: "concat",
		Arguments: []functions.ArgSpec{
			{Types: []functions.JpType{functions.JpString}},
			{Types: []functions.JpType{functions.JpString}},
		},
		Handler:     jpfConcat,
		Description: "Concatenates two strings together and returns the result.",
	}, {
		Name: "json_parse",
		Arguments: []functions.ArgSpec{
			{Types: []functions.JpType{functions.JpString}},
		},
		Handler:     jpfJsonParse,
		Description: "Parses a given JSON string into an object.",
	}, {
		Name: "wildcard",
		Arguments: []functions.ArgSpec{
			{Types: []functions.JpType{functions.JpString}},
			{Types: []functions.JpType{functions.JpString}},
		},
		Handler:     jpfWildcard,
		Description: "Compares a wildcard pattern with a given string and returns if they match or not.",
	}}
}
