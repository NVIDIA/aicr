package v1alpha1

// Compiler defines the compiler to use when evaluating expressions.
// +kubebuilder:validation:Enum:=jp;cel
type Compiler string

const (
	EngineJP  Compiler = "jp"
	EngineCEL Compiler = "cel"
)
