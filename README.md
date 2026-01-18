# FerretDB Controller

The FerretDB Controller is a Kubernetes operator that manages users and databases in FerretDB 2.0+ instances. It introduces the `FerretDbUser` Custom Resource Definition (CRD), allowing you to declaratively create, edit, and remove users and databases in FerretDB via Kubernetes resources.

## Features

- Create, update, and delete users in FerretDB.
- Create and remove databases in FerretDB.
- Declarative management using the `FerretDbUser` CRD.

## Prerequisites

- Kubernetes cluster (v1.20+ recommended)
- FerretDB 2.0 or newer deployed and accessible
- The `ferretdb-postgres` Kubernetes Secret must exist in the same namespace as the controller. This secret should contain the credentials for FerretDB's PostgreSQL backend.

## Security defaults

- The controller only manages databases and roles it created, identified by a Postgres comment tag.
- FerretDB authorization is not yet supported; `roles` are currently ignored and default to `[]`. `POSTGRES_ALLOWED_ROLES` is reserved for future use.

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
  name: appdb-user
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
  secret: appdb-user
```

### Outcome

- A new database named `appdb` will be created in FerretDB (if it does not already exist).
- A user `alice` will be created with the password `s3cr3t`.
- If the CRD is updated, the controller will update the user's password (roles are currently ignored by FerretDB).
- If the CRD is deleted, the user and (optionally) the database will be removed from FerretDB.

## CRD Reference

The `FerretDbUser` CRD has the following spec fields:

- `database`: The name of the database to create or manage
- `secret`: The name of the Kubernetes Secret containing the user's credentials (must match the `FerretDbUser` name)
- `roles`: A list of roles to assign to the user (e.g., `readWrite`, `dbAdmin`). Defaults to `[]` and is currently ignored by FerretDB.
- `usernameKey`: The key in the secret that contains the username (default: `database_username`)
- `passwordKey`: The key in the secret that contains the password (default: `database_password`)

## Development

### Debugging PostgreSQL state

Use the FerretDB Postgres pod and the admin credentials from the PostgreSQL secret.

List roles:

```powershell
kubectl exec -n ferretdb-system ferretdb-postgres-0 -- bash -c 'PGPASSWORD=admin psql -U admin -d postgres -c "\\du"'
```

List databases:

```powershell
kubectl exec -n ferretdb-system ferretdb-postgres-0 -- bash -c 'PGPASSWORD=admin psql -U admin -d postgres -c "\\l"'
```

Inspect grants on a specific database (replace `appdb`):

```powershell
kubectl exec -n ferretdb-system ferretdb-postgres-0 -- bash -c 'PGPASSWORD=admin psql -U admin -d appdb -c "\\dp"'
```

```sh
psql -U admin -d postgres -c "\\du"
psql -U admin -d postgres -c "\\l"
psql -U admin -d appdb -c "\\dp"

psql -U admin -d postgres -c "DROP DATABASE IF EXISTS appdb;"
psql -U admin -d postgres -c "DROP ROLE IF EXISTS alice;"
```

Check comments for ownership verification:

```sh
psql -U admin -d postgres -c "SELECT rolname, shobj_description(oid, 'pg_authid') AS comment FROM pg_roles WHERE rolname = 'alice';"
psql -U admin -d postgres -c "SELECT datname, shobj_description(oid, 'pg_database') AS comment FROM pg_database WHERE datname = 'appdb';"
```
