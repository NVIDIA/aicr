package v1alpha1

// Assertion contains an assertion tree associated with a message.
type Assertion struct {
	// Compiler defines the default compiler to use when evaluating expressions.
	// +optional
	Compiler *Compiler `json:"compiler,omitempty"`

	// Message is the message associated message.
	// +optional
	Message *Message `json:"message,omitempty"`

	// Check is the assertion check definition.
	Check AssertionTree `json:"check"`
}
