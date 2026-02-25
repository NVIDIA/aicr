package reflect

import (
	"reflect"
)

func GetKind(value any) reflect.Kind {
	if value == nil {
		return reflect.Invalid
	}
	return reflect.TypeOf(value).Kind()
}
