package db

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

// createPostgresUser connects to PostgreSQL and creates the user and database
func CreatePostgresUser(username, password, database string) error {
	connStr, err := getConnectionString()
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(context.Background(), connStr)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	// Check if user exists
	var userExists bool
	err = conn.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", username).Scan(&userExists)
	if err != nil {
		return err
	}

	if !userExists {
		_, err = conn.Exec(context.Background(), fmt.Sprintf("CREATE USER \"%s\" WITH PASSWORD '%s';", username, password))
		if err != nil {
			return err
		}

		log.Printf("User %s created.\n", username)	
	}

	// Check if database exists
	var dbExists bool
	err = conn.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", database).Scan(&dbExists)
	if err != nil {
		return err
	}

	if !dbExists {
		_, err = conn.Exec(context.Background(), fmt.Sprintf("CREATE DATABASE \"%s\" OWNER \"%s\";", database, username))
		if err != nil {
			return err
		}

		log.Printf("Database %s created with owner %s.\n", database, username)
	}

	return nil
}

func UpdatePostgresUser(oldUsername, newUsername, oldPassword, newPassword, oldDatabase, newDatabase string) error {
	connStr, err := getConnectionString()
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(context.Background(), connStr)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	// Only update if username or password changed
	if oldUsername != newUsername || oldPassword != newPassword {
		_, err = conn.Exec(context.Background(), fmt.Sprintf("ALTER USER \"%s\" WITH PASSWORD '%s';", newUsername, newPassword))
		if err != nil {
			return err
		}

		log.Printf("User %s updated.\n", newUsername)
	}

	// Only update if database name or owner changed
	if oldDatabase != newDatabase || oldUsername != newUsername {
		_, err = conn.Exec(context.Background(), fmt.Sprintf("ALTER DATABASE \"%s\" OWNER TO \"%s\";", newDatabase, newUsername))
		if err != nil {
			return err
		}

		log.Printf("Database %s updated with owner %s.\n", newDatabase, newUsername)
	}

	return nil
}

// deletePostgresUser connects to PostgreSQL and deletes the user and database
func DeletePostgresUser(username, database string) error {
	connStr, err := getConnectionString()
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(context.Background(), connStr)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	// Terminate connections to the database before dropping it
	_, _ = conn.Exec(context.Background(), fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", database))

	// Drop database
	_, err = conn.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\";", database))
	if err != nil {
		return err
	}

	log.Printf("Database %s dropped.\n", database)

	// Drop user
	_, err = conn.Exec(context.Background(), fmt.Sprintf("DROP USER IF EXISTS \"%s\";", username))
	if err != nil {
		return err
	}

	log.Printf("User %s dropped.\n", username)

	return nil
}

func getConnectionString() (string, error) {
	pgHost := os.Getenv("POSTGRES_HOST")
	pgPort := os.Getenv("POSTGRES_PORT")
	pgAdmin := os.Getenv("POSTGRES_USER")
	pgAdminPass := os.Getenv("POSTGRES_PASSWORD")
	pdDatabase := os.Getenv("POSTGRES_DATABASE")

	if pgHost == "" || pgPort == "" || pgAdmin == "" || pgAdminPass == "" {
		return "", fmt.Errorf("PostgreSQL connection info missing in env vars")
	}

	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", pgHost, pgPort, pgAdmin, pgAdminPass, pdDatabase), nil
}

// isDuplicateUserError checks if error is 'user already exists'
func isDuplicateUserError(err error) bool {
	return err != nil && (err.Error() == "ERROR: role \"user\" already exists (SQLSTATE 42710)")
}

// isDuplicateDatabaseError checks if error is 'database already exists'
func isDuplicateDatabaseError(err error) bool {
	return err != nil && (err.Error() == "ERROR: database \"db\" already exists (SQLSTATE 42P04)")
}