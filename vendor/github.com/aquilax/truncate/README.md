# truncate

[![GoDoc](https://godoc.org/github.com/aquilax/truncate?status.svg)](https://godoc.org/github.com/aquilax/truncate)

Go library for truncating strings

Sample usage:

```go
package truncate_test

import (
	"fmt"
	"unicode/utf8"

	"github.com/aquilax/truncate"
)

func ExampleTruncate() {
	text := "This is a long text"
	truncated := truncate.Truncate(text, 17, "...", truncate.PositionEnd)
	fmt.Printf("%s : %d characters", truncated, utf8.RuneCountInString(truncated))
	// Output: This is a long... : 17 characters
}

func ExampleTruncate_second() {
	text := "This is a long text"
	truncated := truncate.Truncate(text, 15, "...", truncate.PositionStart)
	fmt.Printf("%s : %d characters", truncated, utf8.RuneCountInString(truncated))
	// Output: ... a long text : 15 characters
}

func ExampleTruncate_third() {
	text := "This is a long text"
	truncated := truncate.Truncate(text, 5, "zzz", truncate.PositionMiddle)
	fmt.Printf("%s : %d characters", truncated, utf8.RuneCountInString(truncated))
	// Output: Tzzzt : 5 characters
}

func ExampleTruncator() {
	text := "This is a long text"
	truncated := truncate.Truncator(text, 9, truncate.CutStrategy{})
	fmt.Printf("%s : %d characters", truncated, utf8.RuneCountInString(truncated))
	// Output: This is a : 9 characters
}
```
