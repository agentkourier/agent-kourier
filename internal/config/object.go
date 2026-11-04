package config

// DecodeObject decodes the JSON of one API object into a T, which is v1alpha1.AgentBackend, v1alpha1.ChatConnection or
// v1alpha1.Binding, the way Load decodes a document of a file: strictly (a field the type does not have is a problem),
// then Default and Validate. It returns the object, or no object and every problem found, each as "path: message".
//
// It is the one decoder of the two routes, so a custom resource and a file go through the same checks. It differs from
// Load in one respect only: a Binding may carry a status, which an API server's object does, and which Load refuses in a
// file. It does not check apiVersion or kind, and it reads no metadata but what T holds; the caller has chosen T by the
// resource it read the object from.
func DecodeObject[T any, PT interface {
	*T
	Default()
	Validate() error
}](j []byte) (*T, []error) {
	var dst []T
	if errs := decode[T, PT](j, &dst); len(errs) > 0 || len(dst) == 0 {
		return nil, errs
	}
	return &dst[0], nil
}

// The kinds of the three resources, as Invalid and a Rejection's reports name them.
const (
	KindAgentBackend   = "AgentBackend"
	KindChatConnection = "ChatConnection"
	KindBinding        = "Binding"
)
