package tenantreconcile

// Model identity expressions are shared by authorization, rate limiting, and
// telemetry. Subscription selection resolves aliases to a MaaSModelRef key;
// requests without that result retain the gateway's path/header identity.
const (
	CELPathParts                  = `request.path.split("/").filter(x, x != "")`
	CELPathModelIdentityAvailable = `size(` + CELPathParts + `) >= 2 && ` +
		CELPathParts + `[0] != "v1" && ` + CELPathParts + `[0] != "maas-api"`
	CELModelIdentityAvailable = `(` + CELPathModelIdentityAvailable + ` || "x-gateway-model-name" in request.headers)`
	CELModelIdentity          = `(` + CELPathModelIdentityAvailable + ` ? ` + CELPathParts + `[0] + "/" + ` + CELPathParts + `[1]` +
		` : ("x-gateway-model-name" in request.headers ? request.headers["x-gateway-model-name"] : ""))`
	// Authorino evaluates this before exporting its response metadata.
	CELAuthResolvedModelIdentity = `(has(auth.metadata) && "subscription-info" in auth.metadata && ` +
		`has(auth.metadata["subscription-info"].resolvedModel) && auth.metadata["subscription-info"].resolvedModel != ""` +
		` ? auth.metadata["subscription-info"].resolvedModel : ` + CELModelIdentity + `)`
	// Limitador receives exported identity metadata, not Authorino's private
	// auth.metadata binding. Keep the same canonical identity and fallback.
	CELResolvedModelIdentity = `(has(auth.identity) && has(auth.identity.subscription_info) && ` +
		`has(auth.identity.subscription_info.resolvedModel) && auth.identity.subscription_info.resolvedModel != ""` +
		` ? auth.identity.subscription_info.resolvedModel : ` + CELModelIdentity + `)`
)
