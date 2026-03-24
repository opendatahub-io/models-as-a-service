/*
Copyright 2026.

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

package plugins

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/bbr/framework"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/common/observability/logging"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/framework/interface/plugin"
)

const (
	ExamplePluginType = "example-plugin"
)

// compile-time type validation
var _ framework.RequestProcessor = &ExamplePlugin{}

// ExamplePluginFactory defines the factory function for ExamplePlugin.
func ExamplePluginFactory(name string, _ json.RawMessage, _ framework.Handle) (framework.BBRPlugin, error) {
	plugin, err := NewExamplePlugin()
	if err != nil {
		return nil, fmt.Errorf("failed to create '%s' plugin - %w", ExamplePluginType, err)
	}

	return plugin.WithName(name), nil
}

// ExamplePlugin initializes a new ExamplePlugin and returns its pointer.
func NewExamplePlugin() (*ExamplePlugin, error) {
	return &ExamplePlugin{
		typedName: plugin.TypedName{
			Type: ExamplePluginType,
			Name: ExamplePluginType,
		},
	}, nil
}

// ExamplePlugin shows an example of how to implement request/response payload processor plugin
type ExamplePlugin struct {
	typedName plugin.TypedName
}

// TypedName returns the type and name tuple of this plugin instance.
func (p *ExamplePlugin) TypedName() plugin.TypedName {
	return p.typedName
}

// WithName sets the name of the plugin instance.
func (p *ExamplePlugin) WithName(name string) *ExamplePlugin {
	p.typedName.Name = name
	return p
}

// ProcessRequest extracts value from a given body field and sets it as HTTP header.
func (p *ExamplePlugin) ProcessRequest(ctx context.Context, cycleState *framework.CycleState, request *framework.InferenceRequest) error {
	if request == nil || request.Headers == nil || request.Body == nil {
		return fmt.Errorf("invalid inference request: request/headers/body must be non-nil") // this shouldn't happen
	}

	// extract value from headers
	exampleHeaderKey := "example-header"
	headerValue := request.Headers[exampleHeaderKey]
	log.FromContext(ctx).V(logutil.VERBOSE).Info("parsed header", "key", exampleHeaderKey, "value", headerValue)

	// extract value from body
	exampleBodyFieldKey := "example-body-field"
	rawBodyFieldValue, exists := request.Body[exampleBodyFieldKey]
	if !exists {
		return fmt.Errorf("field '%s' not found in request body", exampleBodyFieldKey)
	}

	fieldStr := fmt.Sprintf("%v", rawBodyFieldValue) // convert any type to string
	if fieldStr == "" {
		return fmt.Errorf("field '%s' is empty and couldn't be processed", exampleBodyFieldKey)
	}

	log.FromContext(ctx).V(logutil.VERBOSE).Info("parsed field from body", "field", exampleBodyFieldKey, "value", fieldStr)

	// mutations
	// for mutating headers, use request.SetHeader/RemoveHeader
	// request.SetHeader(exampleHeaderKey, headerValue+headerValue) // example for mutation an existing header
	// request.SetHeader(exampleHeaderKey+"-new", headerValue)      // example for adding a new header
	// request.RemoveHeader("header-to-remove")                     // example for removing a header.

	// for mutation request body, use the appropriate Set/Remove functions.
	// request.SetBody(map[string]any{"new": "body"})               // use SetBody to override the whole body at once. this is useful in specific use cases.
	// request.SetBodyField(exampleBodyFieldKey, fieldStr+fieldStr) // use SetBodyField to mutate a specific body field.
	// request.RemoveBodyField("field-to-remove")                   // use RemoveBodyField to remove a specific body field.

	// CycleState is a shared object between all plugins that can be used to share a per-request information between plugins
	// without thrashing the request itself (not update the request headers for example).
	// in some use cases there is a need to communicate information between plugins or even between different extension points
	// of the same plugin (e.g., api-translation can communicate which provider was chosen from the request to the response).

	// CycleState is a per-request state shared between the plugins, on all hook points
	cycleState.Write("state-key", 24)                                   // share with other plugins/hook points, can add any type of value
	_, err := framework.ReadCycleStateKey[int](cycleState, "state-key") // use this function with given T to get autoconversion by the framework
	if err != nil {
		return fmt.Errorf("couldn't read value from cycle state - %w", err)
	}
	cycleState.Delete("state-key") // delete a state. NO need to clean cycle state at the end of request handling.

	return nil
}
