package functions

func GetDefaultFunctions() []FunctionEntry {
	return []FunctionEntry{{
		Name: "abs",
		Arguments: []ArgSpec{
			{Types: []JpType{JpNumber}},
		},
		Handler:     jpfAbs,
		Description: "Returns the absolute value of the provided argument.",
	}, {
		Name: "avg",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArrayNumber}},
		},
		Handler:     jpfAvg,
		Description: "Returns the average of the elements in the provided array. An empty array will produce a return value of null.",
	}, {
		Name: "ceil",
		Arguments: []ArgSpec{
			{Types: []JpType{JpNumber}},
		},
		Handler:     jpfCeil,
		Description: "Returns the next highest integer value by rounding up if necessary.",
	}, {
		Name: "contains",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray, JpString}},
			{Types: []JpType{JpAny}},
		},
		Handler:     jpfContains,
		Description: "Returns `true` if the given subject contains the provided search value. If the subject is an array, this function returns `true` if one of the elements in the array is equal to the provided search value. If the provided subject is a string, this function returns `true` if the string contains the provided search argument.",
	}, {
		Name: "ends_with",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
		},
		Handler:     jpfEndsWith,
		Description: "Reports whether the given string ends with the provided suffix argument.",
	}, {
		Name: "find_first",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
			{Types: []JpType{JpNumber}, Optional: true},
			{Types: []JpType{JpNumber}, Optional: true},
		},
		Handler:     jpfFindFirst,
		Description: "Returns the zero-based index of the first occurence where the substring appears in a string or null if it does not appear.",
	}, {
		Name: "find_last",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
			{Types: []JpType{JpNumber}, Optional: true},
			{Types: []JpType{JpNumber}, Optional: true},
		},
		Handler:     jpfFindLast,
		Description: "Returns the zero-based index of the last occurence where the substring appears in a string or null if it does not appear.",
	}, {
		Name: "floor",
		Arguments: []ArgSpec{
			{Types: []JpType{JpNumber}},
		},
		Handler:     jpfFloor,
		Description: "Returns the next lowest integer value by rounding down if necessary.",
	}, {
		Name: "from_items",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArrayArray}},
		},
		Handler:     jpfFromItems,
		Description: "Returns an object from the provided array of key value pairs. This function is the inversed of the `items()` function.",
	}, {
		Name: "group_by",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray}},
			{Types: []JpType{JpExpref}},
		},
		Handler:     jpfGroupBy,
		Description: "Groups an array of objects using an expression as the group key.",
	}, {
		Name: "items",
		Arguments: []ArgSpec{
			{Types: []JpType{JpObject}},
		},
		Handler:     jpfItems,
		Description: "Converts a given object into an array of key-value pairs.",
	}, {
		Name: "join",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpArrayString}},
		},
		Handler:     jpfJoin,
		Description: "Returns all of the elements from the provided array joined together using the glue argument as a separator between each.",
	}, {
		Name: "keys",
		Arguments: []ArgSpec{
			{Types: []JpType{JpObject}},
		},
		Handler:     jpfKeys,
		Description: "Returns an array containing the keys of the provided object.",
	}, {
		Name: "length",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString, JpArray, JpObject}},
		},
		Handler:     jpfLength,
		Description: "Returns the length of the given argument. If the argument is a string this function returns the number of code points in the string. If the argument is an array this function returns the number of elements in the array. If the argument is an object this function returns the number of key-value pairs in the object.",
	}, {
		Name: "lower",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
		},
		Handler:     jpfLower,
		Description: "Returns the given string with all Unicode letters mapped to their lower case.",
	}, {
		Name: "map",
		Arguments: []ArgSpec{
			{Types: []JpType{JpExpref}},
			{Types: []JpType{JpArray}},
		},
		Handler:     jpfMap,
		Description: "Transforms elements in a given array and returns the result.",
	}, {
		Name: "max",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArrayNumber, JpArrayString}},
		},
		Handler:     jpfMax,
		Description: "Returns the highest found element in the provided array argument. An empty array will produce a return value of null.",
	}, {
		Name: "max_by",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray}},
			{Types: []JpType{JpExpref}},
		},
		Handler:     jpfMaxBy,
		Description: "Returns the highest found element using a custom expression to compute the associated value for each element in the input array.",
	}, {
		Name: "merge",
		Arguments: []ArgSpec{
			{Types: []JpType{JpObject}, Variadic: true},
		},
		Handler:     jpfMerge,
		Description: "Meges a list of objects together and returns the result.",
	}, {
		Name: "min",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArrayNumber, JpArrayString}},
		},
		Handler:     jpfMin,
		Description: "Returns the lowest found element in the provided array argument.",
	}, {
		Name: "min_by",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray}},
			{Types: []JpType{JpExpref}},
		},
		Handler:     jpfMinBy,
		Description: "Returns the lowest found element using a custom expression to compute the associated value for each element in the input array.",
	}, {
		Name: "not_null",
		Arguments: []ArgSpec{
			{Types: []JpType{JpAny}, Variadic: true},
		},
		Handler:     jpfNotNull,
		Description: "Returns the first non null element in the input array.",
	}, {
		Name: "pad_left",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpNumber}},
			{Types: []JpType{JpString}, Optional: true},
		},
		Handler:     jpfPadLeft,
		Description: "Adds characters to the beginning of a string.",
	}, {
		Name: "pad_right",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpNumber}},
			{Types: []JpType{JpString}, Optional: true},
		},
		Handler:     jpfPadRight,
		Description: "Adds characters to the end of a string.",
	}, {
		Name: "replace",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
			{Types: []JpType{JpNumber}, Optional: true},
		},
		Handler:     jpfReplace,
		Description: "Returns a copy of the input string with instances of old string argument replaced by new string argument.",
	}, {
		Name: "reverse",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray, JpString}},
		},
		Handler:     jpfReverse,
		Description: "Reverses the input string or array and returns the result.",
	}, {
		Name: "sort",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArrayString, JpArrayNumber}},
		},
		Handler:     jpfSort,
		Description: "This function accepts an array argument and returns the sorted elements as an array.",
	}, {
		Name: "sort_by",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray}},
			{Types: []JpType{JpExpref}},
		},
		Handler:     jpfSortBy,
		Description: "This function accepts an array argument and returns the sorted elements as an array using a custom expression to compute the associated value for each element.",
	}, {
		Name: "split",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
			{Types: []JpType{JpNumber}, Optional: true},
		},
		Handler:     jpfSplit,
		Description: "Slices input string into substrings separated by a string argument and returns an array of the substrings between those separators.",
	}, {
		Name: "starts_with",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}},
		},
		Handler:     jpfStartsWith,
		Description: "Reports whether the input string begins with the provided string prefix argument.",
	}, {
		Name: "sum",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArrayNumber}},
		},
		Handler:     jpfSum,
		Description: "Returns the sum of all numbers contained in the provided array.",
	}, {
		Name: "to_array",
		Arguments: []ArgSpec{
			{Types: []JpType{JpAny}},
		},
		Handler:     jpfToArray,
		Description: "Returns a one element array containing the passed in argument, or the passed in value if it's an array.",
	}, {
		Name: "to_number",
		Arguments: []ArgSpec{
			{Types: []JpType{JpAny}},
		},
		Handler:     jpfToNumber,
		Description: "Returns the parsed number.",
	}, {
		Name: "to_string",
		Arguments: []ArgSpec{
			{Types: []JpType{JpAny}},
		},
		Handler:     jpfToString,
		Description: "The JSON encoded value of the given argument.",
	}, {
		Name: "trim",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}, Optional: true},
		},
		Handler:     jpfTrim,
		Description: "Removes the leading and trailing characters found in the passed in string argument.",
	}, {
		Name: "trim_left",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}, Optional: true},
		},
		Handler:     jpfTrimLeft,
		Description: "Removes the leading characters found in the passed in string argument.",
	}, {
		Name: "trim_right",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
			{Types: []JpType{JpString}, Optional: true},
		},
		Handler:     jpfTrimRight,
		Description: "Removes the trailing characters found in the passed in string argument.",
	}, {
		Name: "type",
		Arguments: []ArgSpec{
			{Types: []JpType{JpAny}},
		},
		Handler:     jpfType,
		Description: "Returns the JavaScript type of the given argument as a string value.",
	}, {
		Name: "upper",
		Arguments: []ArgSpec{
			{Types: []JpType{JpString}},
		},
		Handler:     jpfUpper,
		Description: "Returns the given string with all Unicode letters mapped to their upper case.",
	}, {
		Name: "values",
		Arguments: []ArgSpec{
			{Types: []JpType{JpObject}},
		},
		Handler:     jpfValues,
		Description: "Returns the values of the provided object.",
	}, {
		Name: "zip",
		Arguments: []ArgSpec{
			{Types: []JpType{JpArray}},
			{Types: []JpType{JpArray}, Variadic: true},
		},
		Handler:     jpfZip,
		Description: "Accepts one or more arrays as arguments and returns an array of arrays in which the i-th array contains the i-th element from each of the argument arrays. The returned array is truncated to the length of the shortest argument array.",
	}}
}
