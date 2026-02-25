package v1alpha1

// Feedback contains a feedback entry.
type Feedback struct {
	// Compiler defines the default compiler to use when evaluating expressions.
	// +optional
	Compiler *Compiler `json:"compiler,omitempty"`

	// Name is the feedback entry name.
	Name string `json:"name"`

	// Value is the feedback entry value (a JMESPath expression).
	// +optional
	Value *Any `json:"value,omitempty"`
}
