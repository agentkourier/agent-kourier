// Package fixture is a small API in the shape of api/v1alpha1, for the generator's golden test.
package fixture

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Color is a named string, listed by its constants when it keys a map.
type Color string

// The colors.
const (
	ColorRed  Color = "red"
	ColorBlue Color = "blue"
)

// Widget is a thing with <angle brackets> and a | pipe in its doc.
//
// +kubebuilder:object:root=true
type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   WidgetSpec   `json:"spec"`
	Status WidgetStatus `json:"status,omitzero"`
}

// WidgetList is skipped: a list is no resource of its own.
//
// +kubebuilder:object:root=true
type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	Items           []Widget `json:"items"`
}

// WidgetSpec is the desired state of a Widget.
type WidgetSpec struct {
	// Name is required and *short*.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Mode defaults to fast.
	// +kubebuilder:validation:Enum=fast;slow
	// +kubebuilder:default=fast
	// +optional
	Mode string `json:"mode"`
	// Parts are the widget's parts.
	Parts []Part `json:"parts,omitempty"`
	// Ref points elsewhere.
	Ref *Ref `json:"ref,omitempty"`
	// Paint maps a color to a shade.
	Paint map[Color]string `json:"paint,omitempty"`
	// Every is a duration.
	Every metav1.Duration `json:"every,omitzero"`
	// +kubebuilder:validation:Minimum=0
	Count   int32 `json:"count,omitempty"`
	Enabled bool  `json:"enabled,omitempty"`
	hidden  string
}

// Part is one part.
type Part struct {
	// ID matches a pattern.
	// +kubebuilder:validation:Pattern=`^P[0-9]+$`
	ID  string `json:"id"`
	Ref Ref    `json:"ref"`
}

// Ref names a thing.
type Ref struct {
	Name string `json:"name"`
}

// WidgetStatus is the observed state.
type WidgetStatus struct {
	// Conditions say how it is.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
