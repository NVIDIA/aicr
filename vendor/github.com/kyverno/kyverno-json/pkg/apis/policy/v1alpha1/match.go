package v1alpha1

// Match defines collections of assertion trees.
type Match struct {
	// Compiler defines the default compiler to use when evaluating expressions.
	// +optional
	Compiler *Compiler `json:"compiler,omitempty"`

	// Any allows specifying assertion trees which will be ORed.
	// +optional
	Any []AssertionTree `json:"any,omitempty"`

	// All allows specifying assertion trees which will be ANDed.
	// +optional
	All []AssertionTree `json:"all,omitempty"`
}
