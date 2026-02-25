package v1alpha1

// ValidatingRule defines a validating rule.
type ValidatingRule struct {
	// Name is a label to identify the rule, It must be unique within the policy.
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Compiler defines the default compiler to use when evaluating expressions.
	// +optional
	Compiler *Compiler `json:"compiler,omitempty"`

	// Context defines variables and data sources that can be used during rule execution.
	// +optional
	Context []ContextEntry `json:"context,omitempty"`

	// Match defines when this policy rule should be applied.
	// +optional
	Match *Match `json:"match,omitempty"`

	// Exclude defines when this policy rule should not be applied.
	// +optional
	Exclude *Match `json:"exclude,omitempty"`

	// Identifier declares a JMESPath expression to extract a name from the payload.
	// +optional
	Identifier string `json:"identifier,omitempty"`

	// Feedback declares rule feedback entries.
	// +optional
	Feedback []Feedback `json:"feedback,omitempty"`

	// Assert is used to validate matching resources.
	Assert Assert `json:"assert"`
}
