package store

import "errors"

var (
	// ErrVehicleNotFound is returned when a vehicle lookup finds no matching row.
	ErrVehicleNotFound = errors.New("vehicle not found")

	// ErrDriveNotFound is returned when a drive lookup finds no matching row.
	ErrDriveNotFound = errors.New("drive not found")

	// ErrDatabaseClosed is returned when an operation is attempted on a
	// closed database connection pool.
	ErrDatabaseClosed = errors.New("database connection closed")
)
