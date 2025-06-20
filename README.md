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
apiVersion: ferretdb.io/v1alpha1
kind: FerretDbUser
metadata:
  name: alice
spec:
  database: appdb
  username: alice
  password: s3cr3t
  roles:
    - readWrite
    - dbAdmin
```

### Outcome

- A new database named `appdb` will be created in FerretDB (if it does not already exist).
- A user `alice` will be created with the password `s3cr3t` and assigned the `readWrite` and `dbAdmin` roles on the `appdb` database.
- If the CRD is updated, the controller will update the user’s password and roles accordingly.
- If the CRD is deleted, the user and (optionally) the database will be removed from FerretDB.
