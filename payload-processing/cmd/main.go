/*
Copyright 2025.

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

/**
 * This file is adapted from Gateway API Inference Extension
 * Original source: https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/cmd/bbr/main.go
 * Licensed under the Apache License, Version 2.0
 */

package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/gateway-api-inference-extension/cmd/bbr/runner"
)

func main() {
	// Register ai-gateway payload processing plugins with pluggable bbr
	registerPlugins()

	if err := runner.NewRunner().
		// WithCustomCollectors(...). // THIS should be used for custom metrics exposed by our plugins
		Run(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}

func registerPlugins() {
	// TODO uncomment after all code moves
	// framework.Register(provider_resolver.ModelProviderResolverPluginType, provider_resolver.ModelProviderResolverFactory)
	// framework.Register(api_translation.APITranslationPluginType, api_translation.APITranslationFactory)
	// framework.Register(apikey_injection.APIKeyInjectionPluginType, apikey_injection.APIKeyInjectionFactory)
}
