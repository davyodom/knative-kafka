/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	duckv1alpha1 "knative.dev/pkg/apis/duck/v1alpha1"
	"knative.dev/pkg/kmeta"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// SinkBinding describes a Binding that is also a Source.
// The `sink` (from the Source duck) is resolved to a URL and
// then projected into the `subject` by augmenting the runtime
// contract of the referenced containers to have a `K_SINK`
// environment variable holding the endpoint to which to send
// cloud events.
type SinkBinding struct {
	// Deprecated allows ApiServerSource to have a deprecated message.
	Deprecated

	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SinkBindingSpec   `json:"spec"`
	Status SinkBindingStatus `json:"status"`
}

// Check the interfaces that SinkBinding should be implementing.
var (
	_ runtime.Object     = (*SinkBinding)(nil)
	_ kmeta.OwnerRefable = (*SinkBinding)(nil)
	_ apis.Validatable   = (*SinkBinding)(nil)
	_ apis.Defaultable   = (*SinkBinding)(nil)
	_ apis.HasSpec       = (*SinkBinding)(nil)
)

// SinkBindingSpec holds the desired state of the SinkBinding (from the client).
type SinkBindingSpec struct {
	duckv1.SourceSpec        `json:",inline"`
	duckv1alpha1.BindingSpec `json:",inline"`
}

const (
	// SinkBindingConditionReady is configured to indicate whether the Binding
	// has been configured for resources subject to its runtime contract.
	SinkBindingConditionReady = apis.ConditionReady
)

// SinkBindingStatus communicates the observed state of the SinkBinding (from the controller).
type SinkBindingStatus struct {
	duckv1.SourceStatus `json:",inline"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SinkBindingList contains a list of SinkBinding
type SinkBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SinkBinding `json:"items"`
}