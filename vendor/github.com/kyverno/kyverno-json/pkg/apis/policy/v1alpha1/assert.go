package v1alpha1

// Assert defines collections of assertions.
type Assert struct {
	// Compiler defines the default compiler to use when evaluating expressions.
	// +optional
	Compiler *Compiler `json:"compiler,omitempty"`

	// Any allows specifying assertions which will be ORed.
	// +optional
	Any []Assertion `json:"any,omitempty"`

	// All allows specifying assertions which will be ANDed.
	// +optional
	All []Assertion `json:"all,omitempty"`
}
