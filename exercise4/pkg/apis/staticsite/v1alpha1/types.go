package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:subresource:status
type StaticSite struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StaticSiteSpec   `json:"spec,omitempty"`
	Status StaticSiteStatus `json:"status,omitempty"`
}

type StaticSiteSpec struct {
	Image string `json:"image"`

	// Replicas 允许为空，控制器会在为空时回退到默认值 1。
	Replicas *int32 `json:"replicas,omitempty"`

	// ImagePullPolicy 允许为空，控制器会在为空时回退到 IfNotPresent。
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
}

type StaticSiteStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	AvailableReplicas  int32 `json:"availableReplicas,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type StaticSiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StaticSite `json:"items"`
}
