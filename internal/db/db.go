package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Host           string
	Port           string
	User           string
	Password       string
	Database       string
	SSLMode        string
	ConnectTimeout time.Duration
	AllowedRoles   map[string]struct{}
	AllowAllRoles  bool
}

type Client struct {
	pool          *pgxpool.Pool
	allowedRoles  map[string]struct{}
	allowAllRoles bool
}

const managedByTag = "managed-by=ferretdb-controller"

type Ownership struct {
	Namespace string
	Name      string
	UID       string
}

type NotManagedError struct {
	Resource string
	Name     string
}

func (e *NotManagedError) Error() string {
	return fmt.Sprintf("%s %q is not managed by this controller", e.Resource, e.Name)
}

func IsNotManaged(err error) bool {
	var nm *NotManagedError
	return errors.As(err, &nm)
}

func LoadConfigFromEnv() (Config, error) {
	allowedRoles, allowAllRoles := parseAllowedRoles(os.Getenv("POSTGRES_ALLOWED_ROLES"))
	config := Config{
		Host:           strings.TrimSpace(os.Getenv("POSTGRES_HOST")),
		Port:           strings.TrimSpace(os.Getenv("POSTGRES_PORT")),
		User:           strings.TrimSpace(os.Getenv("POSTGRES_USER")),
		Password:       os.Getenv("POSTGRES_PASSWORD"),
		Database:       strings.TrimSpace(os.Getenv("POSTGRES_DATABASE")),
		SSLMode:        strings.TrimSpace(os.Getenv("POSTGRES_SSLMODE")),
		ConnectTimeout: 5 * time.Second,
		AllowedRoles:   allowedRoles,
		AllowAllRoles:  allowAllRoles,
	}

	if config.Host == "" || config.Port == "" || config.User == "" || config.Password == "" {
		return Config{}, errors.New("PostgreSQL connection info missing in env vars")
	}

	if config.Database == "" {
		config.Database = "postgres"
	}

	if config.SSLMode == "" {
		config.SSLMode = "disable"
	}

	return config, nil
}

func NewClient(ctx context.Context, config Config) (*Client, error) {
	connString := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s connect_timeout=%d",
		config.Host,
		config.Port,
		config.User,
		config.Password,
		config.Database,
		config.SSLMode,
		int(config.ConnectTimeout.Seconds()),
	)

	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, err
	}

	return &Client{
		pool:          pool,
		allowedRoles:  config.AllowedRoles,
		allowAllRoles: config.AllowAllRoles,
	}, nil
}

func (c *Client) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

func (c *Client) CreateUserAndDatabase(ctx context.Context, owner Ownership, username, password, database string, roles []string) error {
	if err := validateOwnership(owner); err != nil {
		return err
	}
	if err := c.assertDatabaseManaged(ctx, owner, database); err != nil {
		return err
	}
	if err := c.CreateUser(ctx, owner, username, password, roles); err != nil {
		return err
	}
	return c.CreateDatabase(ctx, owner, username, database)
}

func (c *Client) CreateUser(ctx context.Context, owner Ownership, username, password string, roles []string) error {
	if err := validateOwnership(owner); err != nil {
		return err
	}
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.createUserOnce(ctx, owner, username, password, roles)
		if err == nil {
			return nil
		}
		if !isRetryableCatalogError(err) || attempt == maxAttempts {
			return err
		}
		backoff := time.Duration(attempt*attempt) * 150 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			continue
		}
	}
	return nil
}

func (c *Client) createUserOnce(ctx context.Context, owner Ownership, username, password string, roles []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	roleInfo, userExists, err := c.getRoleInfo(ctx, username)
	if err != nil {
		return err
	}

	userIdent := pgx.Identifier{username}.Sanitize()
	if userExists {
		if !isOwnedBy(roleInfo.Comment, owner) {
			return &NotManagedError{Resource: "role", Name: username}
		}
		if _, err := c.pool.Exec(ctx, fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s", userIdent, quoteSQLLiteral(password))); err != nil {
			return err
		}
	} else {
		if _, err := c.pool.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s", userIdent, quoteSQLLiteral(password))); err != nil {
			return err
		}
		if err := c.setRoleComment(ctx, username, owner); err != nil {
			return err
		}
		log.Printf("Created PostgreSQL role %s", username)
	}

	return nil
}

