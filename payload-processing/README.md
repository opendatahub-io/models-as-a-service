# AI Gateway Payload Processing

This directory contains Payload Processing plugins that will be connected to an AI Gateway via a pluggable BBR (Body Based Routing) framework developed as part of the [Kubernetes Inference Gateway](https://github.com/kubernetes-sigs/gateway-api-inference-extension).

BBR plugins enable custom request/response mutations of both headers and body, allowing advanced capabilities such as promoting the model from a field in the body to a header and routing to a selected endpoint accordingly.
