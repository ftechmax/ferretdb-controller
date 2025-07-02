# FerretDB Controller

The FerretDB Controller is a Kubernetes operator that manages users and databases in FerretDB 2.0+ instances. It introduces the `FerretDbUser` Custom Resource Definition (CRD), allowing you to declaratively create, edit, and remove users and databases in FerretDB via Kubernetes resources.

## Features

- Create, update, and delete users in FerretDB.
- Create and remove databases in FerretDB.
- Declarative management using the `FerretDbUser` CRD.

## Prerequisites

- Kubernetes cluster (v1.20+ recommended)
- FerretDB 2.0 or newer deployed and accessible
- The `ferretdb-postgres` Kubernetes Secret must exist in the same namespace as the controller. This secret should contain the credentials for FerretDB’s PostgreSQL backend.

## Installation

1. **Deploy the controller:**

```powershell
kubectl apply -k k8s/overlays/local
```

2. **Ensure the `ferretdb-postgres` secret exists:**

```powershell
kubectl get secret ferretdb-postgres
```

If it does not exist, then first deploy the FerretDB PostgreSQL backend and note the secret used there.

## Example FerretDbUser CRD

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: appdb-user-secret
  namespace: myapp
stringData:
  database_username: alice
  database_password: s3cr3t
---
apiVersion: k8s.ftechmax.net/v1alpha1
kind: FerretDbUser
metadata:
  name: appdb-user
  namespace: myapp
spec:
  database: appdb
  secret: appdb-user-secret
  roles:
    - readWrite
    - dbAdmin
```

### Outcome

- A new database named `appdb` will be created in FerretDB (if it does not already exist).
- A user `alice` will be created with the password `s3cr3t`.
- If the CRD is updated, the controller will update the user’s password and roles accordingly.
- If the CRD is deleted, the user and (optionally) the database will be removed from FerretDB.

## CRD Reference

The `FerretDbUser` CRD has the following spec fields:

- `database`: The name of the database to create or manage
- `secret`: The name of the Kubernetes Secret containing the user's credentials
- `roles`: A list of roles to assign to the user (e.g., `readWrite`, `dbAdmin`)
- `usernameKey`: The key in the secret that contains the username (default: `database_username`)
- `passwordKey`: The key in the secret that contains the password (default: `database_password`)