func (c *Client) getRoleMemberships(ctx context.Context, username string) ([]string, error) {
	rows, err := c.pool.Query(ctx, "SELECT r.rolname FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid JOIN pg_roles u ON u.oid = m.member WHERE u.rolname = $1", username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roles := []string{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return roles, nil
}

func (c *Client) syncRoleGrants(ctx context.Context, username string, desiredRoles []string) error {
	currentRoles, err := c.getRoleMemberships(ctx, username)
	if err != nil {
		return err
	}

	desired := make(map[string]string, len(desiredRoles))
	for _, role := range desiredRoles {
		roleKey := strings.ToLower(role)
		if _, ok := desired[roleKey]; ok {
			continue
		}
		desired[roleKey] = role
	}

	current := make(map[string]string, len(currentRoles))
	for _, role := range currentRoles {
		roleKey := strings.ToLower(role)
		if _, ok := current[roleKey]; ok {
			continue
		}
		current[roleKey] = role
	}

	userIdent := pgx.Identifier{username}.Sanitize()
	for roleKey, role := range current {
		if _, ok := desired[roleKey]; ok {
			continue
		}
		if !c.isRoleAllowed(roleKey) {
			continue
		}
		roleIdent := pgx.Identifier{role}.Sanitize()
		if _, err := c.pool.Exec(ctx, fmt.Sprintf("REVOKE %s FROM %s", roleIdent, userIdent)); err != nil {
			return err
		}
	}

	for roleKey, role := range desired {
		if _, ok := current[roleKey]; ok {
			continue
		}
		var roleExists bool
		if err := c.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", role).Scan(&roleExists); err != nil {
			return err
		}
		if !roleExists {
			continue
		}
		roleIdent := pgx.Identifier{role}.Sanitize()
		if _, err := c.pool.Exec(ctx, fmt.Sprintf("GRANT %s TO %s", roleIdent, userIdent)); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) CreateDatabase(ctx context.Context, owner Ownership, username, database string) error {
	if err := validateOwnership(owner); err != nil {
		return err
	}
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.createDatabaseOnce(ctx, owner, username, database)
		if err == nil {
			return nil
		}
		if !isRetryableCatalogError(err) || attempt == maxAttempts {
			return err
		}
		backoff := time.Duration(attempt*attempt) * 150 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			continue
		}
	}
	return nil
}

func (c *Client) createDatabaseOnce(ctx context.Context, owner Ownership, username, database string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	dbInfo, dbExists, err := c.getDatabaseInfo(ctx, database)
	if err != nil {
		return err
	}

	dbIdent := pgx.Identifier{database}.Sanitize()
	userIdent := pgx.Identifier{username}.Sanitize()
	if dbExists {
		if !isOwnedBy(dbInfo.Comment, owner) {
			return &NotManagedError{Resource: "database", Name: database}
		}
		if dbInfo.Owner != username {
			if _, err := c.pool.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", dbIdent, userIdent)); err != nil {
				return err
			}
		}
		return nil
	}

	if _, err := c.pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s OWNER %s", dbIdent, userIdent)); err != nil {
		return err
	}
	if err := c.setDatabaseComment(ctx, database, owner); err != nil {
		return err
	}
	log.Printf("Created PostgreSQL database %s with owner %s", database, username)
	return nil

}

func (c *Client) DeleteUserAndDatabase(ctx context.Context, owner Ownership, username, database string) error {
	if err := validateOwnership(owner); err != nil {
		return err
	}
	if err := c.assertDatabaseManaged(ctx, owner, database); err != nil {
		return err
	}
	if err := c.assertRoleManaged(ctx, owner, username); err != nil {
		return err
	}
	if err := c.DeleteDatabase(ctx, owner, database); err != nil {
		return err
	}
	return c.DeleteUser(ctx, owner, username)
}

