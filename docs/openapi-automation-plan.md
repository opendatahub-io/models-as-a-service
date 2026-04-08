# OpenAPI Validation & Automation Plan

## Current State
- **Spec Location**: `maas-api/openapi3.yaml` (933 lines)
- **Format**: OpenAPI 3.0.3
- **Maintenance**: Manual (no code generation from annotations)
- **CI Validation**: None
- **Documentation**: Rendered via mkdocs-swagger-ui-tag plugin

## Proposed Improvements

### Phase 1: Validation & Linting (High Priority)

#### 1.1 OpenAPI Spec Validation
**Goal**: Ensure spec is valid OpenAPI 3.0.3

**Tools**: 
- [Spectral](https://stoplight.io/open-source/spectral) - OpenAPI linter
- [Redocly CLI](https://redocly.com/docs/cli) - OpenAPI validator

**Implementation**:
```yaml
# .github/workflows/openapi-validation.yml
- name: Validate OpenAPI Spec
  run: |
    npm install -g @stoplight/spectral-cli
    spectral lint maas-api/openapi3.yaml --ruleset .spectral.yml
```

**Benefits**:
- Catches schema errors before merge
- Enforces consistency (naming conventions, description quality)
- Validates examples match schemas

#### 1.2 Breaking Change Detection
**Goal**: Prevent accidental API breaking changes

**Tool**: [oasdiff](https://github.com/oasdiff/oasdiff)

**Implementation**:
```yaml
- name: Check for Breaking Changes
  run: |
    curl -fsSL https://raw.githubusercontent.com/oasdiff/oasdiff/main/install.sh | sh
    oasdiff breaking origin/main:maas-api/openapi3.yaml maas-api/openapi3.yaml
```

**Benefits**:
- Catches removed endpoints, changed required fields, etc.
- Fails PR if breaking changes detected
- Forces explicit versioning decisions

### Phase 2: Contract Testing (Medium Priority)

#### 2.1 Spec-Implementation Alignment
**Goal**: Ensure API implementation matches OpenAPI spec

**Tool**: [Dredd](https://dredd.org/en/latest/) or [Prism](https://stoplight.io/open-source/prism)

**Implementation**:
```yaml
- name: Run Contract Tests
  run: |
    # Start API server
    ./bin/maas-api --config test-config.yaml &
    # Run contract tests
    npm install -g dredd
    dredd maas-api/openapi3.yaml http://localhost:8080
```

**Benefits**:
- Catches drift between spec and implementation
- Ensures examples in spec actually work
- Tests all response status codes

#### 2.2 Request/Response Validation
**Goal**: Validate real API responses against schema

**Tool**: OpenAPI middleware in Go service

**Implementation**:
```go
// Add to maas-api
import "github.com/getkin/kin-openapi/openapi3filter"

func ValidateRequest(handler http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Validate request against OpenAPI spec
        // Log warnings for mismatches
        handler.ServeHTTP(w, r)
    })
}
```

**Benefits**:
- Runtime validation in dev/test environments
- Logs when implementation diverges from spec
- Can enable in CI tests

### Phase 3: Documentation & Developer Experience (Medium Priority)

#### 3.1 Auto-generate Client SDKs
**Goal**: Provide client libraries for users

**Tool**: [openapi-generator](https://openapi-generator.tech/)

**Implementation**:
```bash
# Generate Python client
openapi-generator generate \
  -i maas-api/openapi3.yaml \
  -g python \
  -o clients/python

# Generate Go client
openapi-generator generate \
  -i maas-api/openapi3.yaml \
  -g go \
  -o clients/go
```

**Benefits**:
- Users get type-safe clients
- Reduces integration errors
- Auto-updates when spec changes

#### 3.2 Enhanced API Documentation
**Goal**: Better docs than raw swagger UI

**Tool**: [Redoc](https://redocly.com/redoc) or [Stoplight Elements](https://stoplight.io/open-source/elements)

**Implementation**:
```html
<!-- docs/api.html -->
<redoc spec-url="../openapi3.yaml"></redoc>
```

**Benefits**:
- Better UX than swagger-ui
- Supports examples, tutorials
- Can embed in existing docs

### Phase 4: Automation (Lower Priority)

#### 4.1 Auto-generate from Code Annotations
**Goal**: Generate spec from Go code

**Tool**: [swaggo/swag](https://github.com/swaggo/swag)

**Trade-offs**:
- Pro: Single source of truth (code)
- Pro: Can't drift from implementation
- Con: Requires refactoring all handlers
- Con: Annotations clutter code
- **Decision**: Defer until spec stabilizes

#### 4.2 Mock Server for Development
**Goal**: Frontend can develop against spec before backend ready

**Tool**: [Prism](https://stoplight.io/open-source/prism)

**Implementation**:
```bash
# Run mock server
prism mock maas-api/openapi3.yaml
# Returns example responses from spec
```

**Benefits**:
- Parallel frontend/backend development
- Can test edge cases
- Useful for demos

## Recommended Implementation Order

1. **Week 1**: Spectral validation in CI
2. **Week 2**: Breaking change detection
3. **Week 3**: Contract testing (dredd)
4. **Week 4**: Client SDK generation (Python)

## Success Metrics

- Zero spec validation errors
- No breaking changes merged without approval
- 100% endpoint coverage in contract tests
- Client SDK published to PyPI

## Open Questions

1. Should we version the API (v1, v2)?
2. Who owns spec updates (backend team only or shared)?
3. Should we enforce spec-first development (spec then code)?
4. Do we want runtime validation in production (performance impact)?
