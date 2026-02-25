package v1alpha1

// ValidatingPolicySpec contains the policy spec.
type ValidatingPolicySpec struct {
	// Compiler defines the default compiler to use when evaluating expressions.
	// +optional
	Compiler *Compiler `json:"compiler,omitempty"`

	// Rules is a list of ValidatingRule instances.
	Rules []ValidatingRule `json:"rules"`
}