func (c *Client) DeleteDatabase(ctx context.Context, owner Ownership, database string) error {
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.deleteDatabaseOnce(ctx, owner, database)
		if err == nil {
			return nil
		}
		if !isRetryableCatalogError(err) || attempt == maxAttempts {
			return err
		}
		backoff := time.Duration(attempt*attempt) * 150 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			continue
		}
	}
	return nil
}

func (c *Client) deleteDatabaseOnce(ctx context.Context, owner Ownership, database string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	dbInfo, dbExists, err := c.getDatabaseInfo(ctx, database)
	if err != nil {
		return err
	}
	if !dbExists {
		return nil
	}
	if !isOwnedBy(dbInfo.Comment, owner) {
		return &NotManagedError{Resource: "database", Name: database}
	}

	_, _ = c.pool.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1", database)
	dbIdent := pgx.Identifier{database}.Sanitize()
	if _, err := c.pool.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbIdent)); err != nil {
		return err
	}
	log.Printf("Deleted PostgreSQL database %s", database)

	return nil
}

func (c *Client) DeleteUser(ctx context.Context, owner Ownership, username string) error {
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := c.deleteUserOnce(ctx, owner, username)
		if err == nil {
			return nil
		}
		if !isRetryableCatalogError(err) || attempt == maxAttempts {
			return err
		}
		backoff := time.Duration(attempt*attempt) * 150 * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			continue
		}
	}
	return nil
}

func (c *Client) deleteUserOnce(ctx context.Context, owner Ownership, username string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	roleInfo, userExists, err := c.getRoleInfo(ctx, username)
	if err != nil {
		return err
	}
	if !userExists {
		return nil
	}
	if !isOwnedBy(roleInfo.Comment, owner) {
		return &NotManagedError{Resource: "role", Name: username}
	}

	userIdent := pgx.Identifier{username}.Sanitize()
	if _, err := c.pool.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", userIdent)); err != nil {
		return err
	}
	log.Printf("Deleted PostgreSQL role %s", username)

	return nil
}

func (c *Client) normalizeRoles(roles []string) ([]string, error) {
	result := make([]string, 0, len(roles))
	seen := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			continue
		}
		roleKey := strings.ToLower(role)
		if !c.isRoleAllowed(roleKey) {
			return nil, fmt.Errorf("role %q is not allowed", role)
		}
		if _, ok := seen[roleKey]; ok {
			continue
		}
		seen[roleKey] = struct{}{}
		result = append(result, role)
	}
	return result, nil
}

func (c *Client) isRoleAllowed(role string) bool {
	if c.allowAllRoles {
		return true
	}
	_, ok := c.allowedRoles[strings.ToLower(role)]
	return ok
}

func (c *Client) getRoleInfo(ctx context.Context, role string) (roleInfo, bool, error) {
	var comment pgtype.Text
	err := c.pool.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname=$1", role).Scan(&comment)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return roleInfo{}, false, nil
		}
		return roleInfo{}, false, err
	}
	return roleInfo{Comment: comment.String}, true, nil
}

func (c *Client) RoleExistsOwnedBy(ctx context.Context, owner Ownership, role string) (bool, bool, error) {
	if err := validateOwnership(owner); err != nil {
		return false, false, err
	}
	info, exists, err := c.getRoleInfo(ctx, role)
	if err != nil || !exists {
		return exists, false, err
	}
	return true, isOwnedBy(info.Comment, owner), nil
}

