# MaaS Installation Overview

ODH's _Models-as-a-Service_ is compatible with the Open Data Hub project (ODH) and
Red Hat OpenShift AI (RHOAI). MaaS is installed by enabling it in the DataScienceCluster resource:

* [Install your platform](platform-setup.md) (ODH or RHOAI) with MaaS enabled in the DataScienceCluster
  (the operator will automatically deploy the MaaS API, MaaS API AuthPolicy, and NetworkPolicy).
* [Install Gateway and policies manually](maas-setup.md) (Gateway, Gateway AuthPolicy, TokenRateLimitPolicy, and RateLimitPolicy).

MaaS inherits the platform requirement for a Red Hat OpenShift cluster version 4.19.9 or
later, which is the version that has formal support for Gateway API. For earlier OpenShift
versions, there are alternatives (e.g. see a [guide here](https://github.com/opendatahub-io/kserve/tree/release-v0.15/docs/samples/llmisvc/ocp-4-18-setup)),
but we provide no support for such setups.

## Database Prerequisite

**IMPORTANT:** MaaS requires a PostgreSQL database for API key management. You **must** create a Secret with the database connection URL **before** enabling modelsAsService in your DataScienceCluster.

### Database Options

Choose one of the following PostgreSQL solutions for **production**:

- **AWS RDS for PostgreSQL** (recommended for AWS deployments)
- **Azure Database for PostgreSQL** (recommended for Azure deployments)
- **Crunchy PostgreSQL Operator** (recommended for on-premises OpenShift)
- **Self-managed PostgreSQL cluster** with backups and high availability

!!! note "Development"
    For **development**, the `scripts/deploy.sh` script creates a PostgreSQL instance and Secret automatically.

### Required Secret

Create the `maas-db-config` Secret in your ODH/RHOAI namespace (typically `opendatahub`):

```bash
kubectl create secret generic maas-db-config \
  -n opendatahub \
  --from-literal=DB_CONNECTION_URL='postgresql://username:password@hostname:5432/database?sslmode=require'
```

**Connection String Format:**
```
postgresql://USERNAME:PASSWORD@HOSTNAME:PORT/DATABASE?sslmode=require
```

**Example for AWS RDS:**
```bash
kubectl create secret generic maas-db-config \
  -n opendatahub \
  --from-literal=DB_CONNECTION_URL='postgresql://maasuser:mypassword@mydb.abc123.us-east-1.rds.amazonaws.com:5432/maas?sslmode=require'
```

!!! warning "Deployment Order"
    The maas-api will fail to start if the Secret is missing. Create the Secret **before** enabling modelsAsService in your DataScienceCluster.


## Requirements for Open Data Hub project

MaaS requires Open Data Hub version 3.0 or later, with the Model Serving component
enabled (KServe) and properly configured for deploying models with `LLMInferenceService`
resources.

A specific requirement for MaaS is to set up ODH's Model Serving with Kuadrant v1.3+, even
though ODH can work with earlier Kuadrant versions.

## Requirements for Red Hat OpenShift AI

MaaS requires Red Hat OpenShift AI (RHOAI) version 3.0 or later, with the Model Serving
component enabled (KServe) and properly configured for deploying models with
`LLMInferenceService` resources.

A specific requirement for MaaS is to set up RHOAI Model Serving with Red Hat Connectivity
Link (RHCL) v1.2+, even though RHOAI can work with earlier RHCL versions.
