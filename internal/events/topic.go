package events

// Topic identifies an event channel. Subscribers filter by topic.
type Topic string

const (
	// TopicVehicleTelemetry is published when a batch of telemetry fields
	// arrives from a vehicle. The payload is VehicleTelemetryEvent.
	TopicVehicleTelemetry Topic = "vehicle.telemetry"

	// TopicConnectivity is published when a vehicle connects or disconnects
	// from the telemetry server. The payload is ConnectivityEvent.
	TopicConnectivity Topic = "vehicle.connectivity"

	// TopicDriveStarted is published when the drive detector identifies
	// that a vehicle has begun a drive. The payload is DriveStartedEvent.
	TopicDriveStarted Topic = "drive.started"

	// TopicDriveUpdated is published for each route point accumulated
	// during an active drive. The payload is DriveUpdatedEvent.
	TopicDriveUpdated Topic = "drive.updated"

	// TopicDriveEnded is published when the drive detector identifies
	// that a vehicle has completed a drive. The payload is DriveEndedEvent.
	TopicDriveEnded Topic = "drive.ended"
)