func (c *Client) RenameUser(ctx context.Context, owner Ownership, oldName, newName string) error {
	if err := validateOwnership(owner); err != nil {
		return err
	}
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" || oldName == newName {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	oldInfo, oldExists, err := c.getRoleInfo(ctx, oldName)
	if err != nil {
		return err
	}
	if !oldExists {
		return nil
	}
	if !isOwnedBy(oldInfo.Comment, owner) {
		return &NotManagedError{Resource: "role", Name: oldName}
	}

	_, newExists, err := c.getRoleInfo(ctx, newName)
	if err != nil {
		return err
	}
	if newExists {
		return fmt.Errorf("role %q already exists", newName)
	}

	oldIdent := pgx.Identifier{oldName}.Sanitize()
	newIdent := pgx.Identifier{newName}.Sanitize()
	if _, err := c.pool.Exec(ctx, fmt.Sprintf("ALTER ROLE %s RENAME TO %s", oldIdent, newIdent)); err != nil {
		return err
	}
	log.Printf("Renamed PostgreSQL role %s to %s", oldName, newName)
	return nil
}

func (c *Client) getDatabaseInfo(ctx context.Context, database string) (databaseInfo, bool, error) {
	var owner string
	var comment pgtype.Text
	err := c.pool.QueryRow(ctx, "SELECT pg_catalog.pg_get_userbyid(datdba), shobj_description(oid, 'pg_database') FROM pg_database WHERE datname=$1", database).Scan(&owner, &comment)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return databaseInfo{}, false, nil
		}
		return databaseInfo{}, false, err
	}
	return databaseInfo{Owner: owner, Comment: comment.String}, true, nil
}

func (c *Client) setRoleComment(ctx context.Context, role string, owner Ownership) error {
	tag := ownershipTag(owner)
	roleIdent := pgx.Identifier{role}.Sanitize()
	_, err := c.pool.Exec(ctx, fmt.Sprintf("COMMENT ON ROLE %s IS %s", roleIdent, quoteSQLLiteral(tag)))
	return err
}

func (c *Client) setDatabaseComment(ctx context.Context, database string, owner Ownership) error {
	tag := ownershipTag(owner)
	dbIdent := pgx.Identifier{database}.Sanitize()
	_, err := c.pool.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS %s", dbIdent, quoteSQLLiteral(tag)))
	return err
}

func (c *Client) assertRoleManaged(ctx context.Context, owner Ownership, role string) error {
	roleInfo, exists, err := c.getRoleInfo(ctx, role)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !isOwnedBy(roleInfo.Comment, owner) {
		return &NotManagedError{Resource: "role", Name: role}
	}
	return nil
}

func (c *Client) assertDatabaseManaged(ctx context.Context, owner Ownership, database string) error {
	dbInfo, exists, err := c.getDatabaseInfo(ctx, database)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !isOwnedBy(dbInfo.Comment, owner) {
		return &NotManagedError{Resource: "database", Name: database}
	}
	return nil
}

type roleInfo struct {
	Comment string
}

type databaseInfo struct {
	Owner   string
	Comment string
}

func ownershipTag(owner Ownership) string {
	return fmt.Sprintf("%s uid=%s ns=%s name=%s", managedByTag, owner.UID, owner.Namespace, owner.Name)
}

func isOwnedBy(comment string, owner Ownership) bool {
	return strings.TrimSpace(comment) == ownershipTag(owner)
}

func validateOwnership(owner Ownership) error {
	if strings.TrimSpace(owner.UID) == "" || strings.TrimSpace(owner.Namespace) == "" || strings.TrimSpace(owner.Name) == "" {
		return errors.New("ownership metadata missing")
	}
	return nil
}

func quoteSQLLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// parseAllowedRoles parses comma-separated role names; "*" allows all roles.
func parseAllowedRoles(raw string) (map[string]struct{}, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]struct{}{}, false
	}
	parts := strings.Split(raw, ",")
	allowed := make(map[string]struct{}, len(parts))
	allowAll := false
	for _, part := range parts {
		role := strings.TrimSpace(part)
		if role == "" {
			continue
		}
		if role == "*" {
			allowAll = true
			continue
		}
		allowed[strings.ToLower(role)] = struct{}{}
	}
	return allowed, allowAll
}

func isRetryableCatalogError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "XX000" && strings.Contains(strings.ToLower(pgErr.Message), "tuple concurrently updated") {
			return true
		}
	}
	return false
}
