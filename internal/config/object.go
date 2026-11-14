package config

// DecodeObject decodes the JSON of one API object into a T, which is v1alpha1.AgentBackend, v1alpha1.ChatConnection or
// v1alpha1.Binding, then Default and Validate, the way Load decodes a document of a file, with one difference: it does not
// refuse a field that T does not have. It returns the object and the path of each such field ("spec.chat.futureField",
// in document order, no value), or no object and every problem found, each as "path: message".
//
// Unknown fields are tolerated because on the custom resource route the API server's schema is the arbiter of what an
// object may hold, and a broker is routinely older than the CRDs of its cluster: a later release adds a field, the API
// server defaults it onto every stored object without a change of generation, and a broker that refused the field would
// serve none of them. The caller says what it ignored (the CR source logs it). The file route keeps the refusal: see Load
// and DecodeStrict.
//
// A Binding may carry a status, which an API server's object does, and which Load refuses in a file. DecodeObject does
// not check apiVersion or kind, and it reads no metadata but what T holds; the caller has chosen T by the resource it read
// the object from.
func DecodeObject[T any, PT interface {
	*T
	Default()
	Validate() error
}](j []byte) (obj *T, unknown []string, errs []error) {
	obj = new(T)
	unknown, other, err := scanDecode(j, obj)
	if err != nil {
		return nil, nil, []error{err}
	}
	errs = other
	PT(obj).Default()
	if err := PT(obj).Validate(); err != nil {
		errs = append(errs, flatten(err)...)
	}
	if len(errs) > 0 {
		return nil, unknown, errs
	}
	return obj, unknown, nil
}

// The kinds of the three resources, as Invalid and a Rejection's reports name them.
const (
	KindAgentBackend   = "AgentBackend"
	KindChatConnection = "ChatConnection"
	KindBinding        = "Binding"
)
