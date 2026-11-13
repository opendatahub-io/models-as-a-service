/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package inferencepaths lists OpenAI-style inference path suffixes used for
// gateway observability and interim request-count rate limiting.
package inferencepaths

// RequestCountPathSuffix is the JSON BBR path where TokenRateLimitPolicy cannot
// extract usage.total_tokens; per-subscription request budgets apply here.
const RequestCountPathSuffix = "/v1/rerank"

// UsageLogPathSuffixes are POST inference paths emitted to the OTEL usage log
// (suffix match on :path). Keep deployment/envoy-otel-access-log.yaml in sync.
var UsageLogPathSuffixes = []string{
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/embeddings",
	RequestCountPathSuffix,
}
